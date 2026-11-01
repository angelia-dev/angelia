package githubtag_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/githubtag"
	"github.com/angelia-dev/angelia/internal/source/sourcetest"
)

const (
	secretToken  = "ghp_SyntheticTokenForTests0123456789" //nolint:gosec // G101: synthetic test value.
	tagsPath     = "/repos/example/tool/tags"
	commitPrefix = "/repos/example/tool/git/commits/"
	enterprisePx = "/api/v3"
)

func fixedNow() time.Time { return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) }

// earlier is the state of a previous scan that knew none of the fixture tags,
// so every tag is new and (not being a first scan) gets a date lookup.
var earlier = source.FetchState{Cursor: `{"tags":["v0.0.1"],"full_at":0}`}

var valid = source.Target{Kind: "github-tag", Coordinates: map[string]string{"repo": "example/tool"}}

type reqRecord struct {
	Path        string
	Page        int
	IfNoneMatch string
	Auth        string
	Status      int
}

// fakeUpstream serves recorded /tags and /git/commits responses the way
// api.github.com does: pages, a weak ETag, and 304 for a matching
// If-None-Match.
type fakeUpstream struct {
	prefix string

	mu       sync.Mutex
	tags     []json.RawMessage
	commits  map[string]json.RawMessage
	extra    int
	reqs     []reqRecord
	failTags int
	failSHA  map[string]int
}

func newFake(t *testing.T, prefix string) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{prefix: prefix, failSHA: map[string]int{}}
	b, err := os.ReadFile("testdata/tags.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &f.tags); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile("testdata/commits.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &f.commits); err != nil {
		t.Fatal(err)
	}
	return f
}

// add prepends the recorded new tag (v2.1.0).
func (f *fakeUpstream) add(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile("testdata/new-tag.json")
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tags = append([]json.RawMessage{b}, f.tags...)
	f.extra++
}

func (f *fakeUpstream) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = nil
}

func (f *fakeUpstream) records() []reqRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

// lookups returns the commit SHAs that were requested.
func (f *fakeUpstream) lookups() []string {
	var out []string
	for _, r := range f.records() {
		if sha, ok := strings.CutPrefix(r.Path, f.prefix+commitPrefix); ok {
			out = append(out, sha)
		}
	}
	return out
}

func (f *fakeUpstream) tagRequests() []reqRecord {
	var out []reqRecord
	for _, r := range f.records() {
		if r.Path == f.prefix+tagsPath {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := reqRecord{Path: r.URL.Path, IfNoneMatch: r.Header.Get("If-None-Match"), Auth: r.Header.Get("Authorization")}
	finish := func(status int) {
		rec.Status = status
		f.reqs = append(f.reqs, rec)
	}
	switch {
	case r.URL.Path == f.prefix+tagsPath:
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		page = max(page, 1)
		rec.Page = page
		if f.failTags != 0 {
			finish(f.failTags)
			http.Error(w, "boom", f.failTags)
			return
		}
		etag := fmt.Sprintf(`W/"rev%d"`, f.extra)
		w.Header().Set("ETag", etag)
		if rec.IfNoneMatch == etag {
			finish(http.StatusNotModified)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		lo := min((page-1)*per, len(f.tags))
		hi := min(lo+per, len(f.tags))
		finish(http.StatusOK)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.tags[lo:hi])
	case strings.HasPrefix(r.URL.Path, f.prefix+commitPrefix):
		sha := strings.TrimPrefix(r.URL.Path, f.prefix+commitPrefix)
		if code := f.failSHA[sha]; code != 0 {
			finish(code)
			http.Error(w, "boom", code)
			return
		}
		c, ok := f.commits[sha]
		if !ok {
			finish(http.StatusNotFound)
			http.NotFound(w, r)
			return
		}
		finish(http.StatusOK)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(c)
	default:
		finish(http.StatusNotFound)
		http.NotFound(w, r)
	}
}

func TestContract(t *testing.T) {
	f := newFake(t, "")
	sourcetest.Run(t, nil, sourcetest.Fixtures{
		Handler: f,
		New: func(baseURL string, c *http.Client) source.Source {
			// A fixed clock keeps the cursor (full_at) identical across calls.
			return githubtag.New(baseURL, c, githubtag.WithPageSize(2), githubtag.WithClock(fixedNow)) // 3 pages: paginated
		},
		Valid: valid,
		Invalid: []source.Target{
			{Kind: "github-tag"},
			{Kind: "github-tag", Coordinates: map[string]string{"repo": "justone"}},
			{Kind: "github-tag", Coordinates: map[string]string{"repo": "a/b/c"}},
			{Kind: "github-tag", Coordinates: map[string]string{"repo": "a/.."}},
			{Kind: "github-tag", Coordinates: map[string]string{"repo": "-bad/x"}},
			{Kind: "github-release", Coordinates: map[string]string{"repo": "example/tool"}},
		},
		Missing:     &source.Target{Kind: "github-tag", Coordinates: map[string]string{"repo": "example/missing"}},
		WantRaw:     []string{"v2.1.0-rc.1", "v2.0.0", "v1.2.0", "v1.1.0", "v1.0.0"},
		Conditional: true,
		Change:      func() { f.add(t) },
		// The first scan is a baseline: no date lookups, so no PublishedAt.
		HasPublishedAt: false,
	})
}

func newSource(t *testing.T, f *fakeUpstream, opts ...githubtag.Option) *githubtag.Source {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return githubtag.New(srv.URL+f.prefix, sourcetest.Client(t, srv), opts...)
}

func TestMapsFields(t *testing.T) {
	f := newFake(t, "")
	s := newSource(t, f)
	res, err := s.List(t.Context(), valid, earlier)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Versions) != 5 {
		t.Fatalf("got %d versions, want 5", len(res.Versions))
	}
	v := res.Versions[1]
	if v.Raw != "v2.0.0" || v.Prerelease != nil || v.Yanked {
		t.Errorf("v = %+v, want Raw v2.0.0 and unknown prerelease", v)
	}
	if !strings.HasSuffix(v.URL, "/example/tool/releases/tag/v2.0.0") {
		t.Errorf("URL = %q", v.URL)
	}
	if got := v.PublishedAt.Format(time.RFC3339); got != "2026-09-01T09:00:00Z" {
		t.Errorf("PublishedAt = %s, want the committer date", got)
	}
	if v.Metadata["sha"] != "c0000000000000000000000000000000000000a4" {
		t.Errorf("metadata = %v", v.Metadata)
	}
	if res.State.ETag == "" || res.State.Cursor == "" {
		t.Errorf("state = %+v", res.State)
	}
}

// Acceptance: only new tags trigger the extra date lookup.
func TestOnlyNewTagsTriggerDateLookup(t *testing.T) {
	f := newFake(t, "")
	s := newSource(t, f, githubtag.WithPageSize(2))

	first, err := s.List(t.Context(), valid, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(f.lookups()); got != 0 {
		t.Fatalf("first scan: %d date lookups, want 0 (a baseline)", got)
	}
	for _, v := range first.Versions {
		if !v.PublishedAt.IsZero() {
			t.Errorf("first scan: %s carries a date", v.Raw)
		}
	}

	// Nothing changed: 304 on page 1, no lookup at all.
	f.reset()
	second, err := s.List(t.Context(), valid, first.State)
	if err != nil || !second.NotModified {
		t.Fatalf("second = %+v, %v; want NotModified", second, err)
	}
	if got := f.lookups(); len(got) != 0 {
		t.Errorf("unchanged: lookups %v, want none", got)
	}

	// Validators dropped (page 1 refetched) but nothing is new: no lookup.
	f.reset()
	res, err := s.List(t.Context(), valid, source.FetchState{Cursor: first.State.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.lookups(); len(got) != 0 {
		t.Errorf("known tags: lookups %v, want none", got)
	}
	if len(res.Versions) != 5 {
		t.Errorf("got %d versions, want all 5 returned again", len(res.Versions))
	}
	for _, v := range res.Versions {
		if !v.PublishedAt.IsZero() {
			t.Errorf("known tag %s carries a date; the lookup was skipped", v.Raw)
		}
	}

	// One new tag: exactly one lookup, for its commit.
	f.add(t)
	f.reset()
	res, err = s.List(t.Context(), valid, first.State)
	if err != nil || res.NotModified {
		t.Fatalf("after add: %+v, %v", res, err)
	}
	if got := f.lookups(); !slices.Equal(got, []string{"c0000000000000000000000000000000000000a6"}) {
		t.Errorf("lookups = %v, want only the new tag's commit", got)
	}
	if res.Versions[0].Raw != "v2.1.0" || res.Versions[0].PublishedAt.Format(time.RFC3339) != "2026-10-25T09:00:00Z" {
		t.Errorf("new tag = %+v", res.Versions[0])
	}

	// The new state knows v2.1.0: a forced rescan looks nothing up.
	f.reset()
	if _, err := s.List(t.Context(), valid, source.FetchState{Cursor: res.State.Cursor}); err != nil {
		t.Fatal(err)
	}
	if got := f.lookups(); len(got) != 0 {
		t.Errorf("lookups after the new tag was recorded: %v", got)
	}
}

func TestDateLookupFailures(t *testing.T) {
	f := newFake(t, "")
	f.failSHA["c0000000000000000000000000000000000000a4"] = http.StatusUnprocessableEntity
	s := newSource(t, f)
	res, err := s.List(t.Context(), valid, earlier)
	if err != nil {
		t.Fatalf("422 on one commit must not fail List: %v", err)
	}
	if !res.Versions[1].PublishedAt.IsZero() || res.Versions[0].PublishedAt.IsZero() {
		t.Errorf("versions = %+v, want only v2.0.0 undated", res.Versions)
	}
}

// A lookup error that is not a context error or a rate limit (403 without
// headers, 500) leaves the tag undated, logs one warning and advances.
func TestOtherLookupErrorsLeaveTagUndated(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			f := newFake(t, "")
			f.failSHA["c0000000000000000000000000000000000000a4"] = code
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(old)
			s := newSource(t, f)
			res, err := s.List(t.Context(), valid, earlier)
			if err != nil {
				t.Fatalf("List failed: %v", err)
			}
			if !res.Versions[1].PublishedAt.IsZero() || res.Versions[0].PublishedAt.IsZero() {
				t.Errorf("want only v2.0.0 undated, got %+v", res.Versions)
			}
			if res.State.Cursor == "" || !strings.Contains(res.State.Cursor, "v2.0.0") {
				t.Errorf("state did not advance: %+v", res.State)
			}
			out := logs.String()
			if strings.Count(out, "level=WARN") != 1 || !strings.Contains(out, "example/tool") ||
				!strings.Contains(out, "v2.0.0") || strings.Contains(out, "http") {
				t.Errorf("log = %q, want one WARN with repo and tag and no URL", out)
			}
		})
	}
}

// A real rate limit (the server names a retry time) fails List.
func TestLookupRateLimitFailsList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/git/commits/") {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`[{"name":"v1","commit":{"sha":"` + strings.Repeat("a", 40) + `"}}]`))
	}))
	defer srv.Close()
	s := githubtag.New(srv.URL, sourcetest.Client(t, srv))
	var se *netguard.StatusError
	if _, err := s.List(t.Context(), valid, earlier); !errors.As(err, &se) || se.RetryAt.IsZero() {
		t.Errorf("err = %v, want a *netguard.StatusError with RetryAt", err)
	}
}

func TestLookupContextErrorFailsList(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/git/commits/") {
			cancel()
			return
		}
		_, _ = w.Write([]byte(`[{"name":"v1","commit":{"sha":"` + strings.Repeat("a", 40) + `"}}]`))
	}))
	defer srv.Close()
	s := githubtag.New(srv.URL, sourcetest.Client(t, srv))
	if _, err := s.List(ctx, valid, earlier); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestLookupCap(t *testing.T) {
	var lookups atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/git/commits/") {
			lookups.Add(1)
			_, _ = w.Write([]byte(`{"committer":{"date":"2026-01-01T00:00:00Z"}}`))
			return
		}
		var tags []string
		for i := range 60 {
			tags = append(tags, fmt.Sprintf(`{"name":"v0.%d.0","commit":{"sha":"%040x"}}`, i, i+1))
		}
		_, _ = w.Write([]byte("[" + strings.Join(tags, ",") + "]"))
	}))
	defer srv.Close()
	s := githubtag.New(srv.URL, sourcetest.Client(t, srv))
	res, err := s.List(t.Context(), valid, earlier)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Versions) != 60 || lookups.Load() != 50 {
		t.Errorf("versions = %d, lookups = %d; want 60 and 50", len(res.Versions), lookups.Load())
	}
}

func TestSkipsBadSHAAndEmptyNames(t *testing.T) {
	var lookups atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/git/commits/") {
			lookups.Add(1)
		}
		_, _ = w.Write([]byte(`[{"name":"","commit":{"sha":""}},{"name":"a","commit":{"sha":"../x"}},{"name":"a","commit":{"sha":"../x"}}]`))
	}))
	defer srv.Close()
	s := githubtag.New(srv.URL, sourcetest.Client(t, srv))
	res, err := s.List(t.Context(), valid, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Versions) != 1 || lookups.Load() != 0 {
		t.Errorf("versions = %+v, lookups = %d; want one tag and no lookup", res.Versions, lookups.Load())
	}
}

// Acceptance: pagination walks every page and stops at a short one.
func TestPagination(t *testing.T) {
	f := newFake(t, "")
	s := newSource(t, f, githubtag.WithPageSize(2))
	if _, err := s.List(t.Context(), valid, source.FetchState{}); err != nil {
		t.Fatal(err)
	}
	var pages []int
	for _, r := range f.tagRequests() {
		pages = append(pages, r.Page)
	}
	if !slices.Equal(pages, []int{1, 2, 3}) {
		t.Errorf("pages = %v, want [1 2 3] (the third is short)", pages)
	}
}

func TestMaxPagesCap(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/git/commits/") {
			_, _ = w.Write([]byte(`{"committer":{"date":"2026-01-01T00:00:00Z"}}`))
			return
		}
		i := int(n.Add(1))
		_, _ = fmt.Fprintf(w, `[{"name":"v0.%d.0","commit":{"sha":"%040x"}}]`, i, i)
	}))
	defer srv.Close()
	s := githubtag.New(srv.URL, sourcetest.Client(t, srv), githubtag.WithPageSize(1))
	res, err := s.List(t.Context(), valid, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if n.Load() != 10 || len(res.Versions) != 10 {
		t.Errorf("requests = %d, versions = %d, want 10 and 10", n.Load(), len(res.Versions))
	}
}

func TestFullRescanAfterAWindow(t *testing.T) {
	f := newFake(t, "")
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	s := newSource(t, f, githubtag.WithPageSize(2), githubtag.WithClock(func() time.Time { return now }))
	first, err := s.List(t.Context(), valid, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}

	f.reset()
	now = now.Add(23 * time.Hour)
	if res, err := s.List(t.Context(), valid, first.State); err != nil || !res.NotModified {
		t.Fatalf("within the window: %+v, %v; want NotModified", res, err)
	}

	f.reset()
	now = now.Add(2 * time.Hour)
	res, err := s.List(t.Context(), valid, first.State)
	if err != nil || res.NotModified {
		t.Fatalf("after the window: %+v, %v; want an unconditional scan", res, err)
	}
	if reqs := f.tagRequests(); len(reqs) != 3 || reqs[0].IfNoneMatch != "" {
		t.Errorf("rescan requests = %+v", reqs)
	}
	if len(f.lookups()) != 0 {
		t.Errorf("rescan looked up %v; no tag was new", f.lookups())
	}

	// A garbled cursor reads as empty: a first scan again, so no lookups.
	f.reset()
	if _, err := s.List(t.Context(), valid, source.FetchState{Cursor: "not json"}); err != nil {
		t.Fatal(err)
	}
	if got := len(f.lookups()); got != 0 {
		t.Errorf("garbled cursor: %d lookups, want 0 (a first scan)", got)
	}
}

func TestEnterpriseBaseURL(t *testing.T) {
	f := newFake(t, enterprisePx)
	s := newSource(t, f)
	res, err := s.List(t.Context(), valid, earlier)
	if err != nil {
		t.Fatalf("GHE base URL with %s prefix: %v", enterprisePx, err)
	}
	if res.Versions[0].PublishedAt.IsZero() {
		t.Error("commit lookup did not use the prefixed base")
	}
	// The web URL drops the API suffix.
	if !strings.HasSuffix(res.Versions[0].URL, "/example/tool/releases/tag/v2.1.0-rc.1") ||
		strings.Contains(res.Versions[0].URL, "/api/v3") || strings.Contains(res.Versions[0].URL, "api.github.com") {
		t.Errorf("URL = %q", res.Versions[0].URL)
	}
}

func TestTokenIsSentButNeverLeaked(t *testing.T) {
	f := newFake(t, "")
	s := newSource(t, f, githubtag.WithToken(secretToken))
	if _, err := s.List(t.Context(), valid, source.FetchState{}); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.records() {
		if r.Auth != "Bearer "+secretToken {
			t.Errorf("request %s sent Authorization %q", r.Path, r.Auth)
		}
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if out := fmt.Sprintf(format, s); strings.Contains(out, secretToken) {
			t.Errorf("fmt %s of the source leaks the token: %s", format, out)
		}
	}

	f.mu.Lock()
	f.failTags = http.StatusInternalServerError
	f.mu.Unlock()
	if _, err := s.List(t.Context(), valid, source.FetchState{}); err == nil || strings.Contains(err.Error(), secretToken) {
		t.Errorf("500 error = %v", err)
	}
	missing := source.Target{Kind: "github-tag", Coordinates: map[string]string{"repo": "example/missing"}}
	f.mu.Lock()
	f.failTags = 0
	f.mu.Unlock()
	if _, err := s.List(t.Context(), missing, source.FetchState{}); err == nil || strings.Contains(err.Error(), secretToken) {
		t.Errorf("404 error = %v", err)
	}
}

func TestTokenFromEnvIsReadPerCall(t *testing.T) {
	f := newFake(t, "")
	t.Setenv("ANGELIA_TEST_GHTAG_TOKEN", "")
	s := newSource(t, f, githubtag.WithTokenEnv("ANGELIA_TEST_GHTAG_TOKEN"))
	if _, err := s.List(t.Context(), valid, source.FetchState{}); err == nil {
		t.Error("empty token_env variable: want an error, not an anonymous request")
	}
	t.Setenv("ANGELIA_TEST_GHTAG_TOKEN", "  \n")
	if _, err := s.List(t.Context(), valid, source.FetchState{}); err == nil {
		t.Error("blank token_env variable: want an error")
	}
	if n := len(f.records()); n != 0 {
		t.Errorf("%d requests sent without a token", n)
	}
	t.Setenv("ANGELIA_TEST_GHTAG_TOKEN", " "+secretToken+"\n")
	if _, err := s.List(t.Context(), valid, source.FetchState{}); err != nil {
		t.Fatal(err)
	}
	if got := f.records()[0].Auth; got != "Bearer "+secretToken {
		t.Errorf("auth = %q, want the trimmed token", got)
	}
}

func TestBlankFixedTokenIsAnError(t *testing.T) {
	f := newFake(t, "")
	s := newSource(t, f, githubtag.WithToken("  "))
	if _, err := s.List(t.Context(), valid, source.FetchState{}); err == nil || len(f.records()) != 0 {
		t.Errorf("err = %v, requests = %d", err, len(f.records()))
	}
}

func TestInvalidBaseURLIsReportedByList(t *testing.T) {
	for _, base := range []string{"ftp://x", "://bad", "https://user:pw@host", "/relative"} {
		s := githubtag.New(base, http.DefaultClient)
		if _, err := s.List(t.Context(), valid, source.FetchState{}); err == nil {
			t.Errorf("base %q: want an error", base)
		}
	}
}

func TestUnauthorizedHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad credentials", http.StatusUnauthorized)
	}))
	defer srv.Close()
	s := githubtag.New(srv.URL, sourcetest.Client(t, srv), githubtag.WithToken(secretToken))
	_, err := s.List(t.Context(), valid, source.FetchState{})
	if err == nil || !strings.Contains(err.Error(), "invalid or expired") || strings.Contains(err.Error(), secretToken) {
		t.Errorf("err = %v", err)
	}
}

// The token must not follow a redirect to plain http, to another port or to
// another host name, which net/http alone would allow.
func TestRedirectNeverCarriesToken(t *testing.T) {
	var leaked atomic.Int32
	spy := func(r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Add(1)
		}
	}
	plain := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { spy(r) }))
	defer plain.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(plain.URL, "http://"))

	redirectTo := func(target string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target, http.StatusFound)
		})
	}
	tlsSrv := httptest.NewTLSServer(redirectTo(plain.URL + "/x"))
	defer tlsSrv.Close()
	sameHostOtherPort := httptest.NewServer(redirectTo(plain.URL + "/x"))
	defer sameHostOtherPort.Close()
	otherName := httptest.NewServer(redirectTo("http://localhost:" + port + "/x")) // 127.0.0.1 -> localhost
	defer otherName.Close()

	cases := map[string]struct {
		base string
		hc   *http.Client
	}{
		"https to plain http":   {tlsSrv.URL, tlsSrv.Client()},
		"same host, other port": {sameHostOtherPort.URL, &http.Client{}},
		"host name changes":     {otherName.URL, &http.Client{}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := githubtag.New(c.base, c.hc, githubtag.WithToken(secretToken))
			_, err := s.List(t.Context(), valid, source.FetchState{})
			if err == nil || strings.Contains(err.Error(), secretToken) {
				t.Errorf("err = %v, want a redirect refusal without the token", err)
			}
		})
	}
	if n := leaked.Load(); n != 0 {
		t.Fatalf("%d requests reached the redirect target with the token", n)
	}
}

func TestPlainRedirectWithoutTokenIsFollowed(t *testing.T) {
	f := newFake(t, "")
	srv := httptest.NewServer(f)
	defer srv.Close()
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+r.URL.RequestURI(), http.StatusMovedPermanently) //nolint:gosec // G710: test server, fixed target.
	}))
	defer front.Close()
	s := githubtag.New(front.URL, &http.Client{})
	if _, err := s.List(t.Context(), valid, source.FetchState{}); err != nil {
		t.Fatal(err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTokenNotSentOverPlainHTTP(t *testing.T) {
	var sent int
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		sent++
		return nil, errors.New("must not be called")
	})}
	s := githubtag.New("http://ghe.example.com/api/v3", hc, githubtag.WithToken(secretToken))
	_, err := s.List(t.Context(), valid, source.FetchState{})
	if err == nil || sent != 0 || strings.Contains(err.Error(), secretToken) {
		t.Errorf("err = %v, requests = %d; want a refusal that sent nothing", err, sent)
	}
}

func TestContextCancelledWraps(t *testing.T) {
	f := newFake(t, "")
	s := newSource(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.List(ctx, valid, source.FetchState{}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestBadBodiesAreErrors(t *testing.T) {
	for name, body := range map[string]string{
		"tags":   `{"message":"not a list"}`,
		"commit": "",
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/git/commits/") {
					_, _ = w.Write([]byte(`[]`))
					return
				}
				if name == "commit" {
					_, _ = w.Write([]byte(`[{"name":"v1","commit":{"sha":"` + strings.Repeat("a", 40) + `"}}]`))
					return
				}
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			s := githubtag.New(srv.URL, sourcetest.Client(t, srv))
			res, err := s.List(t.Context(), valid, earlier)
			if name == "commit" {
				// A bad commit body is a lookup failure: the tag stays undated.
				if err != nil || len(res.Versions) != 1 || !res.Versions[0].PublishedAt.IsZero() {
					t.Errorf("res = %+v, err = %v; want one undated tag", res, err)
				}
				return
			}
			if err == nil {
				t.Error("expected a decode error")
			}
		})
	}
}

// The source works on top of the real netguard client, reaching loopback via
// Allow.
func TestWithNetguardClient(t *testing.T) {
	f := newFake(t, "")
	srv := httptest.NewServer(f)
	defer srv.Close()
	c, err := netguard.New(netguard.Config{Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	if err != nil {
		t.Fatal(err)
	}
	s := githubtag.New(srv.URL, c.HTTPClient(), githubtag.WithToken(secretToken))
	first, err := s.List(t.Context(), valid, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.List(t.Context(), valid, first.State)
	if err != nil {
		t.Fatal(err)
	}
	if !second.NotModified || second.State.ETag != first.State.ETag || second.State.Cursor != first.State.Cursor {
		t.Errorf("second = %+v, first state = %+v", second, first.State)
	}
}

// With the default base URL the version links to github.com, not the API host.
func TestDefaultBaseMapsToWebURL(t *testing.T) {
	f := newFake(t, "")
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.github.com" {
			return nil, errors.New("unexpected host " + r.URL.Host)
		}
		rec := httptest.NewRecorder()
		f.ServeHTTP(rec, r)
		return rec.Result(), nil
	})}
	s := githubtag.New("", hc)
	res, err := s.List(t.Context(), valid, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Versions[1].URL; got != "https://github.com/example/tool/releases/tag/v2.0.0" {
		t.Errorf("URL = %q", got)
	}
}

// A tag name with "/" keeps it in the web URL; other reserved characters are
// escaped per segment.
func TestTagURLKeepsSlash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"name":"release/v1.0 #1","commit":{"sha":"` + strings.Repeat("a", 40) + `"}}]`))
	}))
	defer srv.Close()
	s := githubtag.New(srv.URL, sourcetest.Client(t, srv))
	res, err := s.List(t.Context(), valid, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Versions[0].URL; !strings.HasSuffix(got, "/example/tool/releases/tag/release/v1.0%20%231") {
		t.Errorf("URL = %q", got)
	}
}

// The token from the environment is sent on every request of a List call.
func TestTokenEnvWorksAcrossPages(t *testing.T) {
	f := newFake(t, "")
	srv := httptest.NewServer(f)
	defer srv.Close()
	t.Setenv("GHTAG_TEST_TOKEN", secretToken)
	s := githubtag.New(srv.URL, sourcetest.Client(t, srv), githubtag.WithTokenEnv("GHTAG_TEST_TOKEN"), githubtag.WithPageSize(2))
	if _, err := s.List(t.Context(), valid, earlier); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.records() {
		if r.Auth != "Bearer "+secretToken {
			t.Fatalf("request %s carried %q", r.Path, r.Auth)
		}
	}
}
