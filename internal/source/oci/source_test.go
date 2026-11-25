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

	"github.com/angelia-dev/angelia/internal/netguard"
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
	manifests map[string]string // tag -> Docker-Content-Digest header ("" for none)
	headCode  map[string]int    // tag -> status of the HEAD
	accepts   []string
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
	if tag, ok := strings.CutPrefix(r.URL.Path, "/v2/team/app/manifests/"); ok {
		g.accepts = append(g.accepts, r.Header.Get("Accept"))
		if c := g.headCode[tag]; c != 0 {
			if c == 429 {
				w.Header().Set("Retry-After", "120")
			}
			w.WriteHeader(c)
			return
		}
		if d := g.manifests[tag]; d != "" {
			w.Header().Set("Docker-Content-Digest", d)
		}
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
	if res.State.Cursor == "" || res.State.ETag != "" {
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

func dig(c string) string { return "sha256:" + strings.Repeat(c, 64) }

func rawsOf(res source.Result) []string {
	var r []string
	for _, v := range res.Versions {
		r = append(r, v.Raw)
	}
	return r
}

func heads(g *registry) []string {
	var h []string
	for _, p := range g.reqs() {
		if strings.HasPrefix(p, "HEAD ") {
			h = append(h, p)
		}
	}
	return h
}

// onePage makes the fake serve one page of the given tags.
func onePage(g *registry, tags ...string) {
	g.nextLink = func(string, string) string { return "" }
	g.pages[""] = `{"name":"team/app","tags":["` + strings.Join(tags, `","`) + `"]}`
}

func TestDigests(t *testing.T) {
	reg := newRegistry(t)
	srv := httptest.NewServer(reg)
	defer srv.Close()
	s := newSource(srv.URL, sourcetest.Client(t, srv))
	tgt := img("registry.test/team/app")
	ctx := t.Context()
	onePage(reg, "1.0.0", "latest")
	reg.manifests = map[string]string{"1.1.0": dig("a"), "1.0.0": dig("e")}

	first, err := s.List(ctx, tgt, source.FetchState{})
	if err != nil || len(heads(reg)) != 0 {
		t.Fatalf("first scan: %v, %d HEADs (want a baseline without lookups)", err, len(heads(reg)))
	}
	for _, v := range first.Versions {
		if v.Metadata != nil {
			t.Errorf("baseline %s carries metadata %v", v.Raw, v.Metadata)
		}
	}
	again, err := s.List(ctx, tgt, first.State)
	if err != nil || len(heads(reg)) != 0 || again.State.Cursor != first.State.Cursor {
		t.Fatalf("unchanged: %v, %d HEADs, cursor changed %v", err, len(heads(reg)), again.State.Cursor != first.State.Cursor)
	}

	onePage(reg, "1.0.0", "1.1.0", "latest")
	third, err := s.List(ctx, tgt, again.State)
	if err != nil {
		t.Fatal(err)
	}
	if h := heads(reg); len(h) != 1 || !strings.HasSuffix(h[0], "/manifests/1.1.0") {
		t.Fatalf("HEADs = %v, want one for the new tag", h)
	}
	for _, want := range []string{"application/vnd.oci.image.index.v1+json", "application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json", "application/vnd.docker.distribution.manifest.v2+json"} {
		if !strings.Contains(reg.accepts[0], want) {
			t.Errorf("Accept %q lacks %s", reg.accepts[0], want)
		}
	}
	for _, v := range third.Versions {
		d, _ := v.Metadata["digest"].(string)
		if (v.Raw == "1.1.0") != (d == dig("a")) {
			t.Errorf("%s: digest %q", v.Raw, d)
		}
	}
}

func TestDigestFailures(t *testing.T) {
	setup := func(t *testing.T) (*registry, *oci.Source, source.FetchState, func(context.Context) (source.Result, error)) {
		reg := newRegistry(t)
		srv := httptest.NewServer(reg)
		t.Cleanup(srv.Close)
		s := newSource(srv.URL, sourcetest.Client(t, srv))
		onePage(reg, "1.0.0")
		base, err := s.List(t.Context(), img("registry.test/team/app"), source.FetchState{})
		if err != nil {
			t.Fatal(err)
		}
		onePage(reg, "1.0.0", "2.0.0")
		return reg, s, base.State, func(ctx context.Context) (source.Result, error) {
			return s.List(ctx, img("registry.test/team/app"), base.State)
		}
	}
	t.Run("invalid or missing digest header", func(t *testing.T) {
		for _, hdr := range []string{"", "md5:abc", "sha256:ZZ"} {
			reg, _, _, list := setup(t)
			reg.manifests = map[string]string{"2.0.0": hdr}
			res, err := list(t.Context())
			if err != nil || len(res.Versions) != 2 || res.Versions[1].Metadata != nil {
				t.Errorf("%q: %v, %+v", hdr, err, res.Versions)
			}
		}
	})
	t.Run("5xx and 404 leave the tag without a digest and warn", func(t *testing.T) {
		for _, code := range []int{500, 404} {
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			reg, _, _, list := setup(t)
			reg.headCode = map[string]int{"2.0.0": code}
			res, err := list(t.Context())
			slog.SetDefault(old)
			if err != nil || len(res.Versions) != 2 || res.Versions[1].Metadata != nil || res.State.Cursor == "" {
				t.Errorf("%d: %v, %+v", code, err, res)
			}
			if !strings.Contains(logs.String(), "level=WARN") || strings.Contains(logs.String(), canary) {
				t.Errorf("%d: logs %q", code, logs.String())
			}
		}
	})
	t.Run("429 fails the call", func(t *testing.T) {
		reg, _, _, list := setup(t)
		reg.headCode = map[string]int{"2.0.0": 429}
		res, err := list(t.Context())
		if err == nil || !strings.Contains(err.Error(), "429") || res.State.Cursor != "" {
			t.Fatalf("err = %v, state %q", err, res.State.Cursor)
		}
	})
	t.Run("cancelled context fails the call", func(t *testing.T) {
		_, _, _, list := setup(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := list(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestDigestLookupsAreBounded(t *testing.T) {
	reg := newRegistry(t)
	srv := httptest.NewServer(reg)
	defer srv.Close()
	s := newSource(srv.URL, sourcetest.Client(t, srv))
	onePage(reg, "0.0.1")
	base, _ := s.List(t.Context(), img("registry.test/team/app"), source.FetchState{})
	var tags []string
	for i := 0; i < 80; i++ {
		tags = append(tags, fmt.Sprintf("1.0.%d", i))
	}
	onePage(reg, tags...)
	if _, err := s.List(t.Context(), img("registry.test/team/app"), base.State); err != nil {
		t.Fatal(err)
	}
	if n := len(heads(reg)); n != 50 {
		t.Errorf("HEADs = %d, want 50", n)
	}
}

func TestUnreadableCursorIsAFirstScan(t *testing.T) {
	reg := newRegistry(t)
	srv := httptest.NewServer(reg)
	defer srv.Close()
	s := newSource(srv.URL, sourcetest.Client(t, srv))
	onePage(reg, "1.0.0")
	for _, c := range []string{"!!not base64!!", "QUJD"} { // bad base64; 3 bytes, not a multiple of 4
		res, err := s.List(t.Context(), img("registry.test/team/app"), source.FetchState{Cursor: c})
		if err != nil || len(heads(reg)) != 0 || res.State.Cursor == c {
			t.Errorf("%q: %v, %d HEADs", c, err, len(heads(reg)))
		}
	}
}

func TestDockerHubTagPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tags":["22.11.0"]}`))
	}))
	defer srv.Close()
	s := oci.New("", sourcetest.Client(t, srv), oci.WithEndpoint("docker.io", srv.URL))
	for image, want := range map[string]string{
		"node":          "https://hub.docker.com/_/node/tags?name=22.11.0",
		"someone/tool":  "https://hub.docker.com/r/someone/tool/tags?name=22.11.0",
		"library/redis": "https://hub.docker.com/_/redis/tags?name=22.11.0",
	} {
		res, err := s.List(t.Context(), img(image), source.FetchState{})
		if err != nil || len(res.Versions) != 1 || res.Versions[0].URL != want {
			t.Errorf("%s: %v, %+v", image, err, res.Versions)
		}
	}
	g := newRegistry(t)
	if res, err, _, _ := listWith(t, g); err != nil || res.Versions[0].URL != "" {
		t.Errorf("another registry: %v, URL %q", err, res.Versions[0].URL)
	}
}

func TestTokenRateLimitKeepsRetryAt(t *testing.T) {
	reg := newRegistry(t)
	reg.tokenCode = http.StatusTooManyRequests
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		reg.ServeHTTP(w, r)
	}))
	defer srv.Close()
	_, err := newSource(srv.URL, sourcetest.Client(t, srv)).List(t.Context(), img("registry.test/team/app"), source.FetchState{})
	var se *netguard.StatusError
	if !errors.As(err, &se) || se.StatusCode != 429 || se.RetryAt.IsZero() {
		t.Fatalf("err = %v, want a StatusError with RetryAt", err)
	}
}
