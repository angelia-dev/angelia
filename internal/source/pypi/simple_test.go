package pypi_test

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/pypi"
	"github.com/angelia-dev/angelia/internal/source/sourcetest"
)

const pep691 = "application/vnd.pypi.simple.v1+json"

// simpleServer serves the synthetic Simple API fixtures of testdata/simple/
// the way a repository would, and records the requests. json selects what it
// answers to a PEP 691 Accept: PEP 691 JSON, or PEP 503 HTML like pypiserver
// (which ignores Accept).
type simpleServer struct {
	json    bool
	ctype   string // overrides the Content-Type
	rev     atomic.Int64
	mu      sync.Mutex
	paths   []string
	accepts []string
}

func (u *simpleServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.paths = append(u.paths, r.URL.Path)
	u.accepts = append(u.accepts, r.Header.Get("Accept"))
	u.mu.Unlock()
	name, ok := strings.CutPrefix(r.URL.Path, "/simple/")
	name, ok2 := strings.CutSuffix(name, "/")
	if !ok || !ok2 || strings.ContainsAny(name, "/._") || strings.ToLower(name) != name {
		http.NotFound(w, r)
		return
	}
	ext, ctype := "html", "text/html; charset=utf-8"
	if u.json && strings.Contains(r.Header.Get("Accept"), pep691) {
		ext, ctype = "json", pep691
	}
	if u.ctype != "" {
		ctype = u.ctype
	}
	body, err := fs.ReadFile(os.DirFS("testdata/simple"), name+"."+ext)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	etag := `"` + name + "-" + ext + `-` + string(rune('a'+u.rev.Load())) + `"`
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", ctype)
	_, _ = w.Write(body) //nolint:gosec // G705: synthetic fixture.
}

func newSimple(base string, c *http.Client) source.Source {
	return pypi.New(base, c, pypi.WithAPI(pypi.APISimple), pypi.WithLogger(slog.New(slog.DiscardHandler)))
}

func listSimple(t *testing.T, up *simpleServer, name string) source.Result {
	t.Helper()
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	res, err := newSimple(srv.URL, srv.Client()).List(context.Background(), target(name), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

var wantRaw = []string{"2.0.0rc1", "1.4.0", "1.3.0", "1.2.0", "1.1.0", "1.0.0"}

func TestSimpleContract(t *testing.T) {
	t.Parallel()
	for name, json := range map[string]bool{"pep691-json": true, "pep503-html": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			up := &simpleServer{json: json}
			sourcetest.Run(t, nil, sourcetest.Fixtures{
				Handler: up,
				New:     newSimple,
				Valid:   target("example-pkg"),
				Invalid: []source.Target{{Kind: "pypi"}, target(""), target("a/b"), target("-x")},
				Missing: func() *source.Target { m := target("does-not-exist"); return &m }(),
				Aliases: []source.Target{target("Example_Pkg"), target(" EXAMPLE.pkg ")},
				WantRaw: wantRaw,

				Conditional:    true,
				Change:         func() { up.rev.Add(1) },
				HasPublishedAt: false, // some fixture files have no upload-time on purpose: see TestSimplePublishedAt
			})
		})
	}
}

// Sdists (.tar.gz, .zip) and wheels give the same versions in both formats,
// with the PEP 503 spellings of the name in the filenames, and the files of
// other projects and non-distribution links are ignored.
func TestSimpleVersionsFromFilenames(t *testing.T) {
	t.Parallel()
	for name, json := range map[string]bool{"json": true, "html": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res := listSimple(t, &simpleServer{json: json}, "example-pkg")
			var got []string
			for _, v := range res.Versions {
				got = append(got, v.Raw)
			}
			if !slices.Equal(got, wantRaw) {
				t.Fatalf("versions = %v, want %v", got, wantRaw)
			}
			vs := byRaw(res)
			if n := vs["1.2.0"].Metadata["files"]; n != 2 {
				t.Errorf("1.2.0 files = %v, want 2 (sdist and wheel)", n)
			}
			if vs["1.4.0"].Metadata["project"] != "example-pkg" {
				t.Errorf("project = %v", vs["1.4.0"].Metadata["project"])
			}
		})
	}
}

// PEP 592: a release is yanked when every file is, as on the JSON path.
func TestSimpleYanked(t *testing.T) {
	t.Parallel()
	for name, json := range map[string]bool{"json": true, "html": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			vs := byRaw(listSimple(t, &simpleServer{json: json}, "example-pkg"))
			if !vs["1.3.0"].Yanked {
				t.Error("1.3.0: every file is yanked, want Yanked")
			}
			if vs["1.3.0"].Metadata["yanked_reason"] == nil {
				t.Error("1.3.0: yanked_reason missing")
			}
			for _, raw := range []string{"1.0.0", "1.1.0", "1.2.0", "1.4.0", "2.0.0rc1"} {
				if vs[raw].Yanked {
					t.Errorf("%s must not be yanked (a file of it is not)", raw)
				}
			}
			if _, ok := vs["1.4.0"].Metadata["yanked_reason"]; ok {
				t.Error("a release that is not yanked carries no yanked_reason")
			}
		})
	}
	if r := byRaw(listSimple(t, &simpleServer{}, "example-pkg"))["1.3.0"].Metadata["yanked_reason"]; r != "broken & unusable" {
		t.Errorf("html yanked_reason = %q, want the unescaped attribute", r)
	}
	if r := byRaw(listSimple(t, &simpleServer{json: true}, "example-pkg"))["1.3.0"].Metadata["yanked_reason"]; r != "broken build" {
		t.Errorf("json yanked_reason = %q", r)
	}
}

// PEP 700 upload-time is the publish time (the earliest file's); without it
// the time is left unset, and HTML has none.
func TestSimplePublishedAt(t *testing.T) {
	t.Parallel()
	vs := byRaw(listSimple(t, &simpleServer{json: true}, "example-pkg"))
	want := map[string]time.Time{
		"1.0.0": time.Date(2025, 1, 10, 9, 0, 0, 123456000, time.UTC),
		"1.2.0": time.Date(2025, 3, 10, 9, 0, 0, 0, time.UTC),
	}
	for raw, w := range want {
		if got := vs[raw].PublishedAt; !got.Equal(w) {
			t.Errorf("%s PublishedAt = %v, want %v", raw, got, w)
		}
	}
	for _, raw := range []string{"1.4.0", "2.0.0rc1"} {
		if !vs[raw].PublishedAt.IsZero() {
			t.Errorf("%s has no upload-time, PublishedAt = %v, want zero", raw, vs[raw].PublishedAt)
		}
	}
	for _, v := range listSimple(t, &simpleServer{}, "example-pkg").Versions {
		if !v.PublishedAt.IsZero() {
			t.Errorf("html %s: PublishedAt = %v, want zero", v.Raw, v.PublishedAt)
		}
	}
}

// The request is GET {base}/simple/{normalized-name}/ with the PEP 691 Accept
// header; a server that ignores it and sends HTML is read as HTML.
func TestSimpleRequest(t *testing.T) {
	t.Parallel()
	up := &simpleServer{json: true}
	listSimple(t, up, "Example.Pkg")
	if !slices.Equal(up.paths, []string{"/simple/example-pkg/"}) {
		t.Errorf("paths = %v", up.paths)
	}
	if len(up.accepts) != 1 || !strings.HasPrefix(up.accepts[0], pep691) {
		t.Errorf("Accept = %v, want it to start with %s", up.accepts, pep691)
	}
	if !strings.Contains(up.accepts[0], "text/html") {
		t.Errorf("Accept = %q must accept HTML too", up.accepts[0])
	}
}

// A base path (a repository on a shared host) is kept.
func TestSimpleBasePath(t *testing.T) {
	t.Parallel()
	up := &simpleServer{}
	srv := httptest.NewServer(http.StripPrefix("/repo/pypi", up))
	t.Cleanup(srv.Close)
	res, err := newSimple(srv.URL+"/repo/pypi/", srv.Client()).List(context.Background(), target("example-pkg"), source.FetchState{})
	if err != nil || len(res.Versions) != len(wantRaw) {
		t.Fatalf("res = %d versions, err = %v", len(res.Versions), err)
	}
}

// The body is told apart by its Content-Type, and by its first byte when the
// server sends none usable.
func TestSimpleContentTypes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		json  bool
		ctype string
	}{
		{"latest json", true, "application/vnd.pypi.simple.latest+json"},
		{"plain json", true, "application/json; charset=utf-8"},
		{"pep691 html", false, "application/vnd.pypi.simple.v1+html"},
		{"octet-stream html", false, "application/octet-stream"},
		{"octet-stream json", true, "application/octet-stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := listSimple(t, &simpleServer{json: tc.json, ctype: tc.ctype}, "example-pkg")
			if len(res.Versions) != len(wantRaw) {
				t.Fatalf("%d versions, want %d", len(res.Versions), len(wantRaw))
			}
		})
	}
}

// A 404 is "package not found", the same error as the JSON path; there is
// no second request.
func TestSimpleNotFound(t *testing.T) {
	t.Parallel()
	up := &simpleServer{}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	_, err := newSimple(srv.URL, srv.Client()).List(context.Background(), target("nope"), source.FetchState{})
	var se *netguard.StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusNotFound {
		t.Fatalf("err = %v, want a 404 StatusError", err)
	}
	_, jerr := pypi.New(srv.URL, srv.Client()).List(context.Background(), target("nope"), source.FetchState{})
	if !errors.As(jerr, &se) || se.StatusCode != http.StatusNotFound || err.Error() != jerr.Error() {
		t.Errorf("json path error %q differs from simple path error %q", jerr, err)
	}
	if len(up.paths) != 2 {
		t.Errorf("paths = %v, want one request per List", up.paths)
	}
}

// api: json (the default) never touches /simple/.
func TestJSONAPIDoesNotUseSimple(t *testing.T) {
	t.Parallel()
	up := &simpleServer{}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	for _, src := range []source.Source{
		pypi.New(srv.URL, srv.Client()),
		pypi.New(srv.URL, srv.Client(), pypi.WithAPI(pypi.APIJSON)),
		pypi.New(srv.URL, srv.Client(), pypi.WithAPI("")),
	} {
		_, _ = src.List(context.Background(), target("example-pkg"), source.FetchState{})
	}
	for _, p := range up.paths {
		if strings.HasPrefix(p, "/simple/") {
			t.Errorf("json API requested %s", p)
		}
	}
}

func TestWithAPIRejectsOtherValues(t *testing.T) {
	t.Parallel()
	_, err := pypi.New("https://example.com", http.DefaultClient, pypi.WithAPI("xmlrpc")).List(context.Background(), target("x"), source.FetchState{})
	if !errors.Is(err, pypi.ErrInvalidBase) {
		t.Fatalf("err = %v, want ErrInvalidBase", err)
	}
}

// The PEP 691 "versions" key lists versions whose files the page may not
// carry; a version with no file is listed, not yanked.
func TestSimpleVersionsKey(t *testing.T) {
	t.Parallel()
	body := `{"name":"example-pkg","versions":["1.0","0.9"],"files":[{"filename":"example_pkg-1.0.tar.gz","url":"x","hashes":{}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", pep691)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	res, err := newSimple(srv.URL, srv.Client()).List(context.Background(), target("example-pkg"), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	vs := byRaw(res)
	if len(vs) != 2 || vs["0.9"].Yanked || vs["0.9"].Metadata["files"] != 0 || vs["1.0"].Metadata["files"] != 1 {
		t.Fatalf("versions = %+v", res.Versions)
	}
}

func TestSimpleMalformed(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ ctype, body string }{
		"bad json":       {pep691, `{"files": [`},
		"json no files":  {pep691, `{"name":"example-pkg"}`},
		"unclosed quote": {"text/html", `<a href="x>example_pkg-1.0.tar.gz</a>`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.ctype)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			res, err := newSimple(srv.URL, srv.Client()).List(context.Background(), target("example-pkg"), source.FetchState{})
			if tc.ctype == "text/html" {
				if err != nil || len(res.Versions) != 0 {
					t.Fatalf("html: res = %v, err = %v; want an empty list", res.Versions, err)
				}
				return
			}
			if err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

// Subscriber-facing URLs follow the JSON path: none for a mirror.
func TestSimpleNoPublicURLForMirror(t *testing.T) {
	t.Parallel()
	for _, v := range listSimple(t, &simpleServer{json: true}, "example-pkg").Versions {
		if v.URL != "" || v.Metadata["project_url"] != nil {
			t.Fatalf("%s: URL = %q, project_url = %v", v.Raw, v.URL, v.Metadata["project_url"])
		}
	}
}
