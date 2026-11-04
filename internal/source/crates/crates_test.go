package crates_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/crates"
	"github.com/angelia-dev/angelia/internal/source/sourcetest"
)

const (
	widgetIndexPath = "/an/ge/angelia-test-widget"
	apiPath         = "/api/v1/crates/angelia-test-widget/versions"
)

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // G304: fixed testdata names.
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// upstream is a fake sparse index plus crates.io API, on one server.
type upstream struct {
	mu       sync.Mutex
	docs     map[string][]byte // path to body
	requests []string          // paths, in order
	agents   []string
	apiCode  int // if non-zero, the status of every API request
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	body, ok := u.docs[r.URL.Path+"?"+r.URL.RawQuery] // a paginated page
	if !ok {
		body, ok = u.docs[r.URL.Path]
	}
	u.requests = append(u.requests, r.URL.Path)
	u.agents = append(u.agents, r.Header.Get("User-Agent"))
	code := u.apiCode
	u.mu.Unlock()
	isAPI := strings.HasPrefix(r.URL.Path, "/api/")
	if isAPI && code != 0 {
		http.Error(w, "boom", code)
		return
	}
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
	_, _ = w.Write(body)
}

func (u *upstream) set(path string, body []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.docs[path] = body
}

func (u *upstream) paths() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.requests...)
}

func (u *upstream) apiRequests() int {
	n := 0
	for _, p := range u.paths() {
		if strings.HasPrefix(p, "/api/") {
			n++
		}
	}
	return n
}

func newUpstream(t testing.TB) *upstream {
	return &upstream{docs: map[string][]byte{
		widgetIndexPath: fixture(t, "widget.index"),
		apiPath:         fixture(t, "widget.api.json"),
	}}
}

func target(name string) source.Target {
	return source.Target{Kind: crates.Kind, Coordinates: map[string]string{"name": name}}
}

// newSource returns a Source on a fake registry whose API is the same server.
func newSource(t *testing.T, up *upstream, opts ...crates.Option) (*crates.Source, *http.Client) {
	t.Helper()
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	c := sourcetest.Client(t, srv)
	return crates.New(srv.URL, c, append([]crates.Option{crates.WithAPIURL(srv.URL)}, opts...)...), c
}

func byRaw(vs []source.Version) map[string]source.Version {
	m := map[string]source.Version{}
	for _, v := range vs {
		m[v.Raw] = v
	}
	return m
}

func rawOf(vs []source.Version) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Raw
	}
	return out
}

func TestCanonical(t *testing.T) {
	s := crates.New(crates.DefaultIndexURL, &http.Client{})
	for in, want := range map[string]string{"Serde": "serde", "serde_json": "serde_json", "Serde-JSON": "serde-json"} {
		got, err := s.Canonical(source.Target{Kind: crates.Kind, Coordinates: map[string]string{"name": in, "extra": "Kept"}})
		if err != nil {
			t.Fatalf("Canonical(%q): %v", in, err)
		}
		if got.Kind != crates.Kind || got.Coordinates["name"] != want || got.Coordinates["extra"] != "Kept" {
			t.Errorf("Canonical(%q) = %v, want name %q and other coordinates kept", in, got, want)
		}
	}
	if _, err := s.Canonical(target("a/b")); !errors.Is(err, source.ErrInvalidTarget) {
		t.Errorf("Canonical of an invalid name: err = %v, want ErrInvalidTarget", err)
	}
}

func TestContract(t *testing.T) {
	up := newUpstream(t)
	missing := target("angelia-test-nope")
	sourcetest.Run(t, nil, sourcetest.Fixtures{
		Handler: up,
		New: func(baseURL string, hc *http.Client) source.Source {
			return crates.New(baseURL, hc, crates.WithAPIURL(baseURL))
		},
		Valid: target("angelia-test-widget"),
		Invalid: []source.Target{
			{Kind: crates.Kind},
			{Kind: crates.Kind, Coordinates: map[string]string{"name": ""}},
			{Kind: crates.Kind, Coordinates: map[string]string{"name": "has space"}},
			{Kind: crates.Kind, Coordinates: map[string]string{"name": "a/b"}},
			{Kind: crates.Kind, Coordinates: map[string]string{"name": "../etc"}},
			{Kind: crates.Kind, Coordinates: map[string]string{"name": "1abc"}},
			{Kind: crates.Kind, Coordinates: map[string]string{"name": "-abc"}},
			{Kind: crates.Kind, Coordinates: map[string]string{"name": "x?y=1"}},
			{Kind: crates.Kind, Coordinates: map[string]string{"name": "@scope/name"}},
			{Kind: crates.Kind, Coordinates: map[string]string{"name": " serde "}},
			{Kind: crates.Kind, Coordinates: map[string]string{"name": "serde\n"}},
			{Kind: crates.Kind, Coordinates: map[string]string{"name": strings.Repeat("a", 65)}},
			{Kind: "npm", Coordinates: map[string]string{"name": "serde"}},
		},
		Missing:        &missing,
		Aliases:        []source.Target{target("Angelia-Test-Widget"), target("ANGELIA-TEST-WIDGET")},
		WantRaw:        []string{"1.1.0-rc.1", "1.0.0", "0.2.0", "0.1.0"},
		Conditional:    true,
		HasPublishedAt: true,
		// A new yank flag changes the index document, like a new release.
		Change: func() { up.set(widgetIndexPath, fixture(t, "widget-after.index")) },
	})
}

func TestYankTransitions(t *testing.T) {
	up := newUpstream(t)
	s, _ := newSource(t, up)
	ctx := context.Background()
	tgt := target("angelia-test-widget")

	first, err := s.List(ctx, tgt, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	vs := byRaw(first.Versions)
	if !vs["0.2.0"].Yanked || vs["1.0.0"].Yanked || vs["0.1.0"].Yanked || vs["1.1.0-rc.1"].Yanked {
		t.Fatalf("initial yank flags wrong: %+v", vs)
	}

	// 1.0.0 is yanked and 0.2.0 is un-yanked, nothing else changes.
	up.set(widgetIndexPath, fixture(t, "widget-after.index"))
	second, err := s.List(ctx, tgt, first.State)
	if err != nil {
		t.Fatal(err)
	}
	if second.NotModified {
		t.Fatal("a yank must invalidate the fetch state")
	}
	if second.State.ETag == first.State.ETag {
		t.Error("ETag did not change")
	}
	vs = byRaw(second.Versions)
	if vs["0.2.0"].Yanked || !vs["1.0.0"].Yanked {
		t.Errorf("after: 0.2.0 yanked=%v (want false), 1.0.0 yanked=%v (want true)", vs["0.2.0"].Yanked, vs["1.0.0"].Yanked)
	}
	if len(second.Versions) != len(first.Versions) {
		t.Errorf("versions %d -> %d, a yank must not add or remove any", len(first.Versions), len(second.Versions))
	}

	// And it settles: the new state is a 304.
	third, err := s.List(ctx, tgt, second.State)
	if err != nil || !third.NotModified {
		t.Fatalf("third: NotModified=%v err=%v, want a 304", third.NotModified, err)
	}
}

func TestNotModifiedSkipsAPI(t *testing.T) {
	up := newUpstream(t)
	s, _ := newSource(t, up)
	res, err := s.List(context.Background(), target("angelia-test-widget"), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if got := up.apiRequests(); got != 1 {
		t.Fatalf("API requests after a full fetch = %d, want 1", got)
	}
	again, err := s.List(context.Background(), target("angelia-test-widget"), res.State)
	if err != nil || !again.NotModified {
		t.Fatalf("NotModified=%v err=%v", again.NotModified, err)
	}
	if got := up.apiRequests(); got != 1 {
		t.Errorf("API requests after a 304 = %d, want still 1: the API is only for created_at of a changed index", got)
	}
}

func TestIndexPaths(t *testing.T) {
	line := `{"name":"x","vers":"1.0.0","cksum":"aa","yanked":false}` + "\n"
	for _, c := range []struct{ name, path string }{
		{"a", "/1/a"},
		{"ab", "/2/ab"},
		{"abc", "/3/a/abc"},
		{"abcd", "/ab/cd/abcd"},
		{"serde_json", "/se/rd/serde_json"},
		{"Serde-JSON", "/se/rd/serde-json"}, // the index is lower case
	} {
		t.Run(c.name, func(t *testing.T) {
			up := &upstream{docs: map[string][]byte{
				c.path: []byte(line),
				"/api/v1/crates/" + strings.ToLower(c.name) + "/versions": []byte(`{"versions":[]}`),
			}}
			s, _ := newSource(t, up)
			if _, err := s.List(context.Background(), target(c.name), source.FetchState{}); err != nil {
				t.Fatalf("List: %v (requests %v)", err, up.paths())
			}
			if got := up.paths()[0]; got != c.path {
				t.Errorf("index path = %q, want %q", got, c.path)
			}
		})
	}
}

// The crate name as published, in its case, is kept for the event purl
// (#106); the index path and target key stay lower case.
func TestCrateNameAsPublished(t *testing.T) {
	up := &upstream{docs: map[string][]byte{
		"/in/fl/inflector": []byte(`{"name":"Inflector","vers":"0.11.4","cksum":"aa","yanked":false}` + "\n" +
			`{"vers":"0.11.5","cksum":"bb","yanked":false}` + "\n"),
		"/api/v1/crates/inflector/versions": []byte(`{"versions":[]}`),
	}}
	s, _ := newSource(t, up)
	res, err := s.List(context.Background(), target("Inflector"), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	vs := byRaw(res.Versions)
	if got := vs["0.11.4"].Metadata["crate_name"]; got != "Inflector" {
		t.Errorf("crate_name = %v, want Inflector", got)
	}
	if _, ok := vs["0.11.5"].Metadata["crate_name"]; ok {
		t.Error("crate_name set for an index line without a name")
	}
}

func TestVersionDetails(t *testing.T) {
	up := newUpstream(t)
	s, _ := newSource(t, up, crates.WithPageURL("https://pages.example/c/"))
	res, err := s.List(context.Background(), target("angelia-test-widget"), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	vs := byRaw(res.Versions)
	v := vs["1.0.0"]
	if want := time.Date(2024, 3, 12, 11, 0, 0, 500000000, time.UTC); !v.PublishedAt.Equal(want) {
		t.Errorf("PublishedAt = %v, want %v", v.PublishedAt, want)
	}
	if v.PublishedAt.Location() != time.UTC {
		t.Errorf("PublishedAt location = %v, want UTC", v.PublishedAt.Location())
	}
	if v.Prerelease != nil {
		t.Errorf("Prerelease = %v, want nil", *v.Prerelease)
	}
	if want := "https://pages.example/c/angelia-test-widget/1.0.0"; v.URL != want {
		t.Errorf("URL = %q, want %q", v.URL, want)
	}
	if v.Metadata["checksum"] != strings.Repeat("3", 64) || v.Metadata["rust_version"] != "1.70" || v.Metadata["license"] != "MIT OR Apache-2.0" {
		t.Errorf("Metadata = %v", v.Metadata)
	}
	old := vs["0.1.0"]
	if _, ok := old.Metadata["rust_version"]; ok {
		t.Errorf("0.1.0 has no rust_version, Metadata = %v", old.Metadata)
	}
	if _, ok := old.Metadata["license"]; ok {
		t.Errorf("0.1.0 has no license, Metadata = %v", old.Metadata)
	}
}

func TestNoPageURLForCustomIndex(t *testing.T) {
	s, _ := newSource(t, newUpstream(t))
	res, err := s.List(context.Background(), target("angelia-test-widget"), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Versions[0].URL != "" {
		t.Errorf("URL = %q, want empty for a non-default registry", res.Versions[0].URL)
	}
}

// recorder is a RoundTripper that answers from canned bodies by host and
// path and records "host path?query" of every request.
type recorder struct {
	mu   sync.Mutex
	got  []string
	docs map[string][]byte // "host/path" to body
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.got = append(r.got, req.URL.Host+req.URL.Path+"?"+req.URL.RawQuery)
	body, ok := r.docs[req.URL.Host+req.URL.Path]
	r.mu.Unlock()
	code := http.StatusOK
	if !ok {
		code = http.StatusNotFound
	}
	return &http.Response{StatusCode: code, Status: http.StatusText(code), Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
}

func (r *recorder) requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

func TestSeparateAPIURL(t *testing.T) {
	rec := &recorder{docs: map[string][]byte{
		"idx.example" + widgetIndexPath: fixture(t, "widget.index"),
		"api.example" + apiPath:         fixture(t, "widget.api.json"),
	}}
	s := crates.New("https://idx.example", &http.Client{Transport: rec}, crates.WithAPIURL("https://api.example/"))
	res, err := s.List(context.Background(), target("angelia-test-widget"), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Versions[0].PublishedAt.IsZero() {
		t.Error("created_at not read from the API host")
	}
	want := []string{"idx.example" + widgetIndexPath + "?", "api.example" + apiPath + "?per_page=100"}
	if got := rec.requests(); !slices.Equal(got, want) {
		t.Errorf("requests = %v, want %v", got, want)
	}
}

func TestPublicIndexUsesPublicAPI(t *testing.T) {
	rec := &recorder{docs: map[string][]byte{
		"index.crates.io" + widgetIndexPath: fixture(t, "widget.index"),
		"crates.io" + apiPath:               fixture(t, "widget.api.json"),
	}}
	res, err := crates.New("", &http.Client{Transport: rec}).List(context.Background(), target("angelia-test-widget"), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"index.crates.io" + widgetIndexPath + "?", "crates.io" + apiPath + "?per_page=100"}
	if got := rec.requests(); !slices.Equal(got, want) {
		t.Errorf("requests = %v, want %v", got, want)
	}
	if res.Versions[0].PublishedAt.IsZero() || res.Versions[0].URL == "" {
		t.Errorf("version incomplete: %+v", res.Versions[0])
	}
}

func TestCustomIndexWithoutAPI(t *testing.T) {
	up := newUpstream(t)
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	s := crates.New(srv.URL, sourcetest.Client(t, srv)) // no WithAPIURL
	res, err := s.List(context.Background(), target("angelia-test-widget"), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if n := up.apiRequests(); n != 0 {
		t.Errorf("API requests = %d, want none for a custom index without WithAPIURL", n)
	}
	if len(res.Versions) != 4 || !res.Versions[0].PublishedAt.IsZero() || res.State.ETag == "" {
		t.Errorf("res = %+v, want 4 versions with zero PublishedAt and an advanced state", res)
	}
	if got := rawOf(res.Versions); got[0] != "1.1.0-rc.1" || got[3] != "0.1.0" {
		t.Errorf("order = %v, want reverse index order", got)
	}
}

func TestAPIPagination(t *testing.T) {
	up := newUpstream(t)
	up.set(apiPath+"?per_page=100", []byte(`{"versions":[
		{"num":"1.1.0-rc.1","created_at":"2024-04-13T12:00:00Z"},
		{"num":"1.0.0","created_at":"2024-03-12T11:00:00Z","license":"MIT"}],
		"meta":{"total":4,"next_page":"?page=2&per_page=100"}}`))
	up.set(apiPath+"?page=2&per_page=100", []byte(`{"versions":[
		{"num":"0.2.0","created_at":"2024-02-11T10:30:00Z"},
		{"num":"0.1.0","created_at":"2024-01-10T09:00:00Z"}],
		"meta":{"total":4,"next_page":null}}`))
	s, _ := newSource(t, up)
	res, err := s.List(context.Background(), target("angelia-test-widget"), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if got := up.apiRequests(); got != 2 {
		t.Errorf("API requests = %d, want 2 pages", got)
	}
	if want := "1.1.0-rc.1,1.0.0,0.2.0,0.1.0"; strings.Join(rawOf(res.Versions), ",") != want {
		t.Errorf("order = %v, want %s", rawOf(res.Versions), want)
	}
	for _, v := range res.Versions {
		if v.PublishedAt.IsZero() {
			t.Errorf("%s has no PublishedAt", v.Raw)
		}
	}
	if res.State.ETag == "" {
		t.Error("state not advanced")
	}
}

func TestPaginationIsBounded(t *testing.T) {
	up := newUpstream(t)
	// Every page points at itself: List must stop, and not hold the state
	// back for versions the capped listing never reaches.
	up.set(apiPath+"?per_page=100", []byte(`{"versions":[{"num":"1.0.0","created_at":"2024-03-12T11:00:00Z"}],"meta":{"next_page":"?per_page=100"}}`))
	s, _ := newSource(t, up)
	res, err := s.List(context.Background(), target("angelia-test-widget"), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if got := up.apiRequests(); got < 2 || got > 100 {
		t.Errorf("API requests = %d, want a bounded number", got)
	}
	if res.State.ETag == "" {
		t.Error("state must advance when the listing was cut by the page cap")
	}
}

func TestAPILagKeepsState(t *testing.T) {
	up := newUpstream(t)
	// The API does not know the newest version yet.
	up.set(apiPath, []byte(`{"versions":[
		{"num":"1.0.0","created_at":"2024-03-12T11:00:00Z"},
		{"num":"0.2.0","created_at":"2024-02-11T10:30:00Z"},
		{"num":"0.1.0","created_at":"2024-01-10T09:00:00Z"}],"meta":{"next_page":null}}`))
	s, _ := newSource(t, up)
	ctx := context.Background()
	tgt := target("angelia-test-widget")
	first, err := s.List(ctx, tgt, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if first.State.ETag != "" || first.NotModified {
		t.Fatalf("state = %+v, want the incoming (empty) state so that the next poll refetches", first.State)
	}
	if !byRaw(first.Versions)["1.1.0-rc.1"].PublishedAt.IsZero() {
		t.Error("the unknown version must have a zero PublishedAt")
	}

	// The API catches up.
	up.set(apiPath, fixture(t, "widget.api.json"))
	second, err := s.List(ctx, tgt, first.State)
	if err != nil {
		t.Fatal(err)
	}
	if second.NotModified || second.State.ETag == "" {
		t.Fatalf("second = %+v, want a full fetch with an advanced state", second.State)
	}
	if byRaw(second.Versions)["1.1.0-rc.1"].PublishedAt.IsZero() {
		t.Error("PublishedAt not filled after the API caught up")
	}
}

func TestOversizedIndex(t *testing.T) {
	big := bytes.Repeat([]byte(`{"vers":"1.0.0","cksum":"aa","yanked":false}`+"\n"), 100)
	up := &upstream{docs: map[string][]byte{widgetIndexPath: big, apiPath: fixture(t, "widget.api.json")}}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	c, err := netguard.New(netguard.Config{MaxAttempts: 1, MaxBodyBytes: 1024, Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	if err != nil {
		t.Fatal(err)
	}
	st := source.FetchState{ETag: `"old"`}
	res, err := crates.New(srv.URL, c.HTTPClient(), crates.WithAPIURL(srv.URL)).List(context.Background(), target("angelia-test-widget"), st)
	if !errors.Is(err, netguard.ErrBodyTooLarge) || !strings.Contains(err.Error(), "size cap") {
		t.Fatalf("err = %v, want one wrapping ErrBodyTooLarge and mentioning the size cap", err)
	}
	if res.State != (source.FetchState{}) {
		t.Errorf("state = %+v, an error must not advance it", res.State)
	}
}

func TestNameNormalization(t *testing.T) {
	if got := crates.Normalize("Serde_JSON"); got != "serde_json" {
		t.Errorf("Normalize = %q, want lower case with _ kept", got)
	}
	if crates.Normalize("serde-json") == crates.Normalize("serde_json") {
		t.Error("- and _ must stay distinct")
	}
	up := newUpstream(t)
	s, _ := newSource(t, up)
	if _, err := s.List(context.Background(), target("Angelia-Test-Widget"), source.FetchState{}); err != nil {
		t.Fatal(err)
	}
}

func TestMissingCrateHint(t *testing.T) {
	s, _ := newSource(t, newUpstream(t))
	_, err := s.List(context.Background(), target("angelia_test_widget"), source.FetchState{})
	if err == nil || !strings.Contains(err.Error(), "'-' versus '_'") {
		t.Errorf("err = %v, want a hint about - versus _", err)
	}
}

func TestEqualTimesKeepReverseIndexOrder(t *testing.T) {
	up := newUpstream(t)
	up.set(apiPath, []byte(`{"versions":[
		{"num":"1.1.0-rc.1","created_at":"2024-04-13T12:00:00Z"},
		{"num":"1.0.0","created_at":"2024-04-13T12:00:00Z"},
		{"num":"0.2.0","created_at":"2024-04-13T12:00:00Z"},
		{"num":"0.1.0","created_at":"2024-01-10T09:00:00Z"}],"meta":{"next_page":null}}`))
	s, _ := newSource(t, up)
	res, err := s.List(context.Background(), target("angelia-test-widget"), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if want := "1.1.0-rc.1,1.0.0,0.2.0,0.1.0"; strings.Join(rawOf(res.Versions), ",") != want {
		t.Errorf("order = %v, want %s", rawOf(res.Versions), want)
	}
}

func TestOrderWithoutTimes(t *testing.T) {
	// The API knows only 0.1.0: the rest have no PublishedAt and follow it in
	// reverse index order.
	up := newUpstream(t)
	up.set(apiPath,
		[]byte(`{"versions":[{"num":"0.1.0","created_at":"2024-01-10T09:00:00+00:00"}]}`))
	s, _ := newSource(t, up)
	res, err := s.List(context.Background(), target("angelia-test-widget"), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"0.1.0", "1.1.0-rc.1", "1.0.0", "0.2.0"}
	if got := rawOf(res.Versions); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", got, want)
	}
	if !res.Versions[1].PublishedAt.IsZero() {
		t.Errorf("unknown time must stay zero, got %v", res.Versions[1].PublishedAt)
	}
}

func TestAPIFailureIsAnError(t *testing.T) {
	up := newUpstream(t)
	up.apiCode = http.StatusInternalServerError
	s, _ := newSource(t, up)
	_, err := s.List(context.Background(), target("angelia-test-widget"), source.FetchState{})
	var se *netguard.StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusInternalServerError {
		t.Fatalf("err = %v, want one wrapping *netguard.StatusError 500", err)
	}
}

func TestMissingCrateIs404(t *testing.T) {
	s, _ := newSource(t, newUpstream(t))
	_, err := s.List(context.Background(), target("angelia-test-nope"), source.FetchState{})
	var se *netguard.StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusNotFound {
		t.Fatalf("err = %v, want one wrapping *netguard.StatusError 404", err)
	}
}

func TestBadDocuments(t *testing.T) {
	for name, c := range map[string]struct {
		index, api string
	}{
		"index not JSON":     {"not json\n", `{"versions":[]}`},
		"index without vers": {`{"name":"x"}` + "\n", `{"versions":[]}`},
		"API not JSON":       {`{"vers":"1.0.0"}` + "\n", `<html>`},
	} {
		t.Run(name, func(t *testing.T) {
			up := &upstream{docs: map[string][]byte{widgetIndexPath: []byte(c.index), apiPath: []byte(c.api)}}
			s, _ := newSource(t, up)
			if _, err := s.List(context.Background(), target("angelia-test-widget"), source.FetchState{}); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestBlankLinesIgnored(t *testing.T) {
	up := &upstream{docs: map[string][]byte{
		widgetIndexPath: []byte("\n" + `{"vers":"1.0.0","cksum":"aa","yanked":false}` + "\n\n"),
		apiPath:         []byte(`{"versions":[]}`),
	}}
	s, _ := newSource(t, up)
	res, err := s.List(context.Background(), target("angelia-test-widget"), source.FetchState{})
	if err != nil || len(res.Versions) != 1 {
		t.Fatalf("res=%+v err=%v, want one version", res, err)
	}
}

func TestCursorIsKept(t *testing.T) {
	s, _ := newSource(t, newUpstream(t))
	res, err := s.List(context.Background(), target("angelia-test-widget"), source.FetchState{Cursor: "c1"})
	if err != nil || res.State.Cursor != "c1" {
		t.Fatalf("cursor = %q err=%v, want it kept", res.State.Cursor, err)
	}
}

func TestNilClient(t *testing.T) {
	_, err := crates.New("http://unused.invalid", nil).List(context.Background(), target("serde"), source.FetchState{})
	if err == nil || errors.Is(err, source.ErrInvalidTarget) {
		t.Errorf("err = %v, want a missing-client error", err)
	}
}

func TestDefaultURLs(t *testing.T) {
	// The default sources point at the public hosts; a request to one of them
	// is blocked by the guard, which proves the URL without touching the net.
	var hosts []string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hosts = append(hosts, r.URL.Host)
		return nil, errors.New("blocked")
	})
	_, _ = crates.New("", &http.Client{Transport: rt}).List(context.Background(), target("serde"), source.FetchState{})
	if len(hosts) != 1 || hosts[0] != "index.crates.io" {
		t.Errorf("hosts = %v, want index.crates.io", hosts)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestUserAgentIdentifiesAngelia checks the crawler-policy requirement that
// a request carries a User-Agent with a way to get in touch, using the
// default netguard client.
func TestUserAgentIdentifiesAngelia(t *testing.T) {
	up := newUpstream(t)
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	c, err := netguard.New(netguard.Config{MaxAttempts: 1, Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crates.New(srv.URL, c.HTTPClient(), crates.WithAPIURL(srv.URL)).List(context.Background(), target("angelia-test-widget"), source.FetchState{}); err != nil {
		t.Fatal(err)
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.agents) != 2 {
		t.Fatalf("got %d requests, want index and API", len(up.agents))
	}
	for _, ua := range up.agents {
		if !strings.HasPrefix(ua, "angelia/") || !strings.Contains(ua, "(+https://") {
			t.Errorf("User-Agent = %q, want product/version with a contact URL", ua)
		}
	}
}
