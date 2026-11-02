package goplugin_test

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/bench/plugins/goplugin"
	"github.com/angelia-dev/angelia/bench/plugins/goplugin/goplugintest"
	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/plugin/wasm/wasmtest"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/sourcetest"
)

func TestMain(m *testing.M) {
	if goplugintest.ServeIfChild() {
		return
	}
	os.Exit(m.Run())
}

var fastLimit = netguard.HostLimit{
	Authenticated: netguard.Limit{Rate: 1e6, Burst: 1000},
	Anonymous:     netguard.Limit{Rate: 1e6, Burst: 1000},
}

// newCrates starts the crates plugin against baseURL, with a netguard
// client over rt that allows loopback, and allowed_hosts set to the
// host of baseURL unless mod changes it.
func newCrates(t *testing.T, baseURL string, rt http.RoundTripper, config map[string]string, mod func(*goplugin.Options), ngMod ...func(*netguard.Config)) *goplugin.Source {
	t.Helper()
	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	ngc := netguard.Config{
		Transport:   rt,
		Allow:       []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		HostLimits:  map[string]netguard.HostLimit{u.Hostname(): fastLimit},
		MaxAttempts: 1,
	}
	for _, m := range ngMod {
		m(&ngc)
	}
	nc, err := netguard.New(ngc)
	if err != nil {
		t.Fatal(err)
	}
	cfg := map[string]string{"base_url": baseURL}
	for k, v := range config {
		cfg[k] = v
	}
	opts := goplugin.Options{
		Kind:         "crates",
		Command:      goplugintest.Command,
		AllowedHosts: []string{u.Hostname()},
		HTTPClient:   nc.HTTPClient(),
		Config:       cfg,
	}
	if mod != nil {
		mod(&opts)
	}
	s, err := goplugin.New(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func crate(name string) source.Target {
	return source.Target{Kind: "crates", Coordinates: map[string]string{"name": name}}
}

// AC: the go-plugin crates source passes the source contract suite, with
// the fixtures of the WASM crates source (internal/plugin/wasm).
func TestCratesContract(t *testing.T) {
	up := wasmtest.NewCratesUpstream(2, map[string]int{"serde": 5}) // 3 pages
	sourcetest.Run(t, nil, sourcetest.Fixtures{
		Handler: up,
		New: func(baseURL string, c *http.Client) source.Source {
			return newCrates(t, baseURL, c.Transport, nil, nil)
		},
		Valid: crate("serde"),
		Invalid: []source.Target{
			{Kind: "crates"},
			crate(""),
			crate("a/b"),
			crate("1abc"),
			crate("../../etc"),
			crate(strings.Repeat("a", 65)),
			{Kind: "crates", Coordinates: map[string]string{"name": "serde", "extra": "1"}},
			{Kind: "pypi", Coordinates: map[string]string{"name": "serde"}},
		},
		Missing:        &source.Target{Kind: "crates", Coordinates: map[string]string{"name": "does-not-exist"}},
		WantRaw:        wasmtest.WantRaw(5),
		Conditional:    true,
		Change:         up.Change,
		HasPublishedAt: true,
	})
}

// Pagination, ETags and auth headers through the brokered Host service.
func TestCratesPaginationETagAuth(t *testing.T) {
	up := wasmtest.NewCratesUpstream(3, map[string]int{"tokio": 7})
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	s := newCrates(t, srv.URL, srv.Client().Transport, map[string]string{"token": "test-token"}, nil)

	res, err := s.List(t.Context(), crate("tokio"), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(res.Versions))
	for i, v := range res.Versions {
		got[i] = v.Raw
	}
	if want := wasmtest.WantRaw(7); !slices.Equal(got, want) {
		t.Fatalf("versions = %v, want %v", got, want)
	}
	if n := up.Requests(); n != 3 {
		t.Errorf("requests = %d, want 3 pages", n)
	}
	for i, a := range up.Authorizations() {
		if a != "test-token" {
			t.Errorf("request %d Authorization = %q", i, a)
		}
	}
	if v := res.Versions[0]; v.Metadata["download_url"] != srv.URL+"/api/v1/crates/tokio/1.0.6/download" {
		t.Errorf("version fields not mapped: %+v", v)
	}
	again, err := s.List(t.Context(), crate("tokio"), res.State)
	if err != nil {
		t.Fatal(err)
	}
	if !again.NotModified || again.State != res.State || up.Requests() != 4 {
		t.Errorf("conditional List = %+v after %d requests, want NotModified after 4", again, up.Requests())
	}
}

// A host outside allowed_hosts is refused before it reaches netguard or
// the network, and fails the call.
func TestHostNotAllowed(t *testing.T) {
	up := wasmtest.NewCratesUpstream(10, map[string]int{"serde": 3})
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	s := newCrates(t, srv.URL, srv.Client().Transport, nil, func(o *goplugin.Options) {
		o.AllowedHosts = []string{"crates.io"}
	})
	_, err := s.List(t.Context(), crate("serde"), source.FetchState{})
	if !errors.Is(err, goplugin.ErrHostNotAllowed) {
		t.Fatalf("err = %v, want ErrHostNotAllowed", err)
	}
	if n := up.Requests(); n != 0 {
		t.Errorf("upstream saw %d requests, want 0", n)
	}
}

func TestRedirectToOtherHostRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://evil.invalid/x", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	s := newCrates(t, srv.URL, srv.Client().Transport, nil, nil)
	_, err := s.List(t.Context(), crate("serde"), source.FetchState{})
	if !errors.Is(err, goplugin.ErrHostNotAllowed) {
		t.Fatalf("err = %v, want ErrHostNotAllowed", err)
	}
}

func TestRequestBudget(t *testing.T) {
	up := wasmtest.NewCratesUpstream(1, map[string]int{"serde": 3}) // 3 pages
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	s := newCrates(t, srv.URL, srv.Client().Transport, nil, func(o *goplugin.Options) { o.MaxRequestsPerCall = 2 })
	_, err := s.List(t.Context(), crate("serde"), source.FetchState{})
	if !errors.Is(err, goplugin.ErrPolicy) {
		t.Fatalf("err = %v, want ErrPolicy", err)
	}
	if n := up.Requests(); n != 2 {
		t.Errorf("upstream saw %d requests, want 2", n)
	}
}

// HTTP outside a List call (Validate, or a forged call id) is refused.
func TestHTTPOutsideCall(t *testing.T) {
	srv := httptest.NewServer(wasmtest.NewCratesUpstream(10, nil))
	t.Cleanup(srv.Close)
	s := newCrates(t, srv.URL, srv.Client().Transport, nil, nil)
	resp, err := s.HTTP(t.Context(), &goplugin.HTTPRequest{CallID: "forged", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil || resp.Error.Code != "not_allowed_here" {
		t.Errorf("response = %+v, want not_allowed_here", resp)
	}
}

// A plugin process that dies is restarted on the next call.
func TestRestartAfterCrash(t *testing.T) {
	up := wasmtest.NewCratesUpstream(10, map[string]int{"serde": 3})
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	s := newCrates(t, srv.URL, srv.Client().Transport, nil, nil)
	pid := s.PID()
	killAndWait(t, s)
	res, err := s.List(t.Context(), crate("serde"), source.FetchState{})
	if err != nil {
		t.Fatalf("List after crash: %v", err)
	}
	if len(res.Versions) != 3 || s.PID() == pid {
		t.Errorf("versions = %d, pid %d (was %d): want 3 versions from a new process", len(res.Versions), s.PID(), pid)
	}
}

func TestChecksumMismatchRefused(t *testing.T) {
	sum := sha256.Sum256([]byte("not the plugin"))
	_, err := goplugin.New(t.Context(), goplugin.Options{
		Kind:       "crates",
		Command:    goplugintest.Command,
		Checksum:   sum[:],
		HTTPClient: &http.Client{Transport: http.DefaultTransport},
	})
	if err == nil {
		t.Fatal("New started a plugin whose checksum does not match")
	}
}

func TestNewRequiresOptions(t *testing.T) {
	if _, err := goplugin.New(t.Context(), goplugin.Options{Kind: "crates", Command: goplugintest.Command}); err == nil {
		t.Error("New without an HTTP client succeeded")
	}
	if _, err := goplugin.New(t.Context(), goplugin.Options{HTTPClient: http.DefaultClient}); err == nil {
		t.Error("New without a kind and command succeeded")
	}
}
