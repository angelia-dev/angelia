package wasm_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/bench/plugins"
	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/plugin/wasm"
	"github.com/angelia-dev/angelia/internal/plugin/wasm/wasmtest"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/sourcetest"
)

func cratesManifest(t *testing.T) wasm.Manifest {
	t.Helper()
	m, err := wasm.ParseManifest(plugins.CratesManifest())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// newCrates builds the crates plugin against baseURL. The given client's
// transport goes under a netguard client that allows loopback, so the
// netguard path is the production one; allowed_hosts is replaced by the
// fake server's host.
func newCrates(t *testing.T, baseURL string, rt http.RoundTripper, config map[string]string) *wasm.Source {
	t.Helper()
	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	nc, err := netguard.New(netguard.Config{
		Transport:   rt,
		Allow:       []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		HostLimits:  map[string]netguard.HostLimit{u.Hostname(): fastLimit},
		MaxAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	m := cratesManifest(t)
	m.AllowedHosts = []string{u.Hostname()}
	cfg := map[string]string{"base_url": baseURL}
	for k, v := range config {
		cfg[k] = v
	}
	s, err := wasm.New(t.Context(), m, plugins.CratesWasm(), wasm.Options{HTTPClient: nc.HTTPClient(), Config: cfg, CompilationCache: testCache})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func crate(name string) source.Target {
	return source.Target{Kind: "crates", Coordinates: map[string]string{"name": name}}
}

// AC: the WASM crates source passes the source contract suite.
func TestCratesContract(t *testing.T) {
	up := wasmtest.NewCratesUpstream(2, map[string]int{"serde": 5}) // 3 pages
	sourcetest.Run(t, nil, sourcetest.Fixtures{
		Handler: up,
		New: func(baseURL string, c *http.Client) source.Source {
			return newCrates(t, baseURL, c.Transport, nil)
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

// Q1: pagination, ETags and auth headers through the host function.
func TestCratesPaginationETagAuth(t *testing.T) {
	up := wasmtest.NewCratesUpstream(3, map[string]int{"tokio": 7}) // pages of 3, 3, 1
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	s := newCrates(t, srv.URL, srv.Client().Transport, map[string]string{"token": "test-token"})

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
	if res.State.ETag == "" {
		t.Error("no ETag in state")
	}
	for i, a := range up.Authorizations() {
		if a != "test-token" {
			t.Errorf("request %d Authorization = %q, want the configured token", i, a)
		}
	}
	v := res.Versions[0]
	if v.URL != "https://crates.io/crates/tokio/1.0.6" || v.Metadata["checksum"] == nil || v.Metadata["download_url"] != srv.URL+"/api/v1/crates/tokio/1.0.6/download" {
		t.Errorf("version fields not mapped: %+v", v)
	}
	if v.Prerelease != nil {
		t.Errorf("Prerelease = %v, want nil (crates.io has no flag)", *v.Prerelease)
	}
	if !res.Versions[3].Yanked {
		t.Errorf("version %s should be yanked", res.Versions[3].Raw)
	}

	// The second call is one conditional request that ends with a 304.
	again, err := s.List(t.Context(), crate("tokio"), res.State)
	if err != nil {
		t.Fatal(err)
	}
	if !again.NotModified || len(again.Versions) != 0 || again.State != res.State {
		t.Errorf("conditional List = %+v, want NotModified with the same state", again)
	}
	if n := up.Requests(); n != 4 {
		t.Errorf("requests = %d, want 4 (one conditional request)", n)
	}
}

func TestCratesManifestIsValid(t *testing.T) {
	m := cratesManifest(t)
	if m.Kind != "crates" || !slices.Equal(m.AllowedHosts, []string{"crates.io"}) {
		t.Errorf("manifest = %+v", m)
	}
}

// AC: a registry mirror added by the operator, with a host-injected
// credential: the crates plugin is unchanged (manifest allows crates.io
// only, no token in its config) and every page carries the credential,
// bound to the mirror's host:port.
func TestCratesMirrorWithInjectedCredential(t *testing.T) {
	up := wasmtest.NewCratesUpstream(3, map[string]int{"tokio": 7})
	srv := httptest.NewTLSServer(up)
	t.Cleanup(srv.Close)
	nc, err := netguard.New(netguard.Config{
		Transport:   srv.Client().Transport,
		Allow:       []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		HostLimits:  map[string]netguard.HostLimit{"127.0.0.1": fastLimit},
		MaxAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := wasm.New(t.Context(), cratesManifest(t), plugins.CratesWasm(), wasm.Options{
		HTTPClient:   nc.HTTPClient(),
		Config:       map[string]string{"base_url": srv.URL},
		AllowedHosts: []string{"127.0.0.1"},
		Credentials: []wasm.Credential{{Host: strings.TrimPrefix(srv.URL, "https://"), Token: func(context.Context) (string, error) {
			return "mirror-token", nil
		}}},
		CompilationCache: testCache,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	res, err := s.List(t.Context(), crate("tokio"), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Versions) != 7 {
		t.Fatalf("got %d versions, want 7", len(res.Versions))
	}
	if a := up.Authorizations(); len(a) != 3 || slices.ContainsFunc(a, func(v string) bool { return v != "mirror-token" }) {
		t.Errorf("Authorization per page = %q, want the injected credential on all 3", a)
	}
}
