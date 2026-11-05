package wasm_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/bench/plugins"
	"github.com/angelia-dev/angelia/internal/plugin/wasm"
	"github.com/angelia-dev/angelia/internal/source"
)

func TestValidate(t *testing.T) {
	_, hc := newTestNet(t, nil)
	s := newHostile(t, hostileManifest(t), wasm.Options{HTTPClient: hc})
	if err := s.Validate(hostileTarget("ok")); err != nil {
		t.Fatalf("valid target: %v", err)
	}
	if err := s.Validate(source.Target{Kind: "hostile"}); err != nil {
		t.Fatalf("nil coordinates: %v", err)
	}
	for _, tg := range []source.Target{hostileTarget("invalid"), {Kind: "crates", Coordinates: map[string]string{"mode": "ok"}}} {
		if err := s.Validate(tg); !errors.Is(err, source.ErrInvalidTarget) {
			t.Errorf("Validate(%v) = %v, want ErrInvalidTarget", tg, err)
		}
	}
	err := s.Validate(hostileTarget("validate_trap"))
	if err == nil || errors.Is(err, source.ErrInvalidTarget) {
		t.Errorf("a trapping validate = %v, want a plugin error that is not ErrInvalidTarget", err)
	}
	if err := s.Validate(hostileTarget("ok")); err != nil {
		t.Errorf("Validate after a trap: %v", err)
	}
}

func TestCallTimeoutInterruptsPlugin(t *testing.T) {
	_, hc := newTestNet(t, nil)
	s := newHostile(t, hostileManifest(t), wasm.Options{HTTPClient: hc, CallTimeout: 200 * time.Millisecond})
	start := time.Now()
	_, err := s.List(t.Context(), hostileTarget("loop"), source.FetchState{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the infinite loop ran for %v", d)
	}
	// The interrupted instance is not reused.
	if _, err := s.List(t.Context(), hostileTarget("ok"), source.FetchState{}); err != nil {
		t.Fatalf("List after a timeout: %v", err)
	}
}

func TestCancelInterruptsPlugin(t *testing.T) {
	_, hc := newTestNet(t, nil)
	s := newHostile(t, hostileManifest(t), wasm.Options{HTTPClient: hc})
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err := s.List(ctx, hostileTarget("loop"), source.FetchState{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestCancelDuringHTTP(t *testing.T) {
	started := make(chan struct{})
	_, hc := newTestNet(t, map[string]http.Handler{
		"allowed.test": http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			close(started)
			<-r.Context().Done()
		}),
	})
	s := newHostile(t, hostileManifest(t, "allowed.test"), wasm.Options{HTTPClient: hc})
	ctx, cancel := context.WithCancel(t.Context())
	go func() { <-started; cancel() }()
	_, err := s.List(ctx, hostileTarget("fetch", "url", "http://allowed.test/"), source.FetchState{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestPluginFailures(t *testing.T) {
	_, hc := newTestNet(t, nil)
	s := newHostile(t, hostileManifest(t), wasm.Options{HTTPClient: hc, MaxMemoryPages: 64})
	cases := map[string]string{
		"trap":             "unreachable", // panic = "abort" traps
		"error":            "plugin says no",
		"alloc":            "wasm error: unreachable", // the allocator aborts at MaxMemoryPages
		"bad_published_at": "published_at",
		"empty_raw":        "empty raw",
	}
	for mode, want := range cases {
		t.Run(mode, func(t *testing.T) {
			_, err := s.List(t.Context(), hostileTarget(mode), source.FetchState{})
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v, want one containing %q", err, want)
			}
		})
	}
	if _, err := s.List(t.Context(), hostileTarget("ok"), source.FetchState{}); err != nil {
		t.Fatalf("List after failures: %v", err)
	}
}

func TestOutputCap(t *testing.T) {
	_, hc := newTestNet(t, nil)
	s := newHostile(t, hostileManifest(t), wasm.Options{HTTPClient: hc, MaxOutputBytes: 10})
	_, err := s.List(t.Context(), hostileTarget("ok"), source.FetchState{})
	if err == nil || !strings.Contains(err.Error(), "exceeds 10") {
		t.Fatalf("err = %v, want the output cap", err)
	}
}

func TestHTTPRequestFeatures(t *testing.T) {
	var mu sync.Mutex
	var gotBody, gotAuth, gotMethod string
	_, hc := newTestNet(t, map[string]http.Handler{
		"allowed.test": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			gotBody, gotAuth, gotMethod = string(b), r.Header.Get("X-Trace"), r.Method
			mu.Unlock()
			w.WriteHeader(http.StatusTeapot)
			_, _ = io.WriteString(w, strings.Repeat("x", 100))
		}),
	})
	s := newHostile(t, hostileManifest(t, "allowed.test"), wasm.Options{HTTPClient: hc})

	res, err := s.List(t.Context(), hostileTarget("fetch", "url", "http://allowed.test/",
		"method", "post", "body", `{"q":1}`, "h:X-Trace", "abc"), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	method, body, auth := gotMethod, gotBody, gotAuth
	mu.Unlock()
	if res.Versions[0].Raw != "418" || method != http.MethodPost || body != `{"q":1}` || auth != "abc" {
		t.Fatalf("status %s, upstream saw %s %q X-Trace %q", res.Versions[0].Raw, method, body, auth)
	}

	small := newHostile(t, hostileManifest(t, "allowed.test"), wasm.Options{HTTPClient: hc, MaxResponseBytes: 10})
	_, err = small.List(t.Context(), hostileTarget("fetch", "url", "http://allowed.test/"), source.FetchState{})
	if err == nil || !strings.Contains(err.Error(), "too_large") {
		t.Fatalf("err = %v, want too_large", err)
	}
}

func TestTransportError(t *testing.T) {
	_, hc := newTestNet(t, nil) // no handler: the network fails
	s := newHostile(t, hostileManifest(t, "allowed.test"), wasm.Options{HTTPClient: hc})
	_, err := s.List(t.Context(), hostileTarget("fetch", "url", "http://allowed.test/"), source.FetchState{})
	if err == nil || !strings.Contains(err.Error(), "transport") {
		t.Fatalf("err = %v, want a transport error", err)
	}
	if errors.Is(err, wasm.ErrHostNotAllowed) || errors.Is(err, wasm.ErrPolicy) {
		t.Fatalf("a transport error is not a policy violation: %v", err)
	}
}

func TestLogHostFunction(t *testing.T) {
	rec := &logRecorder{}
	_, hc := newTestNet(t, nil)
	s := newHostile(t, hostileManifest(t), wasm.Options{HTTPClient: hc, Logger: slog.New(rec)})
	rec.reset()
	if _, err := s.List(t.Context(), hostileTarget("log"), source.FetchState{}); err != nil {
		t.Fatal(err)
	}
	want := []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError, slog.LevelInfo, slog.LevelInfo}
	got := rec.all()
	if len(got) != len(want) {
		t.Fatalf("got %d records, want %d", len(got), len(want))
	}
	for i, r := range got {
		if r.Level != want[i] {
			t.Errorf("record %d level = %v, want %v", i, r.Level, want[i])
		}
	}
	if !strings.HasPrefix(got[0].Message, "hello at debug") {
		t.Errorf("message = %q", got[0].Message)
	}
	if n := len(got[5].Message); n > 4<<10+4 {
		t.Errorf("long message not truncated: %d bytes", n)
	}
}

func TestConcurrentCallsAndReuse(t *testing.T) {
	_, hc := newTestNet(t, nil)
	s := newHostile(t, hostileManifest(t), wasm.Options{HTTPClient: hc, MaxIdle: 2})
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 32 {
		wg.Go(func() {
			if _, err := s.List(t.Context(), hostileTarget("counter"), source.FetchState{}); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	// Sequential calls take the same idle instance back: its call counter,
	// which lives in the instance's memory, goes on increasing.
	counter := func() int {
		res, err := s.List(t.Context(), hostileTarget("counter"), source.FetchState{})
		if err != nil {
			t.Fatal(err)
		}
		n, err := strconv.Atoi(res.Versions[0].Raw)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	first := counter()
	if first < 2 {
		t.Fatalf("first counter = %d: the instance that ran a concurrent call was not reused", first)
	}
	if second := counter(); second != first+1 {
		t.Fatalf("counters %d then %d, want consecutive values from one reused instance", first, second)
	}
}

func TestCloseFailsLaterCalls(t *testing.T) {
	_, hc := newTestNet(t, nil)
	s, err := wasm.New(t.Context(), hostileManifest(t), plugins.HostileWasm(), wasm.Options{HTTPClient: hc, CompilationCache: testCache})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(t.Context(), hostileTarget("ok"), source.FetchState{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(t.Context(), hostileTarget("ok"), source.FetchState{}); err == nil {
		t.Fatal("List after Close succeeded")
	}
}

func TestNewErrors(t *testing.T) {
	_, hc := newTestNet(t, nil)
	cm := cratesManifest(t)
	cases := map[string]struct {
		m    wasm.Manifest
		wasm []byte
		opts wasm.Options
	}{
		"no client":         {cm, plugins.CratesWasm(), wasm.Options{}},
		"no transport":      {cm, plugins.CratesWasm(), wasm.Options{HTTPClient: &http.Client{}}},
		"undeclared config": {cm, plugins.CratesWasm(), wasm.Options{HTTPClient: hc, Config: map[string]string{"secret": "x"}}},
		"config without schema": {
			hostileManifest(t), plugins.HostileWasm(),
			wasm.Options{HTTPClient: hc, Config: map[string]string{"token": "x"}},
		},
		"invalid manifest": {wasm.Manifest{Kind: "Bad Kind"}, plugins.CratesWasm(), wasm.Options{HTTPClient: hc}},
		"not wasm":         {cm, []byte("not wasm"), wasm.Options{HTTPClient: hc}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s, err := wasm.New(t.Context(), c.m, c.wasm, c.opts)
			if err == nil {
				_ = s.Close(t.Context())
				t.Fatal("New succeeded")
			}
		})
	}
}
