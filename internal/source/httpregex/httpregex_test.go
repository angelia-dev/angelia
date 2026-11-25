package httpregex_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/httpregex"
	"github.com/angelia-dev/angelia/internal/source/internal/generic"
	"github.com/angelia-dev/angelia/internal/source/sourcetest"
)

const (
	ph    = "{{server}}"
	regex = `href="/dl/tool-([0-9][^"]*)\.tar\.gz"`
)

func tgt(name, u, re string) source.Target {
	return source.Target{Kind: httpregex.Kind, Coordinates: map[string]string{"name": name, "url": u, "regex": re}}
}

// upstream serves testdata/releases.html at / with an ETag;
// each Change adds a release at the top.
func upstream(t *testing.T, rev *atomic.Int64) http.Handler {
	page, err := os.ReadFile("testdata/releases.html")
	if err != nil {
		t.Fatal(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		n := rev.Load()
		etag := fmt.Sprintf(`"r%d"`, n)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		body := string(page)
		for i := range n {
			body = strings.Replace(body, "<ul>", fmt.Sprintf(`<ul><li><a href="/dl/tool-3.%d.0.tar.gz">new</a></li>`, i), 1)
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(body))
	})
}

func TestContract(t *testing.T) {
	t.Parallel()
	var rev atomic.Int64
	sourcetest.Run(t, nil, sourcetest.Fixtures{
		Handler: upstream(t, &rev),
		New: func(_ string, c *http.Client) source.Source {
			return httpregex.New(c, httpregex.WithAllowHTTPHosts([]string{"127.0.0.1"}))
		},
		ServerURLPlaceholder: ph,
		Valid:                tgt("tool", ph+"/", regex),
		Invalid: []source.Target{
			{Kind: httpregex.Kind},
			{Kind: "http-json", Coordinates: tgt("tool", ph+"/releases.html", regex).Coordinates},
			tgt("", ph+"/releases.html", regex),
			tgt("to ol", ph+"/releases.html", regex),
			tgt("tool", "", regex),
			tgt("tool", "http://example.org/releases.html", regex),
			tgt("tool", ph+"/releases.html", ""),
			tgt("tool", ph+"/releases.html", "("),
			tgt("tool", ph+"/releases.html", `(a)(b)`),
			{Kind: httpregex.Kind, Coordinates: map[string]string{"name": "tool", "url": ph + "/releases.html", "regex": regex, "jsonpath": "$"}},
		},
		Missing:     ptr(tgt("tool", ph+"/missing.html", regex)),
		WantRaw:     []string{"2.1.0", "2.0.0", "2.0.0-rc.1", "1.9.3"},
		Conditional: true,
		Change:      func() { rev.Add(1) },
		// The server URL has no path: canonically "/", so both name "/".
		Aliases: []source.Target{tgt("tool", ph, regex)},
	})
}

func ptr[T any](v T) *T { return &v }

func serve(t *testing.T, h http.HandlerFunc) (*httptest.Server, *httpregex.Source) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, httpregex.New(srv.Client(), httpregex.WithAllowHTTPHosts([]string{"127.0.0.1"}))
}

func raws(vs []source.Version) string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Raw
	}
	return strings.Join(out, ",")
}

func TestGroups(t *testing.T) {
	srv, s := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("v1.0 v1.1 x-2.0 v1.0"))
	})
	cases := map[string]string{
		`v[0-9.]+`:                         "v1.0,v1.1",
		`v([0-9.]+)`:                       "1.0,1.1",
		`(v|x-)(?P<version>[0-9.]+)`:       "1.0,1.1,2.0",
		`(?P<prefix>v|x-)(?P<version>\d+)`: "1,2",
		`v(?:[0-9.]+)`:                     "v1.0,v1.1",
	}
	for re, want := range cases {
		res, err := s.List(context.Background(), tgt("tool", srv.URL+"/", re), source.FetchState{})
		if err != nil {
			t.Errorf("%s: %v", re, err)
			continue
		}
		if got := raws(res.Versions); got != want {
			t.Errorf("%s: %s, want %s", re, got, want)
		}
	}
}

func TestCompile(t *testing.T) {
	for _, bad := range []string{"", strings.Repeat("a", httpregex.MaxRegexBytes+1), "(a)(b)", "(?P<v>a)(b)", "a(?<!b)"} {
		if _, _, err := httpregex.Compile(bad); !errors.Is(err, source.ErrInvalidTarget) {
			t.Errorf("Compile(%q) = %v", bad, err)
		}
	}
	if _, g, err := httpregex.Compile(`(a)|(?P<version>b)`); err != nil || g != 2 {
		t.Errorf("named group: %d %v", g, err)
	}
	if _, _, err := httpregex.Compile(strings.Repeat("a", httpregex.MaxRegexBytes)); err != nil {
		t.Errorf("max length: %v", err)
	}
}

func TestOptionalGroupSkipped(t *testing.T) {
	srv, s := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("a1 b c3")) })
	res, err := s.List(context.Background(), tgt("tool", srv.URL, `[a-c]([0-9])?`), source.FetchState{})
	if err != nil || raws(res.Versions) != "1,3" {
		t.Errorf("%v %v", res, err)
	}
}

func TestZeroMatchesIsError(t *testing.T) {
	srv, s := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("nothing here")) })
	_, err := s.List(context.Background(), tgt("tool", srv.URL, `v\d+`), source.FetchState{})
	if !errors.Is(err, generic.ErrNoVersions) {
		t.Errorf("err = %v", err)
	}
}

func TestTooMany(t *testing.T) {
	var body strings.Builder
	for i := range generic.MaxVersions + 1 {
		fmt.Fprintf(&body, "v%d ", i)
	}
	srv, s := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body.String())) })
	if _, err := s.List(context.Background(), tgt("tool", srv.URL, `v\d+`), source.FetchState{}); !errors.Is(err, generic.ErrTooManyVersions) {
		t.Errorf("distinct: %v", err)
	}
	many := strings.Repeat("v1 ", generic.MaxMatches+1)
	srv2, s2 := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(many)) })
	if _, err := s2.List(context.Background(), tgt("tool", srv2.URL, `v\d+`), source.FetchState{}); !errors.Is(err, generic.ErrTooManyMatches) {
		t.Errorf("matches: %v", err)
	}
}

func TestSameVersionsWithoutValidatorsIsNotModified(t *testing.T) {
	var n atomic.Int64
	srv, s := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		// No ETag or Last-Modified; the page changes, the versions do not.
		_, _ = fmt.Fprintf(w, "<p>visit %d</p> v1.0 v2.0", n.Add(1))
	})
	tg := tgt("tool", srv.URL, `v[0-9.]+`)
	first, err := s.List(context.Background(), tg, source.FetchState{})
	if err != nil || first.NotModified || first.State.Cursor == "" {
		t.Fatalf("first: %+v %v", first, err)
	}
	second, err := s.List(context.Background(), tg, first.State)
	if err != nil || !second.NotModified || len(second.Versions) != 0 || second.State != first.State {
		t.Errorf("second: %+v %v", second, err)
	}
}

func TestSkippedLoggedAtDebug(t *testing.T) {
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("v1 v" + strings.Repeat("9", generic.MaxVersionBytes+1)))
	}))
	defer srv.Close()
	s := httpregex.New(srv.Client(), httpregex.WithAllowHTTPHosts([]string{"127.0.0.1"}), httpregex.WithLogger(log))
	res, err := s.List(context.Background(), tgt("tool", srv.URL, `v\d+`), source.FetchState{})
	if err != nil || raws(res.Versions) != "v1" || !strings.Contains(buf.String(), "skipped=1") {
		t.Errorf("%v %v %s", res, err, buf.String())
	}
}

func TestBodyCap(t *testing.T) {
	srv, s := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("v1 " + strings.Repeat("x", generic.MaxBodyBytes)))
	})
	if _, err := s.List(context.Background(), tgt("tool", srv.URL, `v\d+`), source.FetchState{}); !errors.Is(err, netguard.ErrBodyTooLarge) {
		t.Errorf("err = %v", err)
	}
}

func TestHTTPNeedsAllowList(t *testing.T) {
	s := httpregex.New(http.DefaultClient)
	if err := s.Validate(tgt("tool", "http://127.0.0.1/x", "v")); !errors.Is(err, source.ErrInvalidTarget) {
		t.Errorf("http without allow list: %v", err)
	}
	if err := s.Validate(tgt("tool", "https://127.0.0.1/x", "v")); err != nil {
		t.Errorf("https: %v", err)
	}
}

func TestCanonical(t *testing.T) {
	s := httpregex.New(nil)
	got, err := s.Canonical(tgt("Tool", "HTTPS://Example.ORG:443", "V(.)"))
	if err != nil || got.Coordinates["url"] != "https://example.org/" || got.Coordinates["name"] != "Tool" || got.Coordinates["regex"] != "V(.)" {
		t.Errorf("%v %v", got, err)
	}
	if _, err := s.Canonical(tgt("", "https://example.org", "v")); !errors.Is(err, source.ErrInvalidTarget) {
		t.Errorf("invalid: %v", err)
	}
}

// Review of #97: the match of a 2 MiB body cannot be cancelled, so the
// pattern's cost is bounded when the watch is written.
func TestCompileCost(t *testing.T) {
	for _, bad := range []string{
		`(a|aa|aaa){1,999}b`,
		`a{101}`,
		`(?:x{0,101})`,
		strings.Repeat("[a-z]{100}", 21), // counts at the cap, program above it
	} {
		_, _, err := httpregex.Compile(bad)
		if !errors.Is(err, source.ErrInvalidTarget) || (!strings.Contains(err.Error(), "repetition count") && !strings.Contains(err.Error(), "too complex")) {
			t.Errorf("Compile(%q) = %v, want a cost error", bad, err)
		}
	}
	for _, ok := range []string{
		regex,
		`v(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)`,
		`(?m)^Version:\s*(?P<version>[0-9]+(?:\.[0-9]+){1,3})$`,
		`tool-([0-9]{4}\.[0-9]{1,2}\.[0-9]{1,2})\.tar\.gz`,
		`"tag_name":\s*"(v?[0-9][^"]{0,100})"`,
		`a{100}`,
	} {
		if _, _, err := httpregex.Compile(ok); err != nil {
			t.Errorf("Compile(%q) = %v", ok, err)
		}
	}
}
