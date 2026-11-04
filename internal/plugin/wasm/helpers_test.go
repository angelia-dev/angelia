package wasm_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/tetratelabs/wazero"

	"github.com/angelia-dev/angelia/bench/plugins"
	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/plugin/wasm"
	"github.com/angelia-dev/angelia/internal/source"
)

// fastLimit keeps netguard's rate limiter out of the way in tests.
var fastLimit = netguard.HostLimit{
	Authenticated: netguard.Limit{Rate: 1e6, Burst: 1000},
	Anonymous:     netguard.Limit{Rate: 1e6, Burst: 1000},
}

// fakeResolver maps hostnames to addresses for the netguard SSRF guard.
type fakeResolver map[string][]netip.Addr

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := f[strings.TrimSuffix(strings.ToLower(host), ".")]; ok {
		return a, nil
	}
	return nil, fmt.Errorf("no such host %q", host)
}

// testNet is an in-memory network: requests are routed by host to
// handlers, and every request that reaches it is recorded. It sits under a
// real netguard client, so the SSRF guard and the limiter apply.
type testNet struct {
	mu       sync.Mutex
	handlers map[string]http.Handler // by URL hostname
	seen     []string                // "METHOD URL" of every request that reached the network
}

func (n *testNet) RoundTrip(r *http.Request) (*http.Response, error) {
	n.mu.Lock()
	n.seen = append(n.seen, r.Method+" "+r.URL.String())
	h := n.handlers[strings.TrimSuffix(strings.ToLower(r.URL.Hostname()), ".")]
	n.mu.Unlock()
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	if h == nil {
		return nil, fmt.Errorf("testNet: no handler for %q", r.URL.Host)
	}
	if r.Body == nil {
		r.Body = http.NoBody // as a server would see it
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

func (n *testNet) requests() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.seen...)
}

// newTestNet returns the network and a netguard HTTP client over it. The
// resolver knows a few public (TEST-NET) and private names.
func newTestNet(t *testing.T, handlers map[string]http.Handler) (*testNet, *http.Client) {
	t.Helper()
	n := &testNet{handlers: handlers}
	res := fakeResolver{
		"allowed.test":   {netip.MustParseAddr("203.0.113.10")},
		"other.test":     {netip.MustParseAddr("203.0.113.11")},
		"denied.test":    {netip.MustParseAddr("203.0.113.12")},
		"metadata.test":  {netip.MustParseAddr("169.254.169.254")},
		"internal.test":  {netip.MustParseAddr("10.1.2.3")},
		"rebind.test":    {netip.MustParseAddr("203.0.113.13"), netip.MustParseAddr("127.0.0.1")},
		"localhost.test": {netip.MustParseAddr("::1")},
	}
	limits := map[string]netguard.HostLimit{}
	for h := range res {
		limits[h] = fastLimit
	}
	nc, err := netguard.New(netguard.Config{
		Transport:   n,
		Resolver:    res,
		HostLimits:  limits,
		MaxAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return n, nc.HTTPClient()
}

func hostileManifest(t *testing.T, allowed ...string) wasm.Manifest {
	t.Helper()
	m, err := wasm.ParseManifest(plugins.HostileManifest())
	if err != nil {
		t.Fatal(err)
	}
	if allowed != nil {
		m.AllowedHosts = allowed
	}
	return m
}

// testCache shares compiled code between the tests' Sources; compiling the
// plugin is the slow part of New.
var testCache = wazero.NewCompilationCache()

func TestMain(m *testing.M) {
	code := m.Run()
	_ = testCache.Close(context.Background())
	os.Exit(code)
}

func newHostile(t *testing.T, m wasm.Manifest, opts wasm.Options) *wasm.Source {
	t.Helper()
	opts.CompilationCache = testCache
	s, err := wasm.New(t.Context(), m, plugins.HostileWasm(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func hostileTarget(mode string, kv ...string) source.Target {
	c := map[string]string{"mode": mode}
	for i := 0; i+1 < len(kv); i += 2 {
		c[kv[i]] = kv[i+1]
	}
	return source.Target{Kind: "hostile", Coordinates: c}
}

// okHandler answers 200 with a fixed body.
func okHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, body)
	})
}

// redirectHandler answers 302 to loc.
func redirectHandler(loc string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, loc, http.StatusFound)
	})
}

// logRecorder is an slog.Handler that keeps every record.
type logRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (l *logRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (l *logRecorder) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, r.Clone())
	return nil
}
func (l *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logRecorder) WithGroup(string) slog.Handler      { return l }

func (l *logRecorder) all() []slog.Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]slog.Record(nil), l.records...)
}

// reset drops the records so far, such as the "wasm plugin installed" line.
func (l *logRecorder) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = nil
}
