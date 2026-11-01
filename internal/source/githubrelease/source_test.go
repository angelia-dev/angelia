package githubrelease_test

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
	"github.com/angelia-dev/angelia/internal/source/githubrelease"
	"github.com/angelia-dev/angelia/internal/source/sourcetest"
)

const (
	secretToken  = "ghp_SyntheticTokenForTests0123456789" //nolint:gosec // G101: synthetic test value.
	startBudget  = 5000
	repoPath     = "/repos/example/tool/releases"
	enterprisePx = "/api/v3"
)

func fixedNow() time.Time { return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) }

var valid = source.Target{Kind: "github-release", Coordinates: map[string]string{"repo": "example/tool"}}

// reqRecord is what the fake upstream saw for one request.
type reqRecord struct {
	Page        int
	PerPage     int
	IfNoneMatch string
	Auth        string
	Status      int
}

// fakeUpstream serves the recorded releases fixture the way api.github.com
// does: pages, a weak ETag on every response, a 304 for a matching
// If-None-Match, and X-RateLimit-Remaining that only 200 responses decrement.
type fakeUpstream struct {
	prefix string

	mu        sync.Mutex
	releases  []json.RawMessage
	remaining int
	extra     int // releases added by add()
	reqs      []reqRecord
	failWith  int
}

func newFake(t *testing.T, prefix string) *fakeUpstream {
	t.Helper()
	b, err := os.ReadFile("testdata/releases.json")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeUpstream{prefix: prefix, remaining: startBudget}
	if err := json.Unmarshal(b, &f.releases); err != nil {
		t.Fatal(err)
	}
	return f
}

// add prepends the recorded new release (id 107, v2.1.0).
func (f *fakeUpstream) add(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile("testdata/new-release.json")
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases = append([]json.RawMessage{b}, f.releases...)
	f.extra++
}

func (f *fakeUpstream) records() []reqRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path != f.prefix+repoPath {
		http.NotFound(w, r)
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if page < 1 {
		page = 1
	}
	rec := reqRecord{Page: page, PerPage: per, IfNoneMatch: r.Header.Get("If-None-Match"), Auth: r.Header.Get("Authorization")}
	finish := func(status int) {
		rec.Status = status
		f.reqs = append(f.reqs, rec)
	}
	if f.failWith != 0 {
		finish(f.failWith)
		http.Error(w, "boom", f.failWith)
		return
	}
	etag := fmt.Sprintf(`W/"rev%d"`, f.extra)
	w.Header().Set("ETag", etag)
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(startBudget))
	if rec.IfNoneMatch == etag {
		if rec.Auth == "" {
			f.remaining-- // anonymous 304s are charged; authenticated ones are free
		}
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(f.remaining))
		finish(http.StatusNotModified)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	f.remaining--
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(f.remaining))
	lo := min((page-1)*per, len(f.releases))
	hi := min(lo+per, len(f.releases))
	finish(http.StatusOK)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(f.releases[lo:hi])
}

func TestContract(t *testing.T) {
	f := newFake(t, "")
	sourcetest.Run(t, nil, sourcetest.Fixtures{
		Handler: f,
		New: func(baseURL string, c *http.Client) source.Source {
			// A fixed clock keeps the cursor (full_at) identical across calls.
			return githubrelease.New(baseURL, c, githubrelease.WithPageSize(2), githubrelease.WithClock(fixedNow)) // 3 pages: paginated
		},
		Valid: valid,
		Invalid: []source.Target{
			{Kind: "github-release"},
			{Kind: "github-release", Coordinates: map[string]string{"repo": "justone"}},
			{Kind: "github-release", Coordinates: map[string]string{"repo": "a/b/c"}},
			{Kind: "github-release", Coordinates: map[string]string{"repo": "a/.."}},
			{Kind: "github-release", Coordinates: map[string]string{"repo": "-bad/x"}},
			{Kind: "pypi", Coordinates: map[string]string{"repo": "example/tool"}},
		},
		Missing:        &source.Target{Kind: "github-release", Coordinates: map[string]string{"repo": "example/missing"}},
		WantRaw:        []string{"v2.1.0-rc.1", "v2.0.0", "v1.2.0", "v1.1.0", "v1.0.0"},
		Conditional:    true,
		Change:         func() { f.add(t) },
		HasPublishedAt: true,
	})
}

func newSource(t *testing.T, f *fakeUpstream, opts ...githubrelease.Option) (*githubrelease.Source, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return githubrelease.New(srv.URL+f.prefix, sourcetest.Client(t, srv), opts...), srv
}

func TestMapsFields(t *testing.T) {
	f := newFake(t, "")
	s, _ := newSource(t, f)
	res, err := s.List(t.Context(), valid, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Versions) != 5 {
		t.Fatalf("got %d versions, want 5 (draft skipped)", len(res.Versions))
	}
	for _, v := range res.Versions {
		if v.Raw == "v2.2.0" {
			t.Fatal("draft release returned")
		}
	}
	rc, ga := res.Versions[0], res.Versions[1]
	if rc.Raw != "v2.1.0-rc.1" || rc.Prerelease == nil || !*rc.Prerelease {
		t.Errorf("rc = %+v, want prerelease true", rc)
	}
	if ga.Prerelease == nil || *ga.Prerelease {
		t.Errorf("ga = %+v, want prerelease false", ga)
	}
	if ga.URL != "https://github.com/example/tool/releases/tag/v2.0.0" || ga.NotesURL == "" {
		t.Errorf("urls = %q, %q", ga.URL, ga.NotesURL)
	}
	if ga.PublishedAt.Format("2006-01-02T15:04:05Z") != "2026-09-01T09:05:00Z" {
		t.Errorf("PublishedAt = %v", ga.PublishedAt)
	}
	if cursorMax(t, res.State) != 105 || res.State.ETag == "" {
		t.Errorf("state = %+v", res.State)
	}
	if recs := f.records(); len(recs) != 1 || recs[0].PerPage != 100 {
		t.Errorf("requests = %+v, want one page request with per_page=100", recs)
	}
}

// Acceptance: a second fetch with an unchanged ETag is a 304 and the
// rate limit headers of the recorded responses show that it cost nothing.
func TestNotModifiedDoesNotCostRateLimit(t *testing.T) {
	f := newFake(t, "")
	var headers []http.Header
	var mu sync.Mutex
	srv := httptest.NewServer(f)
	defer srv.Close()
	base := sourcetest.Client(t, srv)
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := base.Transport.RoundTrip(r)
		if err == nil {
			mu.Lock()
			headers = append(headers, resp.Header.Clone())
			mu.Unlock()
		}
		return resp, err
	})}
	s := githubrelease.New(srv.URL, hc, githubrelease.WithToken(secretToken))
	first, err := s.List(t.Context(), valid, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.List(t.Context(), valid, first.State)
	if err != nil {
		t.Fatal(err)
	}
	if !second.NotModified || len(second.Versions) != 0 || second.State != first.State {
		t.Fatalf("second = %+v, want NotModified with the same state", second)
	}
	if len(headers) != 2 {
		t.Fatalf("saw %d responses, want 2", len(headers))
	}
	r1, r2 := headers[0].Get("X-RateLimit-Remaining"), headers[1].Get("X-RateLimit-Remaining")
	if r1 != strconv.Itoa(startBudget-1) || r2 != r1 {
		t.Errorf("remaining after 200 = %s, after 304 = %s; the 304 must not consume budget", r1, r2)
	}
	recs := f.records()
	if recs[1].Status != http.StatusNotModified || recs[1].IfNoneMatch != first.State.ETag {
		t.Errorf("second request = %+v, want If-None-Match %q answered 304", recs[1], first.State.ETag)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Acceptance: pagination stops once a page only has known versions.
func TestStopsPaginatingAtKnownVersions(t *testing.T) {
	f := newFake(t, "")
	s, _ := newSource(t, f, githubrelease.WithPageSize(2))
	first, err := s.List(t.Context(), valid, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if got := pages(f.records()); !slices.Equal(got, []int{1, 2, 3, 4}) {
		t.Fatalf("initial fetch pages = %v, want [1 2 3 4] (the last one is empty)", got)
	}

	// Nothing new but the validator is gone: page 1 is all known, stop there.
	f.mu.Lock()
	f.reqs = nil
	f.mu.Unlock()
	st := source.FetchState{Cursor: first.State.Cursor}
	if _, err := s.List(t.Context(), valid, st); err != nil {
		t.Fatal(err)
	}
	if got := pages(f.records()); !slices.Equal(got, []int{1}) {
		t.Errorf("known-only fetch pages = %v, want [1]", got)
	}

	// One new release (id 107): pages [107 106] new, [105 104] all known -> stop.
	f.add(t)
	f.mu.Lock()
	f.reqs = nil
	f.mu.Unlock()
	res, err := s.List(t.Context(), valid, first.State)
	if err != nil {
		t.Fatal(err)
	}
	if got := pages(f.records()); !slices.Equal(got, []int{1, 2}) {
		t.Errorf("incremental fetch pages = %v, want [1 2] (page 3 only has known versions)", got)
	}
	if res.Versions[0].Raw != "v2.1.0" || cursorMax(t, res.State) != 107 {
		t.Errorf("got first %q cursor %q, want v2.1.0 and 107", res.Versions[0].Raw, res.State.Cursor)
	}
}

func pages(rs []reqRecord) []int {
	var p []int
	for _, r := range rs {
		p = append(p, r.Page)
	}
	return p
}

func TestEnterpriseBaseURL(t *testing.T) {
	f := newFake(t, enterprisePx)
	s, _ := newSource(t, f)
	if _, err := s.List(t.Context(), valid, source.FetchState{}); err != nil {
		t.Fatalf("GHE base URL with %s prefix: %v", enterprisePx, err)
	}
}

func TestTokenIsSentButNeverLeaked(t *testing.T) {
	f := newFake(t, "")
	s, _ := newSource(t, f, githubrelease.WithToken(secretToken))
	if _, err := s.List(t.Context(), valid, source.FetchState{}); err != nil {
		t.Fatal(err)
	}
	if got := f.records()[0].Auth; got != "Bearer "+secretToken {
		t.Errorf("Authorization = %q", got)
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		if out := fmt.Sprintf(format, s); strings.Contains(out, secretToken) {
			t.Errorf("fmt %s of the source leaks the token: %s", format, out)
		}
	}

	// Error paths: upstream failure, not found, decode failure, refusal.
	f.mu.Lock()
	f.failWith = http.StatusInternalServerError
	f.mu.Unlock()
	_, err := s.List(t.Context(), valid, source.FetchState{})
	if err == nil || strings.Contains(err.Error(), secretToken) {
		t.Errorf("500 error = %v", err)
	}
	missing := source.Target{Kind: "github-release", Coordinates: map[string]string{"repo": "example/missing"}}
	_, err = s.List(t.Context(), missing, source.FetchState{})
	if err == nil || strings.Contains(err.Error(), secretToken) {
		t.Errorf("404 error = %v", err)
	}
}

func TestTokenFromEnvIsReadPerCall(t *testing.T) {
	f := newFake(t, "")
	t.Setenv("ANGELIA_TEST_GH_TOKEN", "")
	s, _ := newSource(t, f, githubrelease.WithTokenEnv("ANGELIA_TEST_GH_TOKEN"))
	if _, err := s.List(t.Context(), valid, source.FetchState{}); err == nil {
		t.Error("empty token_env variable: want an error, not an anonymous request")
	}
	t.Setenv("ANGELIA_TEST_GH_TOKEN", "  \n")
	if _, err := s.List(t.Context(), valid, source.FetchState{}); err == nil {
		t.Error("blank token_env variable: want an error")
	}
	if n := len(f.records()); n != 0 {
		t.Errorf("%d requests sent without a token", n)
	}
	t.Setenv("ANGELIA_TEST_GH_TOKEN", " "+secretToken+"\n")
	if _, err := s.List(t.Context(), valid, source.FetchState{}); err != nil {
		t.Fatal(err)
	}
	if got := f.records()[0].Auth; got != "Bearer "+secretToken {
		t.Errorf("auth = %q, want the trimmed token", got)
	}
}

func TestBlankFixedTokenIsAnError(t *testing.T) {
	f := newFake(t, "")
	s, _ := newSource(t, f, githubrelease.WithToken("  "))
	if _, err := s.List(t.Context(), valid, source.FetchState{}); err == nil || len(f.records()) != 0 {
		t.Errorf("err = %v, requests = %d", err, len(f.records()))
	}
}

func TestInvalidBaseURLIsReportedByList(t *testing.T) {
	for _, base := range []string{"ftp://x", "://bad", "https://user:pw@host", "/relative"} {
		s := githubrelease.New(base, http.DefaultClient)
		if _, err := s.List(t.Context(), valid, source.FetchState{}); err == nil {
			t.Errorf("base %q: want an error", base)
		}
	}
}

func cursorMax(t *testing.T, st source.FetchState) int64 {
	t.Helper()
	var c struct {
		MaxID int64 `json:"max_id"`
	}
	if err := json.Unmarshal([]byte(st.Cursor), &c); err != nil {
		t.Fatalf("cursor %q: %v", st.Cursor, err)
	}
	return c.MaxID
}

// An anonymous 304 is charged by GitHub, so the free-304 property is for
// authenticated requests only.
func TestAnonymousNotModifiedIsCharged(t *testing.T) {
	f := newFake(t, "")
	s, _ := newSource(t, f)
	first, err := s.List(t.Context(), valid, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(t.Context(), valid, first.State); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.remaining != startBudget-2 {
		t.Errorf("remaining = %d, want %d", f.remaining, startBudget-2)
	}
}

func TestUnauthorizedHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad credentials", http.StatusUnauthorized)
	}))
	defer srv.Close()
	s := githubrelease.New(srv.URL, sourcetest.Client(t, srv), githubrelease.WithToken(secretToken))
	_, err := s.List(t.Context(), valid, source.FetchState{})
	if err == nil || !strings.Contains(err.Error(), "invalid or expired") || strings.Contains(err.Error(), secretToken) {
		t.Errorf("err = %v", err)
	}
}

// B1: the token must not follow a redirect to plain http, to another port or
// to another host name, which net/http alone would allow.
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

	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/x", http.StatusFound)
	}))
	defer tlsSrv.Close()

	sameHostOtherPort := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/x", http.StatusFound)
	}))
	defer sameHostOtherPort.Close()

	otherName := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://localhost:"+port+"/x", http.StatusFound) // 127.0.0.1 -> localhost
	}))
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
			s := githubrelease.New(c.base, c.hc, githubrelease.WithToken(secretToken))
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
	// no token: no credentials to protect, the redirect is followed
	c := &http.Client{}
	s := githubrelease.New(front.URL, c)
	if _, err := s.List(t.Context(), valid, source.FetchState{}); err != nil {
		t.Fatal(err)
	}
}

func TestMaxPagesCap(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := int(n.Add(1))
		_, _ = fmt.Fprintf(w, `[{"id":%d,"tag_name":"v0.%d.0","html_url":"u","draft":false,"prerelease":false,"created_at":"2026-01-01T00:00:00Z","published_at":"2026-01-01T00:00:00Z"}]`, 1000-i, i)
	}))
	defer srv.Close()
	s := githubrelease.New(srv.URL, sourcetest.Client(t, srv), githubrelease.WithPageSize(1))
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
	s, _ := newSource(t, f, githubrelease.WithPageSize(2), githubrelease.WithClock(func() time.Time { return now }))
	first, err := s.List(t.Context(), valid, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	reset := func() { f.mu.Lock(); f.reqs = nil; f.mu.Unlock() }

	reset()
	now = now.Add(23 * time.Hour)
	if res, err := s.List(t.Context(), valid, first.State); err != nil || !res.NotModified {
		t.Fatalf("within the window: %+v, %v; want NotModified", res, err)
	}

	reset()
	now = now.Add(2 * time.Hour) // 25h after the first full scan
	res, err := s.List(t.Context(), valid, first.State)
	if err != nil || res.NotModified {
		t.Fatalf("after the window: %+v, %v; want a full, unconditional scan", res, err)
	}
	if got := pages(f.records()); !slices.Equal(got, []int{1, 2, 3, 4}) || f.records()[0].IfNoneMatch != "" {
		t.Errorf("rescan pages = %v, first request %+v", got, f.records()[0])
	}
	if len(res.Versions) != 5 {
		t.Errorf("rescan returned %d versions, want 5", len(res.Versions))
	}

	// The legacy decimal cursor is read as max_id with no full scan recorded.
	reset()
	if _, err := s.List(t.Context(), valid, source.FetchState{Cursor: "105"}); err != nil {
		t.Fatal(err)
	}
	if got := pages(f.records()); len(got) != 4 {
		t.Errorf("legacy cursor pages = %v, want a full scan", got)
	}
}

func TestSourceNotLeakedBySlog(t *testing.T) {
	var buf bytes.Buffer
	for _, h := range []slog.Handler{slog.NewJSONHandler(&buf, nil), slog.NewTextHandler(&buf, nil)} {
		s := githubrelease.New("https://api.github.com", http.DefaultClient, githubrelease.WithToken(secretToken))
		slog.New(h).Info("source", "src", s, "src_v", fmt.Sprintf("%+v", s))
	}
	if strings.Contains(buf.String(), secretToken) {
		t.Errorf("token leaked through slog: %s", buf.String())
	}
}

func TestTokenNotSentOverPlainHTTP(t *testing.T) {
	var sent int
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		sent++
		return nil, errors.New("must not be called")
	})}
	s := githubrelease.New("http://ghe.example.com/api/v3", hc, githubrelease.WithToken(secretToken))
	_, err := s.List(t.Context(), valid, source.FetchState{})
	if err == nil || sent != 0 || strings.Contains(err.Error(), secretToken) {
		t.Errorf("err = %v, requests = %d; want a refusal that sent nothing", err, sent)
	}
}

func TestContextCancelledWraps(t *testing.T) {
	f := newFake(t, "")
	s, _ := newSource(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.List(ctx, valid, source.FetchState{}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestBadBodyIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"message":"not a list"}`))
	}))
	defer srv.Close()
	s := githubrelease.New(srv.URL, sourcetest.Client(t, srv))
	if _, err := s.List(t.Context(), valid, source.FetchState{}); err == nil {
		t.Error("expected a decode error")
	}
}

// The source works on top of the real netguard client (conditional requests
// and the authenticated rate-limit bucket), reaching loopback via Allow.
func TestWithNetguardClient(t *testing.T) {
	f := newFake(t, "")
	srv := httptest.NewServer(f)
	defer srv.Close()
	c, err := netguard.New(netguard.Config{Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	if err != nil {
		t.Fatal(err)
	}
	s := githubrelease.New(srv.URL, c.HTTPClient(), githubrelease.WithToken(secretToken))
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
	if recs := f.records(); recs[0].Auth == "" || recs[1].Status != http.StatusNotModified {
		t.Errorf("records = %+v", recs)
	}
}
