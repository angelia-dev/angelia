package goproxy_test

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/goproxy"
	"github.com/angelia-dev/angelia/internal/source/sourcetest"
)

const (
	lowMod = "example.test/team/lib"
	capMod = "example.test/Team/Lib"
	// escaped request prefix of capMod.
	capPrefix = "/example.test/!team/!lib/@v/"
)

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // G304: fixed testdata names.
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func target(mod string) source.Target {
	return source.Target{Kind: "go-proxy", Coordinates: map[string]string{"module": mod}}
}

// proxy is a fake GOPROXY server. docs maps a request path (the escaped,
// case-encoded one) to a body, codes maps a path to a status that replaces
// the body.
type proxy struct {
	mu    sync.Mutex
	docs  map[string]string
	codes map[string]int
	reqs  []string
	hdrs  []http.Header
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.reqs = append(p.reqs, r.URL.Path)
	p.hdrs = append(p.hdrs, r.Header.Clone())
	body, ok := p.docs[r.URL.Path]
	code := p.codes[r.URL.Path]
	p.mu.Unlock()
	switch {
	case code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable:
		w.Header().Set("Retry-After", "120")
		http.Error(w, "slow down", code)
	case code != 0:
		http.Error(w, "boom", code)
	case !ok:
		// What proxy.golang.org says for an unknown or wrong-case path.
		http.Error(w, "not found: invalid escaped module path", http.StatusNotFound)
	default:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}
}

func (p *proxy) set(path, body string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.docs[path] = body
}

func (p *proxy) fail(path string, code int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.codes[path] = code
}

func (p *proxy) requests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.reqs)
}

// lookups returns the requests that are not @latest or go.mod ones: the list
// and the dates.
func (p *proxy) lookups() []string {
	return slices.DeleteFunc(p.requests(), func(r string) bool {
		return strings.HasSuffix(r, "/@latest") || strings.HasSuffix(r, ".mod")
	})
}

func (p *proxy) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs, p.hdrs = nil, nil
}

// libProxy serves the synthetic module example.test/Team/Lib from testdata.
func libProxy(t testing.TB) *proxy {
	return &proxy{
		codes: map[string]int{},
		docs: map[string]string{
			capPrefix + "list":                 string(fixture(t, "team-lib.list")),
			capPrefix + "v1.2.0.info":          string(fixture(t, "team-lib-v1.2.0.info")),
			"/example.test/!team/!lib/@latest": string(fixture(t, "team-lib-v1.2.0.info")),
			capPrefix + "v1.2.0.mod":           "module example.test/Team/Lib\n\ngo 1.22\n",
			capPrefix + "v1.1.0.info":          string(fixture(t, "team-lib-v1.1.0.info")),
			capPrefix + "v1.0.0.info":          `{"Version":"v1.0.0","Time":"2026-01-01T10:00:00Z"}`,
			capPrefix + "v1.2.0-rc.1.info":     `{"Version":"v1.2.0-rc.1","Time":"2026-02-20T10:00:00Z"}`,
			capPrefix + "v1.0.0-!r!c1.info":    `{"Version":"v1.0.0-RC1","Time":"2025-12-01T10:00:00Z"}`,
			// A second, lower-case module: another module for Go.
			"/example.test/team/lib/@v/list": "v0.1.0\n",
		},
	}
}

func newSource(t testing.TB, srv *httptest.Server, base string, buf *bytes.Buffer) *goproxy.Source {
	t.Helper()
	h := slog.Handler(slog.DiscardHandler)
	if buf != nil {
		h = slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	}
	return goproxy.New(srv.URL+base, srv.Client(), goproxy.WithLogger(slog.New(h)))
}

func TestContract(t *testing.T) {
	p := libProxy(t)
	sourcetest.Run(t, nil, sourcetest.Fixtures{
		Handler: p,
		New: func(baseURL string, c *http.Client) source.Source {
			return goproxy.New(baseURL, c, goproxy.WithLogger(slog.New(slog.DiscardHandler)))
		},
		Valid: target(capMod),
		Invalid: []source.Target{
			{Kind: "go-proxy"},
			target(""),
			target("lib/x"),             // no dot in the first element
			target(capMod + "@v1.2.3"),  // version suffix
			target("https://" + capMod), // scheme
			target("../" + capMod),      // dot-dot
			target("/" + capMod),        // leading slash
			target(capMod + "/"),        // trailing slash
			target("example.test/" + strings.Repeat("a", 301)), // too long
			{Kind: "npm", Coordinates: map[string]string{"module": capMod}},
		},
		Missing: func() *source.Target { m := target("example.test/missing/mod"); return &m }(),
		WantRaw: []string{"v1.2.0", "v1.2.0-rc.1", "v1.1.0", "v1.0.0", "v1.0.0-RC1"},
	})
}

func TestValidateAccepts(t *testing.T) {
	s := goproxy.New("", &http.Client{})
	for _, m := range []string{capMod, lowMod, "github.com/BurntSushi/toml", "gopkg.in/yaml.v3", "example.test/team/lib/v2", "golang.org/x/mod"} {
		if err := s.Validate(target(m)); err != nil {
			t.Errorf("%s: %v", m, err)
		}
	}
	for _, m := range []string{"x.test/a b", "x.test/a/v1", "x.test/a@latest"} {
		if err := s.Validate(target(m)); !errors.Is(err, source.ErrInvalidTarget) {
			t.Errorf("%q: err = %v, want ErrInvalidTarget", m, err)
		}
	}
	// No canonical form: the source is not a Canonicalizer.
	if _, ok := any(s).(source.Canonicalizer); ok {
		t.Error("go-proxy implements Canonicalizer; module paths are case-sensitive")
	}
}

// Case encoding: a path and a version with capitals are sent with "!".
func TestCaseEncodingInRequests(t *testing.T) {
	p := libProxy(t)
	srv := httptest.NewServer(p)
	defer srv.Close()
	s := newSource(t, srv, "", nil)
	if _, err := s.List(t.Context(), target(capMod), source.FetchState{}); err != nil {
		t.Fatal(err)
	}
	// Second scan with a cursor that lacks v1.0.0-RC1: one .info for it.
	st := source.FetchState{Cursor: `{"versions":["v1.0.0","v1.1.0","v1.2.0","v1.2.0-rc.1"]}`}
	p.reset()
	res, err := s.List(t.Context(), target(capMod), st)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{capPrefix + "list", capPrefix + "v1.0.0-!r!c1.info"}
	if got := p.lookups(); !slices.Equal(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	for _, v := range res.Versions {
		if v.Raw == "v1.0.0-RC1" && !v.PublishedAt.Equal(time.Date(2025, 12, 1, 10, 0, 0, 0, time.UTC)) {
			t.Errorf("PublishedAt of v1.0.0-RC1 = %v", v.PublishedAt)
		}
	}
}

func TestWrongCaseIsNotFound(t *testing.T) {
	srv := httptest.NewServer(libProxy(t))
	defer srv.Close()
	// "example.test/Team/Lib" is served; "example.test/TEAM/lib" is not.
	_, err := newSource(t, srv, "", nil).List(t.Context(), target("example.test/TEAM/lib"), source.FetchState{})
	var se *netguard.StatusError
	if !errors.As(err, &se) || se.StatusCode != 404 || !strings.Contains(err.Error(), "case-sensitive") {
		t.Fatalf("err = %v, want a 404 that says module paths are case-sensitive", err)
	}
}

func TestGoneIsAnError(t *testing.T) {
	p := libProxy(t)
	p.fail(capPrefix+"list", http.StatusGone)
	srv := httptest.NewServer(p)
	defer srv.Close()
	_, err := newSource(t, srv, "", nil).List(t.Context(), target(capMod), source.FetchState{})
	var se *netguard.StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusGone || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestListFiltersAndOrders(t *testing.T) {
	p := libProxy(t)
	list := strings.Join([]string{
		"v1.0.0", "v1.0.0", // duplicate
		"v2.0.0+incompatible", "v2.0.3+incompatible", "v2.0.1+incompatible",
		"v0.0.0-20240102030405-abcdef123456", // pseudo-version
		"v1.2.3-0.20240102030405-abcdef123456",
		"1.5.0",   // no v
		"v1.5",    // shorthand
		"v1",      // shorthand
		"garbage", // not a version
		"",
		"v1.10.0", "v1.9.0",
		"v1.0.0+meta", "v1.0.0+aaa", // equal precedence: ties by raw
	}, "\n") + "\n"
	p.set(capPrefix+"list", list)
	srv := httptest.NewServer(p)
	defer srv.Close()
	var buf bytes.Buffer
	res, err := newSource(t, srv, "", &buf).List(t.Context(), target(capMod), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range res.Versions {
		got = append(got, v.Raw)
		if v.Prerelease != nil || v.Yanked {
			t.Errorf("%s: Prerelease %v Yanked %v, want nil and false", v.Raw, v.Prerelease, v.Yanked)
		}
	}
	want := []string{"v2.0.3+incompatible", "v2.0.1+incompatible", "v2.0.0+incompatible", "v1.10.0", "v1.9.0", "v1.0.0", "v1.0.0+aaa", "v1.0.0+meta"}
	if !slices.Equal(got, want) {
		t.Fatalf("versions = %v\nwant       %v", got, want)
	}
	if strings.Count(buf.String(), "level=DEBUG") != 1 || !strings.Contains(buf.String(), "count=6") {
		t.Errorf("log = %q, want one DEBUG record for 6 skipped lines", buf.String())
	}
}

func TestEmptyListIsNotAnError(t *testing.T) {
	p := libProxy(t)
	p.set(capPrefix+"list", "")
	srv := httptest.NewServer(p)
	defer srv.Close()
	res, err := newSource(t, srv, "", nil).List(t.Context(), target(capMod), source.FetchState{})
	if err != nil || len(res.Versions) != 0 || res.NotModified {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if res.State.Cursor == "" {
		t.Error("no cursor after an empty list")
	}
	// The next scan has a readable cursor: a new version is looked up.
	p.set(capPrefix+"list", "v1.1.0\n")
	p.reset()
	if _, err := newSource(t, srv, "", nil).List(t.Context(), target(capMod), res.State); err != nil {
		t.Fatal(err)
	}
	if got := p.lookups(); len(got) != 2 {
		t.Errorf("requests = %v, want list and one .info", got)
	}
}

func TestLookups(t *testing.T) {
	p := libProxy(t)
	srv := httptest.NewServer(p)
	defer srv.Close()
	s := newSource(t, srv, "", nil)
	ctx := t.Context()

	// First scan: a baseline, one request, no dates.
	res, err := s.List(ctx, target(capMod), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.lookups(); len(got) != 1 {
		t.Fatalf("first scan sent %v, want only the list", got)
	}
	for _, v := range res.Versions {
		if !v.PublishedAt.IsZero() {
			t.Errorf("%s dated on the first scan", v.Raw)
		}
	}
	// Unchanged: one request.
	p.reset()
	res2, err := s.List(ctx, target(capMod), res.State)
	if err != nil || len(p.lookups()) != 1 || res2.State.Cursor != res.State.Cursor {
		t.Fatalf("unchanged scan: requests %v, err %v, cursor %q vs %q", p.requests(), err, res2.State.Cursor, res.State.Cursor)
	}
	// A new version: one .info, dated.
	p.set(capPrefix+"list", string(fixture(t, "team-lib.list"))+"v1.3.0\n")
	p.set(capPrefix+"v1.3.0.info", `{"Version":"v1.3.0","Time":"2026-04-01T00:00:00Z"}`)
	p.reset()
	res3, err := s.List(ctx, target(capMod), res.State)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{capPrefix + "list", capPrefix + "v1.3.0.info"}; !slices.Equal(p.lookups(), want) {
		t.Fatalf("requests = %v, want %v", p.lookups(), want)
	}
	if res3.Versions[0].Raw != "v1.3.0" || !res3.Versions[0].PublishedAt.Equal(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("newest = %+v", res3.Versions[0])
	}
	if !res3.Versions[1].PublishedAt.IsZero() {
		t.Errorf("a known version was dated: %+v", res3.Versions[1])
	}
	// An unreadable cursor counts as a first scan.
	p.reset()
	if _, err := s.List(ctx, target(capMod), source.FetchState{Cursor: "{nope"}); err != nil || len(p.lookups()) != 1 {
		t.Errorf("unreadable cursor: requests %v, err %v", p.requests(), err)
	}
}

func TestLookupFailures(t *testing.T) {
	newVer := capPrefix + "v1.3.0.info"
	base := `{"versions":["v1.0.0","v1.0.0-RC1","v1.1.0","v1.2.0","v1.2.0-rc.1"]}`
	setup := func(t *testing.T) (*proxy, *httptest.Server) {
		p := libProxy(t)
		p.set(capPrefix+"list", string(fixture(t, "team-lib.list"))+"v1.3.0\n")
		srv := httptest.NewServer(p)
		t.Cleanup(srv.Close)
		return p, srv
	}
	t.Run("5xx leaves the version undated", func(t *testing.T) {
		p, srv := setup(t)
		p.fail(newVer, http.StatusInternalServerError)
		var buf bytes.Buffer
		res, err := newSource(t, srv, "", &buf).List(t.Context(), target(capMod), source.FetchState{Cursor: base})
		if err != nil {
			t.Fatal(err)
		}
		if res.Versions[0].Raw != "v1.3.0" || !res.Versions[0].PublishedAt.IsZero() {
			t.Errorf("v1.3.0 = %+v", res.Versions[0])
		}
		if !strings.Contains(buf.String(), "level=WARN") || strings.Contains(buf.String(), srv.URL) {
			t.Errorf("log = %q, want a WARN without the URL", buf.String())
		}
		if !strings.Contains(res.State.Cursor, "v1.3.0") {
			t.Errorf("cursor %q did not take the version", res.State.Cursor)
		}
	})
	t.Run("503 with Retry-After leaves the version undated", func(t *testing.T) {
		p, srv := setup(t)
		p.fail(newVer, http.StatusServiceUnavailable)
		var buf bytes.Buffer
		res, err := newSource(t, srv, "", &buf).List(t.Context(), target(capMod), source.FetchState{Cursor: base})
		if err != nil {
			t.Fatalf("a 5xx with Retry-After failed the call: %v", err)
		}
		if res.Versions[0].Raw != "v1.3.0" || !res.Versions[0].PublishedAt.IsZero() {
			t.Errorf("v1.3.0 = %+v", res.Versions[0])
		}
		if !strings.Contains(buf.String(), "level=WARN") {
			t.Errorf("log = %q, want a WARN", buf.String())
		}
	})
	t.Run("404 of .info leaves the version undated", func(t *testing.T) {
		_, srv := setup(t) // no v1.3.0.info document
		res, err := newSource(t, srv, "", nil).List(t.Context(), target(capMod), source.FetchState{Cursor: base})
		if err != nil || !res.Versions[0].PublishedAt.IsZero() {
			t.Fatalf("res %+v, err %v", res.Versions[0], err)
		}
	})
	t.Run("429 fails the call", func(t *testing.T) {
		p, srv := setup(t)
		p.fail(newVer, http.StatusTooManyRequests)
		res, err := newSource(t, srv, "", nil).List(t.Context(), target(capMod), source.FetchState{Cursor: base})
		var se *netguard.StatusError
		if !errors.As(err, &se) || se.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("err = %v, want a 429", err)
		}
		if res.State.Cursor != "" || len(res.Versions) != 0 {
			t.Errorf("the failed call returned state %+v", res)
		}
	})
	t.Run("cancelled context fails the call", func(t *testing.T) {
		_, srv := setup(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		s := goproxy.New(srv.URL, &http.Client{Transport: cancelOn(srv.Client().Transport, "v1.3.0.info", cancel)})
		_, err := s.List(ctx, target(capMod), source.FetchState{Cursor: base})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}

// cancelOn cancels the call when a request path ends with suffix, and then
// lets the request run (so it fails with the cancelled context).
func cancelOn(rt http.RoundTripper, suffix string, cancel func()) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, suffix) {
			cancel()
		}
		return rt.RoundTrip(r)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAtMost50Lookups(t *testing.T) {
	p := libProxy(t)
	var list strings.Builder
	for i := range 60 {
		fmt.Fprintf(&list, "v1.%d.0\n", i)
		p.set(fmt.Sprintf("%sv1.%d.0.info", capPrefix, i), fmt.Sprintf(`{"Time":"2026-01-%02dT00:00:00Z"}`, i%28+1))
	}
	p.set(capPrefix+"list", list.String())
	srv := httptest.NewServer(p)
	defer srv.Close()
	s := newSource(t, srv, "", nil)
	res, err := s.List(t.Context(), target(capMod), source.FetchState{Cursor: `{"versions":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(p.lookups()); got != 51 {
		t.Errorf("%d requests, want the list and 50 lookups", got)
	}
	dated := 0
	for _, v := range res.Versions {
		if !v.PublishedAt.IsZero() {
			dated++
		}
	}
	if dated != 50 {
		t.Errorf("%d dated versions, want 50", dated)
	}
	// The cursor holds all 60: the 10 left undated are not looked up again.
	p.reset()
	if _, err := s.List(t.Context(), target(capMod), res.State); err != nil || len(p.lookups()) != 1 {
		t.Errorf("second scan: %d requests, err %v", len(p.requests()), err)
	}
}

// URL is the pkg.go.dev link for the default base URL only.
func TestPageURL(t *testing.T) {
	p := libProxy(t)
	srv := httptest.NewServer(p)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	// A client that sends requests for proxy.golang.org to the fake server.
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "proxy.golang.org" {
			return nil, fmt.Errorf("unexpected host %s", r.URL.Host)
		}
		r2 := r.Clone(r.Context())
		r2.URL.Scheme, r2.URL.Host = u.Scheme, u.Host
		return http.DefaultTransport.RoundTrip(r2)
	})}
	res, err := goproxy.New("", hc).List(t.Context(), target(capMod), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Versions[0].URL; got != "https://pkg.go.dev/example.test/Team/Lib@v1.2.0" {
		t.Errorf("default URL = %q", got)
	}
	res, err = newSource(t, srv, "", nil).List(t.Context(), target(capMod), source.FetchState{})
	if err != nil || res.Versions[0].URL != "" {
		t.Errorf("instance URL = %q, err %v, want none", res.Versions[0].URL, err)
	}
}

// A base URL with a path (Artifactory, Nexus) builds requests under it.
func TestBaseURLWithPath(t *testing.T) {
	const prefix = "/artifactory/api/go/go-virtual"
	p := &proxy{codes: map[string]int{}, docs: map[string]string{prefix + capPrefix + "list": "v1.0.0\n"}}
	srv := httptest.NewServer(p)
	defer srv.Close()
	res, err := newSource(t, srv, prefix+"/", nil).List(t.Context(), target(capMod), source.FetchState{})
	if err != nil || len(res.Versions) != 1 {
		t.Fatalf("res %+v, err %v", res, err)
	}
	if got := p.lookups(); !slices.Equal(got, []string{prefix + capPrefix + "list"}) {
		t.Errorf("requests = %v", got)
	}
}

func TestEveryRequestUsesTheInjectedClient(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(libProxy(t))
	defer srv.Close()
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		n.Add(1)
		return srv.Client().Transport.RoundTrip(r)
	})}
	if _, err := goproxy.New(srv.URL, hc).List(t.Context(), target(capMod), source.FetchState{Cursor: `{"versions":["v1.0.0"]}`}); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 1+1+1+4 { // list, @latest, go.mod and four new versions
		t.Errorf("injected client saw %d requests, want 7", n.Load())
	}
}

func TestNoClient(t *testing.T) {
	if _, err := goproxy.New("", nil).List(t.Context(), target(capMod), source.FetchState{}); err == nil {
		t.Error("want an error without an HTTP client")
	}
	if _, err := goproxy.New("", &http.Client{}).List(t.Context(), target("bad"), source.FetchState{}); !errors.Is(err, source.ErrInvalidTarget) {
		t.Errorf("err = %v, want ErrInvalidTarget", err)
	}
}
