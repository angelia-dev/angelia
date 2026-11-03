package goplugin_test

import (
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/bench/plugins/goplugin"
	"github.com/angelia-dev/angelia/bench/plugins/goplugin/goplugintest"
	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/plugin/wasm/wasmtest"
	"github.com/angelia-dev/angelia/internal/source"
)

// The HTTP rules of ADR 0013 (those of the WASM host, ADR 0011), checked
// through the gRPC path.

func counting(h http.Handler) (http.Handler, *atomic.Int32) {
	var n atomic.Int32
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		h.ServeHTTP(w, r)
	}), &n
}

func TestHTTPSDowngradeRedirectRefused(t *testing.T) {
	plainH, hits := counting(wasmtest.NewCratesUpstream(10, map[string]int{"serde": 1}))
	plain := httptest.NewServer(plainH)
	t.Cleanup(plain.Close)
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+r.URL.Path, http.StatusFound) //nolint:gosec // G710: test server
	}))
	t.Cleanup(tlsSrv.Close)
	s := newCrates(t, tlsSrv.URL, tlsSrv.Client().Transport, nil, nil) // both servers are on 127.0.0.1
	_, err := s.List(t.Context(), crate("serde"), source.FetchState{})
	if err == nil || !strings.Contains(err.Error(), "downgrades https") {
		t.Fatalf("err = %v, want the https downgrade refused", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the http server saw %d requests, want 0", n)
	}
}

func TestSSRFGuardApplies(t *testing.T) {
	srv := httptest.NewServer(wasmtest.NewCratesUpstream(10, map[string]int{"serde": 1}))
	t.Cleanup(srv.Close)
	s := newCrates(t, srv.URL, srv.Client().Transport, nil, nil, func(c *netguard.Config) { c.Allow = nil })
	_, err := s.List(t.Context(), crate("serde"), source.FetchState{})
	if !errors.Is(err, netguard.ErrBlocked) {
		t.Fatalf("err = %v, want netguard.ErrBlocked for loopback", err)
	}
}

func TestRedirectChainCountsAgainstBudget(t *testing.T) {
	h, hits := counting(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path+"x", http.StatusFound) //nolint:gosec // G710: test server
	}))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	s := newCrates(t, srv.URL, srv.Client().Transport, nil, func(o *goplugin.Options) { o.MaxRequestsPerCall = 3 })
	_, err := s.List(t.Context(), crate("serde"), source.FetchState{})
	if !errors.Is(err, goplugin.ErrPolicy) {
		t.Fatalf("err = %v, want ErrPolicy", err)
	}
	if n := hits.Load(); n != 3 {
		t.Errorf("server saw %d requests, want 3", n)
	}
}

func TestResponseBodyCap(t *testing.T) {
	srv := httptest.NewServer(wasmtest.NewCratesUpstream(10, map[string]int{"serde": 5}))
	t.Cleanup(srv.Close)
	s := newCrates(t, srv.URL, srv.Client().Transport, nil, func(o *goplugin.Options) { o.MaxResponseBytes = 64 })
	_, err := s.List(t.Context(), crate("serde"), source.FetchState{})
	if err == nil || !strings.Contains(err.Error(), "too_large") {
		t.Fatalf("err = %v, want too_large", err)
	}
}

// newProbe starts the probe plugin with allowed_hosts set to srv's host.
func newProbe(t *testing.T, srv *httptest.Server) *goplugin.Source {
	t.Helper()
	return newCrates(t, srv.URL, srv.Client().Transport, nil, func(o *goplugin.Options) {
		o.Kind = "probe"
		o.Command = func() *exec.Cmd { return goplugintest.CommandFor(goplugintest.Probe) }
	})
}

func probeList(t *testing.T, s *goplugin.Source, kv ...string) (string, error) {
	t.Helper()
	c := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		c[kv[i]] = kv[i+1]
	}
	res, err := s.List(t.Context(), source.Target{Kind: "probe", Coordinates: c}, source.FetchState{})
	if err != nil {
		return "", err
	}
	return res.Versions[0].Raw, nil
}

func okServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A plugin that ignores the refusal and reports success still fails.
func TestSwallowedDenialStillFailsTheCall(t *testing.T) {
	s := newProbe(t, okServer(t))
	_, err := probeList(t, s, "mode", "swallow", "url", "https://evil.invalid/")
	if !errors.Is(err, goplugin.ErrHostNotAllowed) {
		t.Fatalf("err = %v, want ErrHostNotAllowed although the plugin swallowed it", err)
	}
}

func TestHeadersCheckedOverGRPC(t *testing.T) {
	srv := okServer(t)
	s := newProbe(t, srv)
	for header, want := range map[string]string{
		"":                  "status-200",
		"X-Ok=1":            "status-200",
		"Host=evil.test":    "bad_request",
		"Connection=close":  "bad_request",
		"X-Bad=a\x01b":      "bad_request",
		"Bad Name=1":        "bad_request",
		"Transfer-Encoding": "bad_request",
	} {
		got, err := probeList(t, s, "mode", "fetch", "url", srv.URL, "header", header)
		if err != nil {
			t.Fatalf("%q: %v", header, err)
		}
		if got != want {
			t.Errorf("header %q: %s, want %s", header, got, want)
		}
	}
}

// A request that arrives after its List call has returned is refused.
func TestHTTPAfterCallRefused(t *testing.T) {
	srv := okServer(t)
	s := newProbe(t, srv)
	if _, err := probeList(t, s, "mode", "late", "url", srv.URL); err != nil {
		t.Fatal(err)
	}
	got, err := probeList(t, s, "mode", "report")
	if err != nil {
		t.Fatal(err)
	}
	if got != "not_allowed_here" {
		t.Errorf("late request: %s, want not_allowed_here", got)
	}
}

func TestStartTimeout(t *testing.T) {
	begin := time.Now()
	_, err := goplugin.New(t.Context(), goplugin.Options{
		Kind:         "crates",
		Command:      func() *exec.Cmd { return goplugintest.CommandFor(goplugintest.Hang) },
		HTTPClient:   &http.Client{Transport: http.DefaultTransport},
		StartTimeout: 500 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("New succeeded with a plugin that never completes the handshake")
	}
	if d := time.Since(begin); d > 5*time.Second {
		t.Errorf("New took %v, want about the 500ms StartTimeout", d)
	}
}

// After a failed restart, calls fail fast until RestartBackoff has passed.
func TestRestartBackoff(t *testing.T) {
	srv := httptest.NewServer(wasmtest.NewCratesUpstream(10, map[string]int{"serde": 1}))
	t.Cleanup(srv.Close)
	var broken atomic.Bool
	s := newCrates(t, srv.URL, srv.Client().Transport, nil, func(o *goplugin.Options) {
		o.Command = func() *exec.Cmd {
			if broken.Load() {
				return exec.Command("/nonexistent/angelia-plugin")
			}
			return goplugintest.Command()
		}
		o.RestartBackoff = time.Hour
	})
	broken.Store(true)
	killAndWait(t, s)
	if _, err := s.List(t.Context(), crate("serde"), source.FetchState{}); err == nil || errors.Is(err, goplugin.ErrRestartBackoff) {
		t.Fatalf("first List after crash: err = %v, want the failed restart", err)
	}
	begin := time.Now()
	_, err := s.List(t.Context(), crate("serde"), source.FetchState{})
	if !errors.Is(err, goplugin.ErrRestartBackoff) {
		t.Fatalf("second List: err = %v, want ErrRestartBackoff", err)
	}
	if d := time.Since(begin); d > time.Second {
		t.Errorf("refusal took %v, want it immediate", d)
	}
}

func killAndWait(t *testing.T, s *goplugin.Source) {
	t.Helper()
	proc, err := os.FindProcess(s.PID())
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !s.Exited() {
		if time.Now().After(deadline) {
			t.Fatal("plugin process not reported as exited")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestChecksumMatchAccepted(t *testing.T) {
	bin, err := os.ReadFile(os.Args[0]) //nolint:gosec // G703: the test binary itself
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(bin)
	srv := httptest.NewServer(wasmtest.NewCratesUpstream(10, map[string]int{"serde": 2}))
	t.Cleanup(srv.Close)
	s := newCrates(t, srv.URL, srv.Client().Transport, nil, func(o *goplugin.Options) { o.Checksum = sum[:] })
	if _, err := s.List(t.Context(), crate("serde"), source.FetchState{}); err != nil {
		t.Fatal(err)
	}
}

func TestAllowedHostsMustBeBare(t *testing.T) {
	for _, h := range []string{"crates.io:443", "https://crates.io", "crates.io/api", "user@crates.io", ""} {
		_, err := goplugin.New(t.Context(), goplugin.Options{
			Kind: "crates", Command: goplugintest.Command, AllowedHosts: []string{h},
			HTTPClient: &http.Client{Transport: http.DefaultTransport},
		})
		if err == nil {
			t.Errorf("allowed host %q accepted", h)
		}
	}
}

// An IP literal matches in any spelling.
func TestAllowedIPCanonical(t *testing.T) {
	srv := httptest.NewServer(wasmtest.NewCratesUpstream(10, map[string]int{"serde": 1}))
	t.Cleanup(srv.Close)
	s := newCrates(t, srv.URL, srv.Client().Transport, nil, func(o *goplugin.Options) {
		o.AllowedHosts = []string{"::FFFF:127.0.0.1"}
	})
	if _, err := s.List(t.Context(), crate("serde"), source.FetchState{}); err != nil {
		t.Fatal(err)
	}
}
