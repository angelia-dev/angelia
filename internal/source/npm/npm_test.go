package npm_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/npm"
	"github.com/angelia-dev/angelia/internal/source/sourcetest"
)

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // G304: fixed testdata names.
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// manyVersions builds a packument with n versions 1.0.0 ... 1.(n-1).0, one
// per hour.
func manyVersions(n int) []byte {
	versions := map[string]any{}
	times := map[string]string{"created": "2020-01-01T00:00:00.000Z", "modified": "2020-01-01T00:00:00.000Z"}
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range n {
		v := fmt.Sprintf("1.%d.0", i)
		versions[v] = map[string]any{"version": v, "dist": map[string]string{"tarball": "https://registry.example/many/-/many-" + v + ".tgz"}}
		times[v] = base.Add(time.Duration(i) * time.Hour).Format("2006-01-02T15:04:05.000Z")
	}
	b, _ := json.Marshal(map[string]any{
		"name":      "angelia-test-many",
		"dist-tags": map[string]string{"latest": fmt.Sprintf("1.%d.0", n-1)},
		"versions":  versions,
		"time":      times,
	})
	return b
}

// upstream is a fake registry: path (as sent on the wire) to packument.
type upstream struct {
	mu      sync.Mutex
	docs    map[string][]byte
	accepts []string
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	body, ok := u.docs[r.URL.EscapedPath()]
	u.accepts = append(u.accepts, r.Header.Get("Accept"))
	u.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (u *upstream) set(path string, body []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.docs[path] = body
}

func (u *upstream) lastAccept() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.accepts[len(u.accepts)-1]
}

func newUpstream(t testing.TB) *upstream {
	return &upstream{docs: map[string][]byte{
		"/@angelia-test%2Fwidget": fixture(t, "scoped.json"),
		"/angelia-test-gone":      fixture(t, "unpublished.json"),
		"/angelia-test-many":      manyVersions(600),
		"/angelia-test-abbr":      fixture(t, "abbreviated.json"),
	}}
}

func target(name string) source.Target {
	return source.Target{Kind: npm.Kind, Coordinates: map[string]string{"name": name}}
}

// invalid is shared by every contract run.
var invalid = []source.Target{
	{Kind: npm.Kind},
	{Kind: npm.Kind, Coordinates: map[string]string{"name": ""}},
	{Kind: npm.Kind, Coordinates: map[string]string{"name": "has space"}},
	{Kind: npm.Kind, Coordinates: map[string]string{"name": "@scope"}},
	{Kind: npm.Kind, Coordinates: map[string]string{"name": "@scope/"}},
	{Kind: npm.Kind, Coordinates: map[string]string{"name": "a/b"}},
	{Kind: npm.Kind, Coordinates: map[string]string{"name": "@a/b/c"}},
	{Kind: npm.Kind, Coordinates: map[string]string{"name": "_hidden"}},
	{Kind: npm.Kind, Coordinates: map[string]string{"name": ".dot"}},
	{Kind: npm.Kind, Coordinates: map[string]string{"name": "x?y=1"}},
	{Kind: npm.Kind, Coordinates: map[string]string{"name": strings.Repeat("a", 215)}},
	{Kind: "pypi", Coordinates: map[string]string{"name": "requests"}},
}

func TestContract(t *testing.T) {
	missing := target("angelia-test-nope")
	cases := []struct {
		name string
		pkg  string
		want []string
		opts []npm.Option
		// change adds a version to pkg's document.
		path string
	}{
		{"scoped", "@angelia-test/widget", []string{"2.0.0-beta.1", "1.2.0", "1.1.0", "1.0.0"}, nil, "/@angelia-test%2Fwidget"},
		{"unpublished", "angelia-test-gone", []string{"1.0.1", "1.0.2", "1.0.0"}, nil, "/angelia-test-gone"},
		{"many", "angelia-test-many", nil, nil, "/angelia-test-many"},
		{"abbreviated", "angelia-test-abbr", nil, []npm.Option{npm.WithAbbreviated()}, "/angelia-test-abbr"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := newUpstream(t)
			sourcetest.Run(t, nil, sourcetest.Fixtures{
				Handler: up,
				New: func(baseURL string, hc *http.Client) source.Source {
					return npm.New(baseURL, hc, c.opts...)
				},
				Valid:          target(c.pkg),
				Invalid:        invalid,
				Missing:        &missing,
				WantRaw:        c.want,
				Conditional:    true,
				HasPublishedAt: len(c.opts) == 0,
				Change: func() {
					var doc map[string]any
					if err := json.Unmarshal(up.docs[c.path], &doc); err != nil {
						t.Error(err)
						return
					}
					doc["versions"].(map[string]any)["9.9.9"] = map[string]any{"version": "9.9.9"}
					if tm, ok := doc["time"].(map[string]any); ok {
						tm["9.9.9"] = "2030-01-01T00:00:00.000Z"
					}
					b, _ := json.Marshal(doc)
					up.set(c.path, b)
				},
			})
		})
	}
}

// listOne wires a Source to a fresh fake upstream and lists pkg.
func listOne(t *testing.T, pkg string, opts ...npm.Option) (source.Result, *upstream) {
	t.Helper()
	up := newUpstream(t)
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	s := npm.New(srv.URL, sourcetest.Client(t, srv), opts...)
	res, err := s.List(context.Background(), target(pkg), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	return res, up
}

func byRaw(vs []source.Version) map[string]source.Version {
	m := map[string]source.Version{}
	for _, v := range vs {
		m[v.Raw] = v
	}
	return m
}

func TestScopedPackageURL(t *testing.T) {
	// The upstream only knows the path with the %2F, so a hit proves the
	// scoped name is URL-encoded as npm clients do.
	res, _ := listOne(t, "@angelia-test/widget")
	if len(res.Versions) != 4 {
		t.Fatalf("got %d versions, want 4", len(res.Versions))
	}
}

func TestManyVersions(t *testing.T) {
	res, _ := listOne(t, "angelia-test-many")
	if len(res.Versions) != 600 {
		t.Fatalf("got %d versions, want 600", len(res.Versions))
	}
	if first, last := res.Versions[0].Raw, res.Versions[599].Raw; first != "1.599.0" || last != "1.0.0" {
		t.Errorf("order: first %s last %s, want newest first", first, last)
	}
}

func TestFullMetadata(t *testing.T) {
	res, up := listOne(t, "@angelia-test/widget")
	if got := up.lastAccept(); got != "application/json" {
		t.Errorf("Accept = %q, want the full document", got)
	}
	vs := byRaw(res.Versions)
	wantAt := time.Date(2024, 4, 20, 8, 15, 30, 123000000, time.UTC)
	v := vs["1.2.0"]
	if !v.PublishedAt.Equal(wantAt) {
		t.Errorf("PublishedAt = %v, want %v", v.PublishedAt, wantAt)
	}
	if v.Yanked || v.Prerelease != nil {
		t.Errorf("Yanked=%v Prerelease=%v, want false and nil", v.Yanked, v.Prerelease)
	}
	if got := v.Metadata["dist-tags"]; fmt.Sprint(got) != "[latest]" {
		t.Errorf("dist-tags = %v, want [latest]", got)
	}
	if v.Metadata["tarball"] == "" || v.Metadata["integrity"] != "sha512-cccc" || v.Metadata["shasum"] != "cccc" {
		t.Errorf("dist metadata = %v", v.Metadata)
	}
	if _, ok := v.Metadata["deprecated"]; ok {
		t.Error("non-deprecated version has deprecated metadata")
	}
	if v.URL != "" {
		t.Errorf("URL = %q, want empty for a non-default registry", v.URL)
	}
	if got := vs["2.0.0-beta.1"].Metadata["dist-tags"]; fmt.Sprint(got) != "[next]" {
		t.Errorf("beta dist-tags = %v, want [next]", got)
	}
}

func TestDeprecated(t *testing.T) {
	res, _ := listOne(t, "@angelia-test/widget")
	v := byRaw(res.Versions)["1.1.0"]
	if got := v.Metadata["deprecated"]; got != "use 1.2.0, this has a memory leak" {
		t.Errorf("deprecated = %v", got)
	}
	if v.Yanked {
		t.Error("a deprecated version must not be Yanked")
	}
}

func TestUnpublishedIsYanked(t *testing.T) {
	res, _ := listOne(t, "angelia-test-gone")
	vs := byRaw(res.Versions)
	gone := vs["1.0.2"]
	if !gone.Yanked || gone.Metadata["unpublished"] != true {
		t.Errorf("1.0.2: Yanked=%v Metadata=%v, want yanked and unpublished", gone.Yanked, gone.Metadata)
	}
	if gone.PublishedAt.IsZero() {
		t.Error("an unpublished version keeps its publication time")
	}
	for _, raw := range []string{"1.0.0", "1.0.1"} {
		if vs[raw].Yanked {
			t.Errorf("%s is published but Yanked", raw)
		}
	}
	for _, k := range []string{"created", "modified"} {
		if _, ok := vs[k]; ok {
			t.Errorf("time key %q leaked as a version", k)
		}
	}
}

func TestFullyUnpublishedPackage(t *testing.T) {
	up := &upstream{docs: map[string][]byte{"/gone": []byte(
		`{"name":"gone","time":{"created":"2020-01-01T00:00:00.000Z","modified":"2020-01-02T00:00:00.000Z","unpublished":{"time":"2020-01-02T00:00:00.000Z","versions":["1.0.0"]}}}`)}}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	res, err := npm.New(srv.URL, sourcetest.Client(t, srv)).List(context.Background(), target("gone"), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Versions) != 0 {
		t.Errorf("versions = %v, want none", res.Versions)
	}
}

func TestAbbreviatedVsFull(t *testing.T) {
	res, up := listOne(t, "angelia-test-abbr", npm.WithAbbreviated())
	if got := up.lastAccept(); got != "application/vnd.npm.install-v1+json" {
		t.Errorf("Accept = %q, want the abbreviated media type", got)
	}
	vs := byRaw(res.Versions)
	if len(vs) != 2 {
		t.Fatalf("got %d versions, want 2", len(vs))
	}
	for raw, v := range vs {
		if !v.PublishedAt.IsZero() {
			t.Errorf("%s: abbreviated metadata has no time, PublishedAt = %v", raw, v.PublishedAt)
		}
	}
	if vs["0.2.0"].Metadata["deprecated"] != "old" || fmt.Sprint(vs["0.2.0"].Metadata["dist-tags"]) != "[latest]" {
		t.Errorf("0.2.0 metadata = %v", vs["0.2.0"].Metadata)
	}
	// Default is the full document, because PublishedAt and unpublished
	// detection need "time".
	_, up = listOne(t, "angelia-test-abbr")
	if got := up.lastAccept(); got != "application/json" {
		t.Errorf("default Accept = %q, want the full document", got)
	}
}

func TestDeprecatedBooleanAndEmpty(t *testing.T) {
	doc := `{"versions":{"1.0.0":{"deprecated":true},"1.0.1":{"deprecated":""},"1.0.2":{"deprecated":false}}}`
	up := &upstream{docs: map[string][]byte{"/legacy": []byte(doc)}}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	res, err := npm.New(srv.URL, sourcetest.Client(t, srv)).List(context.Background(), target("legacy"), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	vs := byRaw(res.Versions)
	if vs["1.0.0"].Metadata["deprecated"] != "deprecated" {
		t.Errorf("true: %v", vs["1.0.0"].Metadata)
	}
	for _, raw := range []string{"1.0.1", "1.0.2"} {
		if _, ok := vs[raw].Metadata["deprecated"]; ok {
			t.Errorf("%s must not be deprecated: %v", raw, vs[raw].Metadata)
		}
	}
}

func TestPackageURLs(t *testing.T) {
	up := newUpstream(t)
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	s := npm.New(srv.URL, sourcetest.Client(t, srv), npm.WithPackageURL("https://pkgs.example/p/"))
	res, err := s.List(context.Background(), target("@angelia-test/widget"), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := byRaw(res.Versions)["1.0.0"].URL, "https://pkgs.example/p/@angelia-test/widget/v/1.0.0"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	if got := npm.New("", nil); got == nil {
		t.Fatal("New with an empty base")
	}
}

func TestFetchStateMapping(t *testing.T) {
	var gotINM, gotIMS string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotINM, gotIMS = r.Header.Get("If-None-Match"), r.Header.Get("If-Modified-Since")
		w.Header().Set("ETag", `"e2"`)
		w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
		w.WriteHeader(http.StatusNotModified)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	in := source.FetchState{ETag: `"e1"`, LastModified: "Sun, 01 Jan 2006 00:00:00 GMT", Cursor: "keep"}
	res, err := npm.New(srv.URL, sourcetest.Client(t, srv)).List(context.Background(), target("x"), in)
	if err != nil {
		t.Fatal(err)
	}
	if gotINM != `"e1"` || gotIMS != in.LastModified {
		t.Errorf("sent If-None-Match=%q If-Modified-Since=%q", gotINM, gotIMS)
	}
	want := source.FetchState{ETag: `"e2"`, LastModified: "Mon, 02 Jan 2006 15:04:05 GMT", Cursor: "keep"}
	if !res.NotModified || res.State != want {
		t.Errorf("result = %+v, want NotModified with state %+v", res, want)
	}
}

func TestErrors(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"404":         http.NotFound,
		"500":         func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotImplemented) },
		"invalidJSON": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) },
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			t.Cleanup(srv.Close)
			_, err := npm.New(srv.URL, sourcetest.Client(t, srv)).List(context.Background(), target("x"), source.FetchState{})
			if err == nil {
				t.Fatal("want an error")
			}
			var se *netguard.StatusError
			if name != "invalidJSON" && !errors.As(err, &se) {
				t.Errorf("error %v does not wrap *netguard.StatusError", err)
			}
		})
	}
}

func TestListRejectsInvalidTarget(t *testing.T) {
	_, err := npm.New("http://unused.invalid", nil).List(context.Background(), source.Target{Kind: npm.Kind}, source.FetchState{})
	if !errors.Is(err, source.ErrInvalidTarget) {
		t.Errorf("err = %v, want ErrInvalidTarget", err)
	}
}

func TestValidateAcceptsNames(t *testing.T) {
	s := npm.New("", nil)
	for _, n := range []string{"react", "@types/node", "JSONStream", "lodash.merge", "a-b_c", " react "} {
		if err := s.Validate(target(n)); err != nil {
			t.Errorf("Validate(%q): %v", n, err)
		}
	}
}

func TestFixturesAreValidJSON(t *testing.T) {
	for _, f := range []string{"scoped.json", "unpublished.json", "abbreviated.json"} {
		if !json.Valid(fixture(t, f)) {
			t.Errorf("%s: invalid JSON", f)
		}
	}
}
