package pypi_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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

func target(name string) source.Target {
	return source.Target{Kind: "pypi", Coordinates: map[string]string{"name": name}}
}

// upstream serves the recorded fixtures in testdata/ as the PyPI JSON API,
// with an ETag that changes whenever rev is bumped, and records request paths.
type upstream struct {
	rev   atomic.Int64
	mu    sync.Mutex
	paths []string
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.paths = append(u.paths, r.URL.Path)
	u.mu.Unlock()
	name, ok := strings.CutPrefix(r.URL.Path, "/pypi/")
	name, ok2 := strings.CutSuffix(name, "/json")
	if !ok || !ok2 || strings.ContainsAny(name, "/._") {
		http.NotFound(w, r)
		return
	}
	body, err := fs.ReadFile(os.DirFS("testdata"), name+".json")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	etag := fmt.Sprintf(`"%s-%d"`, name, u.rev.Load())
	w.Header().Set("ETag", etag)
	w.Header().Set("Last-Modified", "Sat, 01 Nov 2025 10:00:00 GMT")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body) //nolint:gosec // G705: recorded JSON fixture.
}

func TestContract(t *testing.T) {
	t.Parallel()
	for _, pkg := range []string{"requests", "django", "boto3"} {
		t.Run(pkg, func(t *testing.T) {
			t.Parallel()
			up := &upstream{}
			sourcetest.Run(t, nil, sourcetest.Fixtures{
				Handler: up,
				New: func(baseURL string, c *http.Client) source.Source {
					return pypi.New(baseURL, c, pypi.WithLogger(slog.New(slog.DiscardHandler)))
				},
				Valid:   target(pkg),
				Invalid: []source.Target{{Kind: "pypi"}, target(""), target("a/b"), target("-x"), target("x y"), {Kind: "npm", Coordinates: map[string]string{"name": "requests"}}},
				Missing: func() *source.Target { m := target("does-not-exist"); return &m }(),

				Conditional:    true,
				Change:         func() { up.rev.Add(1) },
				HasPublishedAt: true,
			})
		})
	}
}

func list(t *testing.T, name string) source.Result {
	t.Helper()
	srv := httptest.NewServer(&upstream{})
	t.Cleanup(srv.Close)
	res, err := pypi.New(srv.URL, srv.Client()).List(context.Background(), target(name), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func byRaw(res source.Result) map[string]source.Version {
	m := map[string]source.Version{}
	for _, v := range res.Versions {
		m[v.Raw] = v
	}
	return m
}

func TestYanked(t *testing.T) {
	t.Parallel()
	vs := byRaw(list(t, "requests"))
	for _, raw := range []string{"2.32.0", "2.32.1"} {
		v, ok := vs[raw]
		if !ok || !v.Yanked {
			t.Errorf("requests %s: present=%v yanked=%v, want yanked", raw, ok, v.Yanked)
		}
	}
	if v := vs["2.31.0"]; v.Yanked {
		t.Error("requests 2.31.0 must not be yanked")
	}
	if _, ok := vs["2.32.0"].Metadata["yanked_reason"]; !ok {
		t.Error("yanked_reason missing in Metadata of a yanked release with a reason")
	}
	dj := byRaw(list(t, "django"))
	if !dj["4.2.12"].Yanked || !dj["5.0.5"].Yanked {
		t.Error("django 4.2.12 and 5.0.5 should be yanked")
	}
}

func TestPrereleasesListedWithNilFlag(t *testing.T) {
	t.Parallel()
	vs := byRaw(list(t, "django"))
	for _, raw := range []string{"5.0a1", "5.0b1", "5.0rc1"} {
		v, ok := vs[raw]
		if !ok {
			t.Errorf("django %s missing", raw)
			continue
		}
		if v.Prerelease != nil {
			t.Errorf("django %s: Prerelease = %v, PyPI has no flag so it must be nil", raw, *v.Prerelease)
		}
	}
}

func TestLargeAndOrder(t *testing.T) {
	t.Parallel()
	res := list(t, "boto3")
	if len(res.Versions) < 1500 {
		t.Fatalf("boto3: %d versions, want a large fixture", len(res.Versions))
	}
	// 0.0.15 has no files, so it is skipped.
	if _, ok := byRaw(res)["0.0.15"]; ok {
		t.Error("boto3 0.0.15 has no files and must be skipped")
	}
	req := list(t, "requests")
	if got := req.Versions[0].Raw; got != "2.34.2" {
		t.Errorf("newest requests version = %q, want 2.34.2", got)
	}
	if got := req.Versions[len(req.Versions)-1].Raw; got != "0.0.1" && got != "0.1.0" && got != "0.2.0" {
		t.Logf("oldest requests version = %q", got)
	}
	v := byRaw(req)["2.31.0"]
	if v.URL != "" || v.Metadata["project_url"] != nil {
		t.Errorf("mirror base: URL = %q, project_url = %v, want both left out", v.URL, v.Metadata["project_url"])
	}
}

func TestPublishedAtIsEarliestFile(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"releases":{
			"1.0":[{"upload_time_iso_8601":"2020-05-02T10:00:00.5Z","yanked":false},
			       {"upload_time_iso_8601":"2020-05-01T09:30:00.123456Z","yanked":true}],
			"1.1":[{"upload_time_iso_8601":"2021-01-01T00:00:00Z","yanked":true,"yanked_reason":"broken"},
			       {"upload_time_iso_8601":"2021-01-02T00:00:00Z","yanked":true}],
			"junk":[{"upload_time_iso_8601":"nope","yanked":false}]}}`))
	}))
	defer srv.Close()
	res, err := pypi.New(srv.URL, srv.Client()).List(context.Background(), target("x"), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	vs := byRaw(res)
	want := time.Date(2020, 5, 1, 9, 30, 0, 123456000, time.UTC)
	if !vs["1.0"].PublishedAt.Equal(want) {
		t.Errorf("1.0 PublishedAt = %v, want %v", vs["1.0"].PublishedAt, want)
	}
	if vs["1.0"].Yanked {
		t.Error("1.0 has an unyanked file, so the release is not yanked")
	}
	if !vs["1.1"].Yanked || vs["1.1"].Metadata["yanked_reason"] != "broken" {
		t.Errorf("1.1 = %+v, want yanked with reason", vs["1.1"])
	}
	if !vs["junk"].PublishedAt.IsZero() {
		t.Error("unparseable time must leave PublishedAt zero")
	}
	var order []string
	for _, v := range res.Versions {
		order = append(order, v.Raw)
	}
	if strings.Join(order, ",") != "1.1,1.0,junk" {
		t.Errorf("order = %v, want 1.1,1.0,junk (unparseable last)", order)
	}
}

func TestReleaseWithoutFilesSkippedAndLoggedAtDebug(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"releases":{"1.0":[],"2.0":[{"upload_time_iso_8601":"2020-01-01T00:00:00Z","yanked":false}]}}`))
	}))
	defer srv.Close()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	res, err := pypi.New(srv.URL, srv.Client(), pypi.WithLogger(log)).List(context.Background(), target("x"), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Versions) != 1 || res.Versions[0].Raw != "2.0" {
		t.Fatalf("versions = %+v, want only 2.0", res.Versions)
	}
	out := buf.String()
	if !strings.Contains(out, "level=DEBUG") || !strings.Contains(out, "version=1.0") {
		t.Errorf("log = %q, want a debug line for 1.0", out)
	}
	// And nothing above debug.
	buf.Reset()
	info := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if _, err := pypi.New(srv.URL, srv.Client(), pypi.WithLogger(info)).List(context.Background(), target("x"), source.FetchState{}); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("info-level log = %q, want empty", buf.String())
	}
}

func TestNameNormalization(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"Requests": "requests", "requests": "requests", "Foo_Bar": "foo-bar",
		"foo.bar": "foo-bar", "a-_.b": "a-b", "Django": "django", "ZOPE.interface": "zope-interface",
	} {
		if got := pypi.Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
	up := &upstream{}
	srv := httptest.NewServer(up)
	defer srv.Close()
	s := pypi.New(srv.URL, srv.Client())
	var raws [2][]string
	for i, name := range []string{"Requests", "requests"} {
		res, err := s.List(context.Background(), target(name), source.FetchState{})
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range res.Versions {
			raws[i] = append(raws[i], v.Raw)
		}
	}
	if len(up.paths) != 2 || up.paths[0] != "/pypi/requests/json" || up.paths[1] != "/pypi/requests/json" {
		t.Errorf("paths = %v, want the same normalized path twice", up.paths)
	}
	if strings.Join(raws[0], ",") != strings.Join(raws[1], ",") {
		t.Error("Requests and requests resolved to different versions")
	}
}

func TestConditionalRequestHeaders(t *testing.T) {
	t.Parallel()
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("ETag", `"new"`)
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()
	in := source.FetchState{ETag: `"old"`, LastModified: "Sat, 01 Nov 2025 10:00:00 GMT"}
	res, err := pypi.New(srv.URL, srv.Client()).List(context.Background(), target("x"), in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Get("If-None-Match") != `"old"` || got.Get("If-Modified-Since") != in.LastModified {
		t.Errorf("headers = %v", got)
	}
	if !res.NotModified || len(res.Versions) != 0 {
		t.Errorf("res = %+v, want NotModified", res)
	}
	if res.State.ETag != `"new"` || res.State.LastModified != in.LastModified {
		t.Errorf("state = %+v, want new ETag and kept Last-Modified", res.State)
	}
}

func TestErrors(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pypi/bad/json":
			_, _ = w.Write([]byte(`{not json`))
		case "/pypi/empty/json":
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s := pypi.New(srv.URL, srv.Client())
	for _, name := range []string{"bad", "empty", "missing"} {
		if _, err := s.List(context.Background(), target(name), source.FetchState{}); err == nil {
			t.Errorf("List(%q) succeeded, want an error", name)
		}
	}
	_, err := s.List(context.Background(), target("missing"), source.FetchState{})
	var se *netguard.StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusNotFound {
		t.Errorf("err = %v, want a wrapped *netguard.StatusError 404", err)
	}
	if _, err := s.List(context.Background(), target("a/b"), source.FetchState{}); !errors.Is(err, source.ErrInvalidTarget) {
		t.Errorf("List with invalid target: %v, want ErrInvalidTarget", err)
	}
}

func TestMirrorBaseURLWithTrailingSlashAndPath(t *testing.T) {
	t.Parallel()
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"releases":{"1.0":[{"upload_time_iso_8601":"2020-01-01T00:00:00Z"}]}}`))
	}))
	defer srv.Close()
	res, err := pypi.New(srv.URL+"/mirror/", srv.Client()).List(context.Background(), target("Foo_Bar"), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/mirror/pypi/foo-bar/json" {
		t.Errorf("path = %q", path)
	}
	if res.Versions[0].URL != "" {
		t.Errorf("URL = %q, want empty for a mirror", res.Versions[0].URL)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDefaultBaseKeepsPyPIURLs(t *testing.T) {
	t.Parallel()
	var host string
	c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		host = r.URL.Host
		rec := httptest.NewRecorder()
		_, _ = rec.WriteString(`{"releases":{"1.0":[{"upload_time_iso_8601":"2020-01-01T00:00:00Z"}]}}`)
		return rec.Result(), nil
	})}
	for _, base := range []string{pypi.DefaultBaseURL, "https://pypi.org/"} {
		res, err := pypi.New(base, c).List(context.Background(), target("Foo_Bar"), source.FetchState{})
		if err != nil {
			t.Fatal(err)
		}
		v := res.Versions[0]
		if v.URL != "https://pypi.org/project/foo-bar/1.0/" || v.Metadata["project_url"] != "https://pypi.org/project/foo-bar/" {
			t.Errorf("base %q: URL = %q, project_url = %v", base, v.URL, v.Metadata["project_url"])
		}
	}
	if host != "pypi.org" {
		t.Errorf("host = %q", host)
	}
}

func TestBaseCredentialsUsedForRequestsOnly(t *testing.T) {
	t.Parallel()
	var user, pass string
	var ok bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok = r.BasicAuth()
		_, _ = w.Write([]byte(`{"releases":{"1.0":[{"upload_time_iso_8601":"2020-01-01T00:00:00Z"}]}}`))
	}))
	defer srv.Close()
	base := strings.Replace(srv.URL, "http://", "http://user:s3cret@", 1) + "/m/"
	res, err := pypi.New(base, srv.Client()).List(context.Background(), target("x"), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if !ok || user != "user" || pass != "s3cret" {
		t.Errorf("basic auth = %q %q %v, want the base credentials on the request", user, pass, ok)
	}
	if dump := fmt.Sprintf("%+v", res); strings.Contains(dump, "s3cret") || strings.Contains(dump, "user:") {
		t.Errorf("credentials leaked into the result: %s", dump)
	}
}

func TestInvalidConfiguration(t *testing.T) {
	t.Parallel()
	hc := &http.Client{}
	cases := map[string]*pypi.Source{
		"ftp scheme":  pypi.New("ftp://mirror/", hc),
		"no scheme":   pypi.New("mirror/path", hc),
		"no host":     pypi.New("https:///x", hc),
		"query":       pypi.New("https://mirror/?a=b", hc),
		"empty query": pypi.New("https://mirror/?", hc),
		"fragment":    pypi.New("https://mirror/#x", hc),
		"empty":       pypi.New("", hc),
		"bad URL":     pypi.New("http://user:s3cret@[::1", hc),
		"nil client":  pypi.New(pypi.DefaultBaseURL, nil),
	}
	for name, s := range cases {
		_, err := s.List(context.Background(), target("x"), source.FetchState{})
		if !errors.Is(err, pypi.ErrInvalidBase) {
			t.Errorf("%s: err = %v, want ErrInvalidBase", name, err)
		}
		if err != nil && strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%s: error leaks credentials: %v", name, err)
		}
		if err := s.Validate(target("x")); err != nil {
			t.Errorf("%s: Validate must not depend on the base: %v", name, err)
		}
	}
}
