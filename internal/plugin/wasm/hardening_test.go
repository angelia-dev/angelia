package wasm_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	extism "github.com/extism/go-sdk"

	"github.com/angelia-dev/angelia/internal/plugin/wasm"
	"github.com/angelia-dev/angelia/internal/source"
)

// authRecorder answers 200 and records the Authorization header it saw.
type authRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (a *authRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.seen = append(a.seen, r.Header.Get("Authorization"))
	a.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (a *authRecorder) all() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.seen...)
}

// A redirect from https to http is refused before it is sent: the
// Authorization header never travels in clear text.
func TestRedirectDowngradeRefused(t *testing.T) {
	plain := &authRecorder{}
	net, hc := newTestNet(t, map[string]http.Handler{
		"allowed.test": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Scheme == "https" {
				http.Redirect(w, r, "http://allowed.test/plain", http.StatusFound)
				return
			}
			plain.ServeHTTP(w, r)
		}),
	})
	s := newHostile(t, hostileManifest(t, "allowed.test"), wasm.Options{HTTPClient: hc})
	_, err := s.List(t.Context(), hostileTarget("fetch", "url", "https://allowed.test/start",
		"authorization", "Bearer s3cret"), source.FetchState{})
	if err == nil || !strings.Contains(err.Error(), "downgrades https") {
		t.Fatalf("err = %v, want the downgrade refused", err)
	}
	if got := net.requests(); len(got) != 1 {
		t.Fatalf("network saw %v, want only the https hop", got)
	}
	if got := plain.all(); len(got) != 0 {
		t.Fatalf("the http target was reached with Authorization %q", got)
	}
}

// A redirect to another allowed host that is not a subdomain loses the
// Authorization header.
func TestRedirectToOtherAllowedHostStripsAuthorization(t *testing.T) {
	other := &authRecorder{}
	_, hc := newTestNet(t, map[string]http.Handler{
		"allowed.test": redirectHandler("https://other.test/final"),
		"other.test":   other,
	})
	s := newHostile(t, hostileManifest(t, "allowed.test", "other.test"), wasm.Options{HTTPClient: hc})
	res, err := s.List(t.Context(), hostileTarget("fetch", "url", "https://allowed.test/",
		"authorization", "Bearer s3cret"), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Versions[0].Raw != "200" {
		t.Fatalf("status = %s", res.Versions[0].Raw)
	}
	if got := other.all(); len(got) != 1 || got[0] != "" {
		t.Fatalf("other.test saw Authorization %q, want it stripped", got)
	}
}

// Redirect hops count against the per-call request budget.
func TestRedirectHopsCountAgainstBudget(t *testing.T) {
	net, hc := newTestNet(t, map[string]http.Handler{"allowed.test": redirectHandler("http://allowed.test/again")})
	s := newHostile(t, hostileManifest(t, "allowed.test"), wasm.Options{HTTPClient: hc, MaxRequestsPerCall: 3})
	_, err := s.List(t.Context(), hostileTarget("fetch_ignore_error", "url", "http://allowed.test/"), source.FetchState{})
	if !errors.Is(err, wasm.ErrPolicy) {
		t.Fatalf("err = %v, want ErrPolicy", err)
	}
	if got := net.requests(); len(got) != 3 {
		t.Fatalf("network saw %d requests, want 3", len(got))
	}
}

func TestPluginHeaders(t *testing.T) {
	var mu sync.Mutex
	var got http.Header
	_, hc := newTestNet(t, map[string]http.Handler{
		"allowed.test": http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			mu.Lock()
			got = r.Header.Clone()
			mu.Unlock()
		}),
	})
	s := newHostile(t, hostileManifest(t, "allowed.test"), wasm.Options{HTTPClient: hc})
	_, err := s.List(t.Context(), hostileTarget("fetch", "url", "http://allowed.test/",
		"h:x-api-version", "2", "h:User-Agent", "plugin/1", "etag", `"v1"`), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got.Get("X-Api-Version") != "2" || got.Get("User-Agent") != "plugin/1" || got.Get("If-None-Match") != `"v1"` {
		t.Fatalf("upstream saw headers %v", got)
	}
}

// countLogs returns the records and how many carry msg.
func countLogs(rec *logRecorder, msg string) ([]slog.Record, int) {
	all := rec.all()
	n := 0
	for _, r := range all {
		if r.Message == msg {
			n++
		}
	}
	return all, n
}

const budgetExhausted = "wasm plugin log budget exhausted; dropping further entries of this call"

// A plugin cannot flood the log: valid, non-JSON and oversized entries all
// draw on one per-call budget, after which one warning is logged.
func TestLogBudget(t *testing.T) {
	rec := &logRecorder{}
	_, hc := newTestNet(t, nil)
	s := newHostile(t, hostileManifest(t), wasm.Options{HTTPClient: hc, Logger: slog.New(rec)})
	if _, err := s.List(t.Context(), hostileTarget("log_flood"), source.FetchState{}); err != nil {
		t.Fatal(err)
	}
	all, n := countLogs(rec, budgetExhausted)
	if n != 1 {
		t.Fatalf("%d budget warnings, want 1", n)
	}
	if len(all) > 257 {
		t.Fatalf("%d records for one call, want at most 256 entries and the warning", len(all))
	}
	var dropped int
	for _, r := range all {
		if strings.HasPrefix(r.Message, "wasm plugin log entry dropped") {
			dropped++
		}
	}
	if dropped == 0 {
		t.Fatal("no dropped-entry warning: the flood did not exercise them")
	}

	// The budget is per call.
	before := len(all)
	if _, err := s.List(t.Context(), hostileTarget("log"), source.FetchState{}); err != nil {
		t.Fatal(err)
	}
	if got := len(rec.all()) - before; got != 6 {
		t.Fatalf("next call logged %d records, want 6", got)
	}
}

// Extism's own log functions share the budget.
func TestExtismLogBudget(t *testing.T) {
	extism.SetLogLevel(extism.LogLevelInfo)
	t.Cleanup(func() { extism.SetLogLevel(extism.LogLevelOff) })
	rec := &logRecorder{}
	_, hc := newTestNet(t, nil)
	s := newHostile(t, hostileManifest(t), wasm.Options{HTTPClient: hc, Logger: slog.New(rec)})
	rec.reset()
	if _, err := s.List(t.Context(), hostileTarget("extism_log_flood"), source.FetchState{}); err != nil {
		t.Fatal(err)
	}
	all, n := countLogs(rec, budgetExhausted)
	if n != 1 || len(all) != 257 {
		t.Fatalf("%d records with %d budget warnings, want 256 entries and 1 warning", len(all), n)
	}
}

func TestValidateTimeout(t *testing.T) {
	_, hc := newTestNet(t, nil)
	s := newHostile(t, hostileManifest(t), wasm.Options{HTTPClient: hc, ValidateTimeout: 100 * time.Millisecond})
	start := time.Now()
	err := s.Validate(hostileTarget("validate_loop"))
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, source.ErrInvalidTarget) {
		t.Fatalf("err = %v, want context.DeadlineExceeded and not ErrInvalidTarget", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("validate ran for %v", d)
	}
	if err := s.Validate(hostileTarget("ok")); err != nil {
		t.Fatalf("Validate after a timeout: %v", err)
	}
}

func TestValidateTimeoutDefault(t *testing.T) {
	if wasm.DefaultValidateTimeout != 2*time.Second {
		t.Fatalf("DefaultValidateTimeout = %v", wasm.DefaultValidateTimeout)
	}
}
