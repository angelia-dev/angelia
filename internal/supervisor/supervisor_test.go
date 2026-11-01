package supervisor

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type fakeRole struct {
	name string
	run  func(ctx context.Context) error
}

func (f fakeRole) Name() string                  { return f.name }
func (f fakeRole) Run(ctx context.Context) error { return f.run(ctx) }

func blockRole(name string) Role {
	return fakeRole{name, func(ctx context.Context) error { <-ctx.Done(); return nil }}
}

func names(rs []Role) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Name())
	}
	return out
}

func TestSelect(t *testing.T) {
	tests := []struct {
		in      []string
		want    string
		wantErr bool
	}{
		{[]string{"all"}, "api,scheduler,worker,dispatcher", false},
		{[]string{"worker", "dispatcher"}, "worker,dispatcher", false},
		{[]string{"dispatcher", "api", "api"}, "api,dispatcher", false},
		{[]string{"bogus"}, "", true},
		{[]string{"all", "api"}, "", true},
		{nil, "", true},
	}
	for _, tc := range tests {
		got, err := Select(tc.in, NewStub)
		if (err != nil) != tc.wantErr {
			t.Fatalf("%v: err=%v", tc.in, err)
		}
		if err == nil && strings.Join(names(got), ",") != tc.want {
			t.Errorf("%v: got %v want %s", tc.in, names(got), tc.want)
		}
	}
}

var client = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}

type harness struct {
	s      *Supervisor
	url    string
	cancel context.CancelFunc
	done   chan error
	logs   *safeBuf
}

func start(t *testing.T, roles []Role, ready ReadyChecker, drain time.Duration) *harness {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	logs := &safeBuf{}
	s := &Supervisor{
		Roles: roles, Listener: ln, Ready: ready, DrainTimeout: drain,
		Logger: newTestLogger(logs),
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{s: s, url: "http://" + ln.Addr().String(), cancel: cancel, done: make(chan error, 1), logs: logs}
	go func() { h.done <- s.Run(ctx) }()
	t.Cleanup(cancel)
	return h
}

func (h *harness) get(t *testing.T, path string) int {
	t.Helper()
	var last error
	for range 100 {
		resp, err := client.Get(h.url + path)
		if err == nil {
			_ = resp.Body.Close()
			return resp.StatusCode
		}
		last = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("GET %s: %v", path, last)
	return 0
}

func (h *harness) wait(t *testing.T, d time.Duration) error {
	t.Helper()
	select {
	case err := <-h.done:
		return err
	case <-time.After(d):
		t.Fatal("supervisor did not return")
		return nil
	}
}

// AC: only the selected roles start, one log line per role.
func TestOnlySelectedRolesStart(t *testing.T) {
	var started sync.Map
	mk := func(n string) Role {
		return fakeRole{n, func(ctx context.Context) error { started.Store(n, true); <-ctx.Done(); return nil }}
	}
	roles, err := Select([]string{"worker", "dispatcher"}, func(n string) Role { return mk(n) })
	if err != nil {
		t.Fatal(err)
	}
	h := start(t, roles, nil, time.Second)
	h.get(t, "/healthz")
	h.cancel()
	if err := h.wait(t, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"worker", "dispatcher"} {
		if _, ok := started.Load(n); !ok {
			t.Errorf("role %s did not start", n)
		}
		if !strings.Contains(h.logs.String(), `"role":"`+n+`"`) {
			t.Errorf("no log line for %s: %s", n, h.logs.String())
		}
	}
	for _, n := range []string{"api", "scheduler"} {
		if _, ok := started.Load(n); ok {
			t.Errorf("role %s should not start", n)
		}
		if strings.Contains(h.logs.String(), `"role":"`+n+`"`) {
			t.Errorf("unexpected log for %s", n)
		}
	}
}

// AC: SIGTERM (ctx cancel) exits cleanly within the drain timeout and gives
// in-flight work the chance to finish.
func TestGracefulShutdownFinishesInFlightWork(t *testing.T) {
	var finished atomic.Bool
	inflight := make(chan struct{})
	r := fakeRole{"worker", func(ctx context.Context) error {
		close(inflight)
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond) // finish the in-flight item
		finished.Store(true)
		return nil
	}}
	h := start(t, []Role{r}, nil, 2*time.Second)
	<-inflight
	h.cancel()
	if err := h.wait(t, 3*time.Second); err != nil {
		t.Fatalf("want clean exit, got %v", err)
	}
	if !finished.Load() {
		t.Error("in-flight work was not finished before exit")
	}
}

// AC: SIGTERM stops accepting new work: /readyz is 503 while roles drain.
func TestShutdownReportsNotReadyWhileDraining(t *testing.T) {
	release := make(chan struct{})
	r := fakeRole{"worker", func(ctx context.Context) error { <-ctx.Done(); <-release; return nil }}
	h := start(t, []Role{r}, nil, 5*time.Second)
	if c := h.get(t, "/readyz"); c != http.StatusOK {
		t.Fatalf("readyz before shutdown = %d", c)
	}
	h.cancel()
	deadline := time.Now().Add(2 * time.Second)
	for h.get(t, "/readyz") != http.StatusServiceUnavailable {
		if time.Now().After(deadline) {
			t.Fatal("readyz never became 503 during drain")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c := h.get(t, "/healthz"); c != http.StatusOK {
		t.Errorf("healthz during drain = %d", c)
	}
	close(release)
	if err := h.wait(t, 3*time.Second); err != nil {
		t.Fatal(err)
	}
}

// A role that returns while the supervisor is running is a failure.
func TestRoleExitingEarlyIsFailure(t *testing.T) {
	early := fakeRole{"scheduler", func(context.Context) error { return nil }}
	h := start(t, []Role{early, blockRole("worker")}, nil, time.Second)
	err := h.wait(t, 3*time.Second)
	if err == nil || !strings.Contains(err.Error(), "exited unexpectedly") {
		t.Fatalf("want unexpected exit error, got %v", err)
	}
}

type httpRole struct{ Role }

func (httpRole) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
}

func TestHTTPRoleMountsOnSharedMux(t *testing.T) {
	h := start(t, []Role{httpRole{blockRole("api")}}, nil, time.Second)
	if c := h.get(t, "/v1/ping"); c != http.StatusTeapot {
		t.Errorf("mounted route = %d", c)
	}
	if c := h.get(t, "/healthz"); c != http.StatusOK {
		t.Errorf("healthz = %d", c)
	}
	h.cancel()
	if err := h.wait(t, 3*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestNoRoles(t *testing.T) {
	if err := (&Supervisor{Listen: "127.0.0.1:0"}).Run(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestRoleNamesReturnsCopy(t *testing.T) {
	n := RoleNames()
	n[0] = "x"
	if RoleNames()[0] != "api" {
		t.Fatal("RoleNames exposes internal state")
	}
}

// Roles that ignore cancellation make the process fail rather than hang.
func TestDrainTimeoutExceeded(t *testing.T) {
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	r := fakeRole{"worker", func(context.Context) error { <-stuck; return nil }}
	h := start(t, []Role{r}, nil, 100*time.Millisecond)
	h.get(t, "/healthz")
	h.cancel()
	err := h.wait(t, 3*time.Second)
	if err == nil || !strings.Contains(err.Error(), "did not stop") {
		t.Fatalf("want drain timeout error, got %v", err)
	}
}

// AC: a role returning an error stops the process with a non-nil error and
// cancels the other roles.
func TestRoleErrorStopsEverything(t *testing.T) {
	boom := errors.New("boom")
	var otherStopped atomic.Bool
	bad := fakeRole{"scheduler", func(context.Context) error {
		time.Sleep(50 * time.Millisecond)
		return boom
	}}
	other := fakeRole{"worker", func(ctx context.Context) error { <-ctx.Done(); otherStopped.Store(true); return nil }}
	h := start(t, []Role{bad, other}, nil, time.Second)
	err := h.wait(t, 3*time.Second)
	if !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
	if !otherStopped.Load() {
		t.Error("other role was not cancelled")
	}
}

func TestListenError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	s := &Supervisor{Roles: []Role{blockRole("api")}, Listen: ln.Addr().String(), Logger: newTestLogger(&safeBuf{})}
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("expected listen error")
	}
}

// AC: /readyz is 503 until migrations are applied, /healthz always 200.
func TestReadiness(t *testing.T) {
	var migrated atomic.Bool
	ready := ReadyFunc(func(context.Context) error {
		if !migrated.Load() {
			return errors.New("migrations pending")
		}
		return nil
	})
	h := start(t, []Role{blockRole("api")}, ready, time.Second)
	if c := h.get(t, "/healthz"); c != http.StatusOK {
		t.Errorf("healthz = %d", c)
	}
	if c := h.get(t, "/readyz"); c != http.StatusServiceUnavailable {
		t.Errorf("readyz before migrations = %d, want 503", c)
	}
	migrated.Store(true)
	if c := h.get(t, "/readyz"); c != http.StatusOK {
		t.Errorf("readyz after migrations = %d, want 200", c)
	}
	if c := h.get(t, "/healthz"); c != http.StatusOK {
		t.Errorf("healthz = %d", c)
	}
	h.cancel()
	if err := h.wait(t, 3*time.Second); err != nil {
		t.Fatal(err)
	}
}
