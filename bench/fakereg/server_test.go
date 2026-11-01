package fakereg

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/bench/wasm"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/githubrelease"
	"github.com/angelia-dev/angelia/internal/source/npm"
	"github.com/angelia-dev/angelia/internal/source/pypi"
)

// fastConfig has no latency and no releases, so tests are quick and
// deterministic unless they change it.
func fastConfig(edit func(*Behaviour)) *Config {
	c := DefaultConfig()
	for name, b := range c.Ecosystems {
		b.Packages, b.History, b.ReleasesPerHour = 10, 3, 0
		b.Latency, b.LatencyJitter = 0, 0
		if edit != nil {
			edit(&b)
		}
		c.Ecosystems[name] = b
	}
	return c
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func get(t *testing.T, h http.Handler, path string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// The fake registry must be readable by the real sources: each one lists
// the history and then gets NotModified on the conditional follow-up.
func TestRealSourcesReadFakeRegistry(t *testing.T) {
	srv := httptest.NewServer(New(fastConfig(nil)))
	defer srv.Close()
	hc := srv.Client()
	for _, tc := range []struct {
		src    source.Source
		target source.Target
		first  string
	}{
		{pypi.New(srv.URL+"/pypi", hc), source.Target{Kind: "pypi", Coordinates: map[string]string{"name": "pkg-00001"}}, "1.2.0"},
		{npm.New(srv.URL+"/npm", hc), source.Target{Kind: "npm", Coordinates: map[string]string{"name": "pkg-00001"}}, ""},
		{githubrelease.New(srv.URL+"/github", hc), source.Target{Kind: "github-release", Coordinates: map[string]string{"repo": "bench/pkg-00001"}}, "v1.2.0"},
		{wasm.NewNativeCrates(srv.URL+"/crates", hc), source.Target{Kind: "crates", Coordinates: map[string]string{"name": "pkg-00001"}}, "1.2.0"},
	} {
		t.Run(tc.src.Kind(), func(t *testing.T) {
			ctx := context.Background()
			res, err := tc.src.List(ctx, tc.target, source.FetchState{})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Versions) != 3 {
				t.Fatalf("got %d versions, want 3: %+v", len(res.Versions), res.Versions)
			}
			if tc.first != "" && res.Versions[0].Raw != tc.first {
				t.Errorf("newest = %q, want %q", res.Versions[0].Raw, tc.first)
			}
			if res.Versions[0].PublishedAt.IsZero() {
				t.Error("PublishedAt not parsed")
			}
			if tc.src.Kind() == "crates" {
				return // the crates port does not send validators
			}
			again, err := tc.src.List(ctx, tc.target, res.State)
			if err != nil {
				t.Fatal(err)
			}
			if !again.NotModified {
				t.Errorf("second List with state %+v: want NotModified", res.State)
			}
		})
	}
}

func TestReleasesAppearAndLagIsMeasured(t *testing.T) {
	clk := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	// 10 packages, 36000/h: each package releases every second.
	s := New(fastConfig(func(b *Behaviour) { b.ReleasesPerHour = 36000 }), WithClock(clk.now))
	clk.add(5 * time.Second)

	w := get(t, s, "/npm/pkg-00000")
	var doc struct {
		Versions map[string]any `json:"versions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	// Phase is in [0,1s), so after 5s package 0 has 5 new releases.
	if len(doc.Versions) != 3+5 {
		t.Fatalf("got %d versions after 5s, want 8", len(doc.Versions))
	}
	st := s.Stats().Ecosystems["npm"]
	if st.Seen != 5 || st.Published != 50 {
		t.Errorf("seen=%d published=%d, want 5 and 50", st.Seen, st.Published)
	}
	if st.Lag.Samples != 50 || st.Lag.MaxSeconds > 5 || st.Lag.P95Seconds <= 0 {
		t.Errorf("lag = %+v", st.Lag)
	}
	// Serving the same versions again records no new lag.
	get(t, s, "/npm/pkg-00000")
	if got := s.Stats().Ecosystems["npm"].Seen; got != 5 {
		t.Errorf("seen after repeat = %d, want 5", got)
	}
	if s.Stats().Total.Published != 250 {
		t.Errorf("total published = %d, want 250", s.Stats().Total.Published)
	}
}

func TestETagModes(t *testing.T) {
	for _, mode := range []string{ETagStrong, ETagWeak, ETagNone, ETagIgnore} {
		t.Run(mode, func(t *testing.T) {
			s := New(fastConfig(func(b *Behaviour) { b.ETag = mode; b.LastModified = false }))
			w := get(t, s, "/pypi/pypi/pkg-00002/json")
			etag := w.Header().Get("ETag")
			switch mode {
			case ETagNone:
				if etag != "" {
					t.Fatalf("ETag %q sent in mode none", etag)
				}
				etag = `"x"`
			case ETagWeak:
				if !strings.HasPrefix(etag, `W/"`) {
					t.Fatalf("ETag %q, want weak", etag)
				}
			default:
				if !strings.HasPrefix(etag, `"`) {
					t.Fatalf("ETag %q, want strong", etag)
				}
			}
			want := map[string]int{ETagStrong: 304, ETagWeak: 304, ETagNone: 200, ETagIgnore: 200}[mode]
			if got := get(t, s, "/pypi/pypi/pkg-00002/json", "If-None-Match", etag).Code; got != want {
				t.Errorf("conditional GET = %d, want %d", got, want)
			}
		})
	}
}

func TestLastModified(t *testing.T) {
	s := New(fastConfig(nil))
	w := get(t, s, "/crates/api/v1/crates/pkg-00003/versions")
	lm := w.Header().Get("Last-Modified")
	if lm == "" {
		t.Fatal("no Last-Modified")
	}
	if got := get(t, s, "/crates/api/v1/crates/pkg-00003/versions", "If-Modified-Since", lm).Code; got != 304 {
		t.Errorf("If-Modified-Since = %d, want 304", got)
	}
	old := "Mon, 01 Jan 2001 00:00:00 GMT"
	if got := get(t, s, "/crates/api/v1/crates/pkg-00003/versions", "If-Modified-Since", old).Code; got != 200 {
		t.Errorf("old If-Modified-Since = %d, want 200", got)
	}
}

func TestRateLimitAndRandom429(t *testing.T) {
	clk := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	s := New(fastConfig(func(b *Behaviour) {
		b.RateLimit = RateLimit{RPS: 1, Burst: 2, RetryAfter: 1500 * time.Millisecond}
	}), WithClock(clk.now))
	path := "/github/repos/bench/pkg-00000/releases"
	for i, want := range []int{200, 200, 429} {
		if got := get(t, s, path).Code; got != want {
			t.Errorf("request %d = %d, want %d", i, got, want)
		}
	}
	if ra := get(t, s, path).Header().Get("Retry-After"); ra != "2" {
		t.Errorf("Retry-After = %q, want 2 (rounded up)", ra)
	}
	clk.add(time.Second)
	if got := get(t, s, path).Code; got != 200 {
		t.Errorf("after refill = %d, want 200", got)
	}
	if st := s.Stats().Ecosystems["github"]; st.Throttled != 2 || st.Requests != 5 {
		t.Errorf("stats = %+v", st)
	}

	s = New(fastConfig(func(b *Behaviour) { b.Random429 = 1 }))
	if got := get(t, s, "/oci/v2/bench/pkg-00000/tags/list").Code; got != 429 {
		t.Errorf("random_429=1: got %d", got)
	}
}

func TestLatency(t *testing.T) {
	s := New(fastConfig(func(b *Behaviour) { b.Latency = 40 * time.Millisecond; b.LatencyJitter = time.Millisecond }))
	start := time.Now()
	get(t, s, "/npm/pkg-00000")
	if d := time.Since(start); d < 40*time.Millisecond {
		t.Errorf("response took %v, want >= 40ms", d)
	}
	// A cancelled request returns without waiting.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/npm/pkg-00000", nil)
	start = time.Now()
	s.ServeHTTP(httptest.NewRecorder(), r)
	if d := time.Since(start); d > 30*time.Millisecond {
		t.Errorf("cancelled request took %v", d)
	}
}

func TestNotFound(t *testing.T) {
	s := New(fastConfig(nil))
	for _, p := range []string{"/npm/pkg-00010", "/npm/requests", "/npm/pkg-1", "/pypi/pypi/pkg-0000x/json"} {
		if got := get(t, s, p).Code; got != 404 {
			t.Errorf("%s = %d, want 404", p, got)
		}
	}
	if s.Stats().Total.NotFound != 4 {
		t.Errorf("not_found = %d", s.Stats().Total.NotFound)
	}
}

func TestGitHubPaging(t *testing.T) {
	s := New(fastConfig(func(b *Behaviour) { b.History = 5 }))
	var rels []struct {
		Tag string `json:"tag_name"`
	}
	w := get(t, s, "/github/repos/bench/pkg-00000/releases?per_page=2&page=3")
	if err := json.Unmarshal(w.Body.Bytes(), &rels); err != nil {
		t.Fatal(err)
	}
	if len(rels) != 1 || rels[0].Tag != "v1.0.0" {
		t.Errorf("page 3 = %+v, want [v1.0.0]", rels)
	}
	if s.Stats().Ecosystems["github"].Seen != 0 {
		t.Error("a later page must not count as seen")
	}
}

func TestOCITagsPaging(t *testing.T) {
	s := New(fastConfig(func(b *Behaviour) { b.History = 12 }))
	var doc struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	w := get(t, s, "/oci/v2/bench/pkg-00004/tags/list?n=3&last=1.1.0")
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Name != "bench/pkg-00004" || strings.Join(doc.Tags, ",") != "1.10.0,1.11.0,1.2.0" {
		t.Errorf("got %+v", doc)
	}
	if link := w.Header().Get("Link"); !strings.Contains(link, "last=1.2.0") || !strings.Contains(link, `rel="next"`) {
		t.Errorf("Link = %q", link)
	}
	if got := get(t, s, "/oci/v2/").Code; got != 200 {
		t.Errorf("/v2/ ping = %d", got)
	}
}

func TestBodyPaddingAndStatsEndpoint(t *testing.T) {
	plain := get(t, New(fastConfig(nil)), "/npm/pkg-00000").Body.Len()
	s := New(fastConfig(func(b *Behaviour) { b.BodyPadding = 5000 }))
	if got := get(t, s, "/npm/pkg-00000").Body.Len(); got < plain+5000 {
		t.Errorf("padded body = %d bytes, want >= %d", got, plain+5000)
	}
	var st Stats
	if err := json.Unmarshal(get(t, s, StatsPath).Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Ecosystems["npm"].OK != 1 || st.Total.Requests != 1 {
		t.Errorf("stats = %+v", st)
	}
}
