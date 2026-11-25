package oci_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/oci"
	"github.com/angelia-dev/angelia/internal/source/sourcetest"
)

const canary = "CANARY-registry-token"

func fixture(t testing.TB, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func img(s string) source.Target {
	return source.Target{Kind: oci.Kind, Coordinates: map[string]string{"image": s}}
}

// registry is a fake registry with the Bearer token flow, on one server.
type registry struct {
	mu        sync.Mutex
	paths     []string // "METHOD path?query" of registry requests
	tokenReqs []url.Values
	tokenAuth []string // Authorization of each token request
	regAuth   []string // Authorization of each registry request
	challenge func(host string) string
	tokenCode int
	pages     map[string]string // query "last" -> body; "" first page
	nextLink  func(host, last string) string
	always401 bool
}

func newRegistry(t testing.TB) *registry {
	return &registry{
		pages: map[string]string{"": fixture(t, "tags-page1.json"), "p2": fixture(t, "tags-page2.json")},
		nextLink: func(host, last string) string {
			if last == "" {
				return `</v2/team/app/tags/list?last=p2&n=1000>; rel="next"`
			}
			return ""
		},
	}
}

func (g *registry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.URL.Path == "/token" {
		g.tokenReqs = append(g.tokenReqs, r.URL.Query())
		g.tokenAuth = append(g.tokenAuth, r.Header.Get("Authorization"))
		if g.tokenCode != 0 {
			http.Error(w, "no", g.tokenCode)
			return
		}
		fmt.Fprintf(w, `{"token":%q,"expires_in":300}`, canary)
		return
	}
	g.paths = append(g.paths, r.Method+" "+r.URL.RequestURI())
	g.regAuth = append(g.regAuth, r.Header.Get("Authorization"))
	if r.Header.Get("Authorization") != "Bearer "+canary || g.always401 {
		ch := fmt.Sprintf(`Bearer realm="http://%s/token",service="registry.test",scope="repository:team/app:pull"`, r.Host)
		if g.challenge != nil {
			ch = g.challenge(r.Host)
		}
		w.Header().Set("WWW-Authenticate", ch)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v2/team/app/tags/list") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	last := r.URL.Query().Get("last")
	if l := g.nextLink(r.Host, last); l != "" {
		w.Header().Set("Link", l)
	}
	_, _ = w.Write([]byte(g.pages[last]))
}

func (g *registry) reqs() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.paths)
}

func newSource(srvURL string, c *http.Client, opts ...oci.Option) *oci.Source {
	u, _ := url.Parse(srvURL)
	opts = append([]oci.Option{oci.WithEndpoint("registry.test", srvURL), oci.WithAllowHTTPHosts(u.Hostname())}, opts...)
	return oci.New("", c, opts...)
}

func TestContract(t *testing.T) {
	reg := newRegistry(t)
	sourcetest.Run(t, nil, sourcetest.Fixtures{
		Handler: reg,
		New:     func(base string, c *http.Client) source.Source { return newSource(base, c) },
		Valid:   img("registry.test/team/app"),
		Invalid: []source.Target{
			{Kind: oci.Kind},
			img("registry.test/team/app:1.0"),
			img("registry.test/team/app@sha256:" + strings.Repeat("a", 64)),
			img("https://registry.test/team/app"),
			img("registry.test/Team/App"),
			img("registry.test/"),
			img("registry.test/" + strings.Repeat("a", 256)),
			{Kind: oci.Kind, Coordinates: map[string]string{"image": "x", "extra": "y"}},
		},
		Missing: &source.Target{Kind: oci.Kind, Coordinates: map[string]string{"image": "registry.test/team/missing"}},
		WantRaw: []string{"0.9.0", "1.0.0", "1.1.0", "1.2.0-rc.1", "latest", "sha-1a2b3c"},
		Aliases: []source.Target{img("REGISTRY.test/team/app")},
	})
}

func TestCanonicalDockerHub(t *testing.T) {
	s := oci.New("", http.DefaultClient)
	for _, in := range []string{"node", "library/node", "docker.io/node", "index.docker.io/library/node", "DOCKER.IO/library/node", "registry-1.docker.io/node"} {
		got, err := source.Canonical(s, img(in))
		if err != nil || got.Coordinates["image"] != "docker.io/library/node" {
			t.Errorf("%q: %v, %v", in, got, err)
		}
	}
	got, _ := source.Canonical(s, img("GHCR.io/org/app"))
	if got.Coordinates["image"] != "ghcr.io/org/app" {
		t.Errorf("ghcr: %v", got)
	}
	got, _ = source.Canonical(s, img("docker.io/someone/tool"))
	if got.Coordinates["image"] != "docker.io/someone/tool" {
		t.Errorf("namespaced: %v", got)
	}
}

func listWith(t *testing.T, reg *registry, opts ...oci.Option) (source.Result, error, *httptest.Server, *http.Client) {
	t.Helper()
	srv := httptest.NewServer(reg)
	t.Cleanup(srv.Close)
	c := sourcetest.Client(t, srv)
	res, err := newSource(srv.URL, c, opts...).List(t.Context(), img("registry.test/team/app"), source.FetchState{})
	return res, err, srv, c
}

func TestTokenFlow(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	reg := newRegistry(t)
	res, err, _, _ := listWith(t, reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.tokenReqs) != 1 {
		t.Fatalf("token requests = %d, want 1", len(reg.tokenReqs))
	}
	q := reg.tokenReqs[0]
	if q.Get("service") != "registry.test" || q.Get("scope") != "repository:team/app:pull" || reg.tokenAuth[0] != "" {
		t.Errorf("token request %v auth %q", q, reg.tokenAuth[0])
	}
	// 401 (no token), retry, then page 2: the token goes on both retries.
	want := []string{"", "Bearer " + canary, "Bearer " + canary}
	if !slices.Equal(reg.regAuth, want) {
		t.Errorf("registry Authorization = %q", reg.regAuth)
	}
	if !res.State.IsZero() {
		t.Errorf("state %+v", res.State)
	}
	for name, s := range map[string]string{"state": fmt.Sprint(res.State), "versions": fmt.Sprint(res.Versions), "logs": logs.String()} {
		if strings.Contains(s, canary) {
			t.Errorf("token leaked into %s", name)
		}
	}
}

func TestTokenFlowErrors(t *testing.T) {
	t.Run("401 after a token", func(t *testing.T) {
		reg := newRegistry(t)
		reg.always401 = true
		_, err, _, _ := listWith(t, reg)
		if err == nil || !strings.Contains(err.Error(), "not found or access denied") || strings.Contains(err.Error(), canary) {
			t.Fatalf("err = %v", err)
		}
		if len(reg.tokenReqs) != 1 {
			t.Errorf("token requests = %d", len(reg.tokenReqs))
		}
	})
	t.Run("http realm outside allow_http_hosts", func(t *testing.T) {
		reg := newRegistry(t)
		srv := httptest.NewServer(reg)
		defer srv.Close()
		s := oci.New("", sourcetest.Client(t, srv), oci.WithEndpoint("registry.test", srv.URL))
		_, err := s.List(t.Context(), img("registry.test/team/app"), source.FetchState{})
		if err == nil || !strings.Contains(err.Error(), "allow_http_hosts") {
			t.Fatalf("err = %v", err)
		}
		if len(reg.tokenReqs) != 0 {
			t.Error("the realm was requested")
		}
	})
	t.Run("basic challenge", func(t *testing.T) {
		reg := newRegistry(t)
		reg.challenge = func(string) string { return `Basic realm="registry"` }
		_, err, _, _ := listWith(t, reg)
		if err == nil || !strings.Contains(err.Error(), "sources.<name>.auth") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("token refused", func(t *testing.T) {
		reg := newRegistry(t)
		reg.tokenCode = http.StatusForbidden
		_, err, _, _ := listWith(t, reg)
		if err == nil || !strings.Contains(err.Error(), "token request") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no challenge", func(t *testing.T) {
		reg := newRegistry(t)
		reg.challenge = func(string) string { return "" }
		_, err, _, _ := listWith(t, reg)
		if err == nil || !strings.Contains(err.Error(), "without a WWW-Authenticate") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestPaging(t *testing.T) {
	t.Run("relative link", func(t *testing.T) {
		reg := newRegistry(t)
		res, err, _, _ := listWith(t, reg)
		if err != nil {
			t.Fatal(err)
		}
		var raws []string
		for _, v := range res.Versions {
			raws = append(raws, v.Raw)
		}
		if !slices.Equal(raws, []string{"0.9.0", "1.0.0", "1.1.0", "1.2.0-rc.1", "latest", "sha-1a2b3c"}) {
			t.Errorf("tags = %v (referrers tags dropped, deduplicated, sorted)", raws)
		}
		if got := reg.reqs(); len(got) != 3 || !strings.Contains(got[0], "n=1000") || !strings.Contains(got[2], "last=p2") {
			t.Errorf("requests = %v", got)
		}
	})
	t.Run("absolute link", func(t *testing.T) {
		reg := newRegistry(t)
		reg.nextLink = func(host, last string) string {
			if last == "" {
				return fmt.Sprintf(`<http://%s/v2/team/app/tags/list?last=p2&n=1000>; rel="next"`, host)
			}
			return ""
		}
		res, err, _, _ := listWith(t, reg)
		if err != nil || len(res.Versions) != 6 {
			t.Fatalf("%d versions, %v", len(res.Versions), err)
		}
	})
	t.Run("another origin", func(t *testing.T) {
		reg := newRegistry(t)
		reg.nextLink = func(string, string) string { return `<http://evil.test/v2/team/app/tags/list?last=p2>; rel="next"` }
		_, err, _, _ := listWith(t, reg) // the guard client fails the test on a request to evil.test
		if err == nil || !strings.Contains(err.Error(), "another origin") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("too many pages", func(t *testing.T) {
		reg := newRegistry(t)
		reg.nextLink = func(string, string) string { return `</v2/team/app/tags/list?last=p2&n=1000>; rel="next"` }
		_, err, _, _ := listWith(t, reg)
		if err == nil || !strings.Contains(err.Error(), "more than 50 pages") {
			t.Fatalf("err = %v", err)
		}
		if n := len(reg.reqs()); n != 51 { // one 401 plus 50 pages
			t.Errorf("requests = %d", n)
		}
	})
	t.Run("link with several relations", func(t *testing.T) {
		reg := newRegistry(t)
		reg.nextLink = func(_, last string) string {
			if last == "" {
				return `</x>; rel="prev", </v2/team/app/tags/list?last=p2&n=1000>; rel="next last"`
			}
			return ""
		}
		if res, err, _, _ := listWith(t, reg); err != nil || len(res.Versions) != 6 {
			t.Fatalf("%d, %v", len(res.Versions), err)
		}
	})
}

func TestPinned(t *testing.T) {
	s := oci.New("https://registry-1.docker.io", http.DefaultClient)
	for _, ok := range []string{"node", "docker.io/library/node", "index.docker.io/node"} {
		if err := s.Validate(img(ok)); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"ghcr.io/org/app", "evil.test/x/y"} {
		if err := s.Validate(img(bad)); !errors.Is(err, source.ErrInvalidTarget) {
			t.Errorf("%q: %v, want ErrInvalidTarget", bad, err)
		}
	}
	h := oci.New("https://harbor.test/", http.DefaultClient)
	if err := h.Validate(img("harbor.test/team/app")); err != nil {
		t.Error(err)
	}
	if err := h.Validate(img("node")); err == nil {
		t.Error("a Harbor-pinned source accepts a Docker Hub image")
	}
}

func TestListHonoursPinnedOnList(t *testing.T) {
	s := oci.New("https://harbor.test", http.DefaultClient)
	if _, err := s.List(context.Background(), img("ghcr.io/o/a"), source.FetchState{}); !errors.Is(err, source.ErrInvalidTarget) {
		t.Fatalf("err = %v", err)
	}
}

func TestStatusErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	_, err := newSource(srv.URL, sourcetest.Client(t, srv)).List(t.Context(), img("registry.test/team/app"), source.FetchState{})
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("err = %v", err)
	}
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("not json")) }))
	defer srv2.Close()
	_, err = newSource(srv2.URL, sourcetest.Client(t, srv2)).List(t.Context(), img("registry.test/team/app"), source.FetchState{})
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("err = %v", err)
	}
}
