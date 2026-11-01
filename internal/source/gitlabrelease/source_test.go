package gitlabrelease_test

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
	"github.com/angelia-dev/angelia/internal/source/gitlabrelease"
	"github.com/angelia-dev/angelia/internal/source/sourcetest"
)

const (
	secretToken = "glpat-SyntheticTokenForTests01" //nolint:gosec // G101: synthetic test value.
	project     = "example-group/sub/tool"
	// wantEscapedPath is the path GitLab requires: the project path is one
	// URL-encoded segment.
	releasesPath = "/api/v4/projects/example-group%2Fsub%2Ftool/releases"
)

func fixedNow() time.Time { return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) }

var valid = source.Target{Kind: "gitlab-release", Coordinates: map[string]string{"project": project}}

// reqRecord is what the fake upstream saw for one request.
type reqRecord struct {
	Page         int
	PerPage      int
	Query        string
	IfNoneMatch  string
	PrivateToken string
	Auth         string
	Status       int
}

// fakeUpstream serves the recorded releases fixture the way GitLab does:
// pages, an ETag on every response, a 304 for a matching If-None-Match, and
// the project path as one URL-encoded segment under a prefix.
type fakeUpstream struct {
	prefix string
	path   string // escaped path of the project's releases, without prefix

	mu       sync.Mutex
	releases []json.RawMessage
	extra    int // releases added by add()
	reqs     []reqRecord
	failWith int
}

func newFake(t *testing.T, prefix string) *fakeUpstream {
	t.Helper()
	b, err := os.ReadFile("testdata/releases.json")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeUpstream{prefix: prefix, path: releasesPath}
	if err := json.Unmarshal(b, &f.releases); err != nil {
		t.Fatal(err)
	}
	return f
}

// add inserts the recorded new release (v2.1.0) below the upcoming one.
func (f *fakeUpstream) add(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile("testdata/new-release.json")
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases = slices.Insert(f.releases, 1, json.RawMessage(b))
	f.extra++
}

// release makes the upcoming release v2.2.0 a released one, as GitLab does
// when its released_at passes. The ETag does not change.
func (f *fakeUpstream) release(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var m map[string]any
	if err := json.Unmarshal(f.releases[0], &m); err != nil {
		t.Fatal(err)
	}
	m["upcoming_release"] = false
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	f.releases[0] = b
}

func (f *fakeUpstream) records() []reqRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

func (f *fakeUpstream) reset() {
	f.mu.Lock()
	f.reqs = nil
	f.mu.Unlock()
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.EscapedPath() != f.prefix+f.path {
		http.NotFound(w, r)
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if page < 1 {
		page = 1
	}
	rec := reqRecord{
		Page: page, PerPage: per, Query: r.URL.RawQuery, IfNoneMatch: r.Header.Get("If-None-Match"),
		PrivateToken: r.Header.Get("PRIVATE-TOKEN"), Auth: r.Header.Get("Authorization"),
	}
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
	if rec.IfNoneMatch == etag {
		finish(http.StatusNotModified)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	lo := min((page-1)*per, len(f.releases))
	hi := min(lo+per, len(f.releases))
	finish(http.StatusOK)
	// GitLab sends X-Next-Page empty on the last page.
	next := ""
	if hi < len(f.releases) {
		next = strconv.Itoa(page + 1)
	}
	w.Header()["X-Next-Page"] = []string{next}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(f.releases[lo:hi])
}

func TestContract(t *testing.T) {
	f := newFake(t, "")
	sourcetest.Run(t, nil, sourcetest.Fixtures{
		Handler: f,
		New: func(baseURL string, c *http.Client) source.Source {
			// A fixed clock keeps the cursor (full_at) identical across calls.
			return gitlabrelease.New(baseURL, c, gitlabrelease.WithPageSize(2), gitlabrelease.WithClock(fixedNow)) // 4 requests: paginated
		},
		Valid: valid,
		Invalid: []source.Target{
			{Kind: "gitlab-release"},
			{Kind: "gitlab-release", Coordinates: map[string]string{"project": "justone"}},
			{Kind: "gitlab-release", Coordinates: map[string]string{"project": "a/../b"}},
			{Kind: "gitlab-release", Coordinates: map[string]string{"project": "a/b/"}},
			{Kind: "gitlab-release", Coordinates: map[string]string{"project": "/a/b"}},
			{Kind: "gitlab-release", Coordinates: map[string]string{"project": "a//b"}},
			{Kind: "gitlab-release", Coordinates: map[string]string{"project": "a/b c"}},
			{Kind: "gitlab-release", Coordinates: map[string]string{"project": "a/b?x=1"}},
			{Kind: "gitlab-release", Coordinates: map[string]string{"project": "a/b%2Fc"}},
			{Kind: "gitlab-release", Coordinates: map[string]string{"project": "a/-b"}},
			{Kind: "gitlab-release", Coordinates: map[string]string{"project": "12a"}},
			{Kind: "pypi", Coordinates: map[string]string{"project": project}},
		},
		Missing:        &source.Target{Kind: "gitlab-release", Coordinates: map[string]string{"project": "example-group/missing"}},
		WantRaw:        []string{"v2.1.0-rc.1", "v2.0.0", "v1.2.0", "v1.1.0", "v1.0.0"},
		Conditional:    true,
		Change:         func() { f.add(t) },
		HasPublishedAt: true,
	})
}

func newSource(t *testing.T, f *fakeUpstream, opts ...gitlabrelease.Option) (*gitlabrelease.Source, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return gitlabrelease.New(srv.URL+f.prefix, sourcetest.Client(t, srv), opts...), srv
}

func TestValidateAcceptsPathsAndIDs(t *testing.T) {
	s := gitlabrelease.New("", http.DefaultClient)
	for _, p := range []string{"group/proj", "a/b/c/d/e", "gitlab-org/gitlab-runner", "_g/my.proj_x-1", "278964"} {
		if err := s.Validate(source.Target{Kind: "gitlab-release", Coordinates: map[string]string{"project": p}}); err != nil {
			t.Errorf("project %q: %v", p, err)
		}
	}
}

func TestMapsFields(t *testing.T) {
	f := newFake(t, "")
	s, srv := newSource(t, f, gitlabrelease.WithClock(fixedNow))
	res, err := s.List(t.Context(), valid, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Versions) != 5 {
		t.Fatalf("got %d versions, want 5 (upcoming skipped)", len(res.Versions))
	}
	for _, v := range res.Versions {
		if v.Raw == "v2.2.0" {
			t.Fatal("upcoming release returned")
		}
	}
	rc, ga, noLinkSafe, noLink := res.Versions[0], res.Versions[1], res.Versions[3], res.Versions[4]
	if rc.Raw != "v2.1.0-rc.1" || rc.Prerelease != nil {
		t.Errorf("rc = %+v, want Prerelease nil (GitLab has no flag)", rc)
	}
	if ga.URL != "https://gitlab.com/example-group/sub/tool/-/releases/v2.0.0" || ga.NotesURL != ga.URL {
		t.Errorf("urls = %q, %q", ga.URL, ga.NotesURL)
	}
	if got := ga.PublishedAt.Format(time.RFC3339); got != "2026-09-01T09:05:00Z" {
		t.Errorf("PublishedAt = %v", got)
	}
	if ga.Metadata["name"] != "2.0.0" || ga.Metadata["sha"] != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa4" {
		t.Errorf("metadata = %v", ga.Metadata)
	}
	// A _links.self that is not http(s) is ignored; the URL is built instead.
	if want := srv.URL + "/example-group/sub/tool/-/releases/v1.1.0"; noLinkSafe.URL != want {
		t.Errorf("fallback URL = %q, want %q", noLinkSafe.URL, want)
	}
	// released_at is user-settable: created_at is kept in the metadata.
	if ga.Metadata["created_at"] != "2026-09-01T09:00:00Z" {
		t.Errorf("created_at metadata = %v", ga.Metadata["created_at"])
	}
	// released_at null falls back to created_at; no name means no name key.
	if noLink.PublishedAt.Format(time.RFC3339) != "2026-06-01T09:00:00Z" {
		t.Errorf("PublishedAt = %v, want created_at", noLink.PublishedAt)
	}
	if _, ok := noLink.Metadata["name"]; ok {
		t.Errorf("metadata = %v, want no empty name", noLink.Metadata)
	}
	if res.State.ETag == "" {
		t.Errorf("state = %+v", res.State)
	}
	recs := f.records()
	if len(recs) != 1 || recs[0].PerPage != 100 || !strings.Contains(recs[0].Query, "order_by=created_at") || !strings.Contains(recs[0].Query, "sort=desc") {
		t.Errorf("requests = %+v, want one page request with per_page=100, newest first", recs)
	}
}

// Acceptance: the project path is sent as one URL-encoded segment, subgroups
// included; a numeric id is sent as is.
func TestProjectPathIsURLEncoded(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.RequestURI)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	s := gitlabrelease.New(srv.URL, sourcetest.Client(t, srv))
	for _, p := range []string{"g/sub/deep/proj.js", "42"} {
		if _, err := s.List(t.Context(), source.Target{Kind: "gitlab-release", Coordinates: map[string]string{"project": p}}, source.FetchState{}); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.HasPrefix(got[0], "/api/v4/projects/g%2Fsub%2Fdeep%2Fproj.js/releases?") || !strings.HasPrefix(got[1], "/api/v4/projects/42/releases?") {
		t.Errorf("request URIs = %q", got)
	}
}

// Acceptance: a self-managed instance, at the root, under a relative URL root,
// and given with the /api/v4 suffix.
func TestSelfManagedBaseURL(t *testing.T) {
	for name, c := range map[string]struct{ prefix, baseSuffix string }{
		"root":                {"", ""},
		"relative url root":   {"/gitlab", "/gitlab"},
		"api suffix accepted": {"", "/api/v4"},
		"trailing slash":      {"", "/"},
		"api suffix any case": {"", "/API/V4/"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, c.prefix)
			srv := httptest.NewServer(f)
			defer srv.Close()
			s := gitlabrelease.New(srv.URL+c.baseSuffix, sourcetest.Client(t, srv), gitlabrelease.WithToken(secretToken))
			res, err := s.List(t.Context(), valid, source.FetchState{})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Versions) != 5 {
				t.Errorf("got %d versions, want 5", len(res.Versions))
			}
			// The fallback link is built on the instance base, not on gitlab.com.
			for _, v := range res.Versions {
				if v.Raw == "v1.1.0" && v.URL != srv.URL+c.prefix+"/example-group/sub/tool/-/releases/v1.1.0" {
					t.Errorf("v1.1.0 URL = %q", v.URL)
				}
			}
		})
	}
}

func TestTokenHeaders(t *testing.T) {
	f := newFake(t, "")
	s, _ := newSource(t, f, gitlabrelease.WithToken(secretToken))
	if _, err := s.List(t.Context(), valid, source.FetchState{}); err != nil {
		t.Fatal(err)
	}
	if r := f.records()[0]; r.PrivateToken != secretToken || r.Auth != "" {
		t.Errorf("default auth = %+v, want PRIVATE-TOKEN only", r)
	}

	f = newFake(t, "")
	s, _ = newSource(t, f, gitlabrelease.WithToken(secretToken), gitlabrelease.WithBearerAuth())
	if _, err := s.List(t.Context(), valid, source.FetchState{}); err != nil {
		t.Fatal(err)
	}
	if r := f.records()[0]; r.Auth != "Bearer "+secretToken || r.PrivateToken != "" {
		t.Errorf("bearer auth = %+v, want Authorization only", r)
	}

	f = newFake(t, "")
	s, _ = newSource(t, f)
	if _, err := s.List(t.Context(), valid, source.FetchState{}); err != nil {
		t.Fatal(err)
	}
	if r := f.records()[0]; r.PrivateToken != "" || r.Auth != "" {
		t.Errorf("anonymous request carried credentials: %+v", r)
	}
}

// Acceptance: the token never appears in an error, a log line or fmt output.
func TestTokenNeverLeaked(t *testing.T) {
	f := newFake(t, "")
	s, _ := newSource(t, f, gitlabrelease.WithToken(secretToken))
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		if out := fmt.Sprintf(format, s); strings.Contains(out, secretToken) {
			t.Errorf("fmt %s of the source leaks the token: %s", format, out)
		}
	}
	var buf bytes.Buffer
	for _, h := range []slog.Handler{slog.NewJSONHandler(&buf, nil), slog.NewTextHandler(&buf, nil)} {
		slog.New(h).Info("source", "src", s, "src_v", fmt.Sprintf("%+v", s))
	}
	if strings.Contains(buf.String(), secretToken) {
		t.Errorf("token leaked through slog: %s", buf.String())
	}

	missing := source.Target{Kind: "gitlab-release", Coordinates: map[string]string{"project": "example-group/missing"}}
	for name, run := range map[string]func() error{
		"404": func() error { _, err := s.List(t.Context(), missing, source.FetchState{}); return err },
		"500": func() error {
			f.mu.Lock()
			f.failWith = http.StatusInternalServerError
			f.mu.Unlock()
			_, err := s.List(t.Context(), valid, source.FetchState{})
			return err
		},
	} {
		if err := run(); err == nil || strings.Contains(err.Error(), secretToken) {
			t.Errorf("%s error = %v", name, err)
		}
	}
}

func TestTokenFromEnvIsReadPerCall(t *testing.T) {
	f := newFake(t, "")
	t.Setenv("ANGELIA_TEST_GL_TOKEN", "")
	s, _ := newSource(t, f, gitlabrelease.WithTokenEnv("ANGELIA_TEST_GL_TOKEN"))
	if _, err := s.List(t.Context(), valid, source.FetchState{}); err == nil {
		t.Error("empty token_env variable: want an error, not an anonymous request")
	}
	t.Setenv("ANGELIA_TEST_GL_TOKEN", "  \n")
	if _, err := s.List(t.Context(), valid, source.FetchState{}); err == nil {
		t.Error("blank token_env variable: want an error")
	}
	if n := len(f.records()); n != 0 {
		t.Errorf("%d requests sent without a token", n)
	}
	t.Setenv("ANGELIA_TEST_GL_TOKEN", " "+secretToken+"\n")
	if _, err := s.List(t.Context(), valid, source.FetchState{}); err != nil {
		t.Fatal(err)
	}
	if got := f.records()[0].PrivateToken; got != secretToken {
		t.Errorf("PRIVATE-TOKEN = %q, want the trimmed token", got)
	}
}

func TestBlankFixedTokenIsAnError(t *testing.T) {
	f := newFake(t, "")
	s, _ := newSource(t, f, gitlabrelease.WithToken("  "))
	if _, err := s.List(t.Context(), valid, source.FetchState{}); err == nil || len(f.records()) != 0 {
		t.Errorf("err = %v, requests = %d", err, len(f.records()))
	}
}

func TestInvalidBaseURLIsReportedByList(t *testing.T) {
	for _, base := range []string{
		"ftp://x", "://bad", "https://user:pw@host", "/relative",
		"https://host?x=1", "https://host/?", "https://host#frag", "https://host/api/v4?x=1",
	} {
		s := gitlabrelease.New(base, http.DefaultClient)
		if _, err := s.List(t.Context(), valid, source.FetchState{}); err == nil {
			t.Errorf("base %q: want an error", base)
		}
	}
}

func TestTokenNotSentOverPlainHTTP(t *testing.T) {
	var sent int
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		sent++
		return nil, errors.New("must not be called")
	})}
	s := gitlabrelease.New("http://gitlab.example.com", hc, gitlabrelease.WithToken(secretToken))
	_, err := s.List(t.Context(), valid, source.FetchState{})
	if err == nil || sent != 0 || strings.Contains(err.Error(), secretToken) {
		t.Errorf("err = %v, requests = %d; want a refusal that sent nothing", err, sent)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The token (PRIVATE-TOKEN, which net/http forwards to any host, or
// Authorization) must not follow a redirect to plain http, to another port or
// to another host name.
func TestRedirectNeverCarriesToken(t *testing.T) {
	var leaked atomic.Int32
	spy := func(r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "" || r.Header.Get("Authorization") != "" {
			leaked.Add(1)
		}
	}
	plain := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { spy(r) }))
	defer plain.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(plain.URL, "http://"))

	redirectTo := func(target string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target, http.StatusFound) //nolint:gosec // G710: test server, fixed target.
		})
	}
	tlsSrv := httptest.NewTLSServer(redirectTo(plain.URL + "/x"))
	defer tlsSrv.Close()
	sameHostOtherPort := httptest.NewServer(redirectTo(plain.URL + "/x"))
	defer sameHostOtherPort.Close()
	otherName := httptest.NewServer(redirectTo("http://localhost:" + port + "/x")) // 127.0.0.1 -> localhost
	defer otherName.Close()

	// https to https on the same IP and port but another (sub)domain: only the
	// host comparison can refuse it. It is refused before any dial.
	subdomain := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://api.gitlab.example.test:"+port+"/x", http.StatusFound) //nolint:gosec // G710: test server, fixed target.
	}))
	defer subdomain.Close()

	cases := map[string]struct {
		base string
		hc   *http.Client
		opts []gitlabrelease.Option
	}{
		"https to plain http":          {tlsSrv.URL, tlsSrv.Client(), nil},
		"same host, other port":        {sameHostOtherPort.URL, &http.Client{}, nil},
		"host name changes":            {otherName.URL, &http.Client{}, nil},
		"bearer, host name changes":    {otherName.URL, &http.Client{}, []gitlabrelease.Option{gitlabrelease.WithBearerAuth()}},
		"bearer, same host, otherport": {sameHostOtherPort.URL, &http.Client{}, []gitlabrelease.Option{gitlabrelease.WithBearerAuth()}},
		"https to a subdomain":         {subdomain.URL, subdomain.Client(), nil},
		"bearer, https to a subdomain": {subdomain.URL, subdomain.Client(), []gitlabrelease.Option{gitlabrelease.WithBearerAuth()}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			opts := append([]gitlabrelease.Option{gitlabrelease.WithToken(secretToken)}, c.opts...)
			s := gitlabrelease.New(c.base, c.hc, opts...)
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

// A same-origin redirect keeps working with a token, and a redirect without a
// token is followed anywhere.
func TestRedirects(t *testing.T) {
	f := newFake(t, "")
	srv := httptest.NewServer(f)
	defer srv.Close()
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+r.URL.RequestURI(), http.StatusMovedPermanently) //nolint:gosec // G710: test server, fixed target.
	}))
	defer front.Close()
	s := gitlabrelease.New(front.URL, &http.Client{})
	if _, err := s.List(t.Context(), valid, source.FetchState{}); err != nil {
		t.Fatalf("anonymous redirect: %v", err)
	}

	sameOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/old/") {
			http.Redirect(w, r, strings.TrimPrefix(r.URL.RequestURI(), "/old"), http.StatusMovedPermanently) //nolint:gosec // G710: test server.
			return
		}
		f.ServeHTTP(w, r)
	}))
	defer sameOrigin.Close()
	s = gitlabrelease.New(sameOrigin.URL+"/old", &http.Client{}, gitlabrelease.WithToken(secretToken))
	if _, err := s.List(t.Context(), valid, source.FetchState{}); err != nil {
		t.Fatalf("same-origin redirect with a token: %v", err)
	}
}

// Acceptance: pagination walks until a short page, and a 304 on page 1 stops.
func TestPaginationAndNotModified(t *testing.T) {
	f := newFake(t, "")
	s, _ := newSource(t, f, gitlabrelease.WithPageSize(2), gitlabrelease.WithClock(fixedNow))
	first, err := s.List(t.Context(), valid, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if got := pages(f.records()); !slices.Equal(got, []int{1, 2, 3}) {
		t.Fatalf("pages = %v, want [1 2 3] (X-Next-Page is empty on the last)", got)
	}
	f.reset()
	second, err := s.List(t.Context(), valid, first.State)
	if err != nil {
		t.Fatal(err)
	}
	if !second.NotModified || len(second.Versions) != 0 || second.State != first.State {
		t.Fatalf("second = %+v, want NotModified with the same state", second)
	}
	if got := pages(f.records()); !slices.Equal(got, []int{1}) || f.records()[0].IfNoneMatch != first.State.ETag {
		t.Errorf("records = %+v, want one conditional request", f.records())
	}
}

func pages(rs []reqRecord) []int {
	var p []int
	for _, r := range rs {
		p = append(p, r.Page)
	}
	return p
}

// Acceptance: an upcoming release is not reported until its released_at has
// passed, even though the ETag has not changed.
func TestUpcomingReleaseAppearsWhenDue(t *testing.T) {
	f := newFake(t, "")
	// The first scan is less than a day before the release, so only the
	// upcoming release (not the daily rescan) can trigger the second one.
	now := time.Date(2027, 1, 14, 12, 0, 0, 0, time.UTC)
	s, _ := newSource(t, f, gitlabrelease.WithClock(func() time.Time { return now }))
	first, err := s.List(t.Context(), valid, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.State.Cursor, `"upcoming_at":1799971200`) {
		t.Fatalf("cursor = %s, want the earliest upcoming released_at (2027-01-15)", first.State.Cursor)
	}

	// Before the release date: still a 304.
	now = time.Date(2027, 1, 14, 20, 0, 0, 0, time.UTC)
	res, err := s.List(t.Context(), valid, first.State)
	if err != nil || !res.NotModified {
		t.Fatalf("before due: %+v, %v; want NotModified", res, err)
	}

	// After it: GitLab flips the flag without changing the ETag.
	f.release(t)
	f.reset()
	now = time.Date(2027, 1, 15, 0, 0, 30, 0, time.UTC)
	if res, err = s.List(t.Context(), valid, first.State); err != nil || !res.NotModified {
		t.Fatalf("within the grace minute: %+v, %v; want NotModified", res, err)
	}
	f.reset()
	now = time.Date(2027, 1, 15, 0, 1, 1, 0, time.UTC)
	res, err = s.List(t.Context(), valid, first.State)
	if err != nil || res.NotModified {
		t.Fatalf("after due: %+v, %v; want a full, unconditional scan", res, err)
	}
	if f.records()[0].IfNoneMatch != "" {
		t.Errorf("request = %+v, want no If-None-Match", f.records()[0])
	}
	if len(res.Versions) != 6 || res.Versions[0].Raw != "v2.2.0" {
		t.Fatalf("versions = %+v, want v2.2.0 first among 6", res.Versions)
	}
	if strings.Contains(res.State.Cursor, "upcoming_at") {
		t.Errorf("cursor = %s, want no pending upcoming release", res.State.Cursor)
	}
}

func TestFullRescanAfterAWindow(t *testing.T) {
	f := newFake(t, "")
	now := fixedNow()
	s, _ := newSource(t, f, gitlabrelease.WithPageSize(2), gitlabrelease.WithClock(func() time.Time { return now }))
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
		t.Fatalf("after the window: %+v, %v; want a full, unconditional scan", res, err)
	}
	if got := pages(f.records()); !slices.Equal(got, []int{1, 2, 3}) || f.records()[0].IfNoneMatch != "" {
		t.Errorf("rescan pages = %v, first request %+v", got, f.records()[0])
	}
	if len(res.Versions) != 5 {
		t.Errorf("rescan returned %d versions, want 5", len(res.Versions))
	}
	// A garbage cursor is a first scan.
	f.reset()
	if _, err := s.List(t.Context(), valid, source.FetchState{Cursor: "garbage"}); err != nil || len(f.records()) != 3 {
		t.Errorf("garbage cursor: %v, %d requests, want a full scan", err, len(f.records()))
	}
}

// The page cap truncates a huge history at 10 pages; documented in doc.go.
func TestMaxPagesCap(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := int(n.Add(1))
		_, _ = fmt.Fprintf(w, `[{"tag_name":"v0.%d.0","created_at":"2026-01-01T00:00:00Z","released_at":"2026-01-01T00:00:00Z","upcoming_release":false}]`, i)
	}))
	defer srv.Close()
	s := gitlabrelease.New(srv.URL, sourcetest.Client(t, srv), gitlabrelease.WithPageSize(1))
	res, err := s.List(t.Context(), valid, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if n.Load() != 10 || len(res.Versions) != 10 {
		t.Errorf("requests = %d, versions = %d, want 10 and 10", n.Load(), len(res.Versions))
	}
}

func TestErrorHints(t *testing.T) {
	for status, want := range map[int]string{
		http.StatusUnauthorized: "invalid or expired",
		http.StatusNotFound:     "not found",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "nope", status)
		}))
		s := gitlabrelease.New(srv.URL, sourcetest.Client(t, srv), gitlabrelease.WithToken(secretToken))
		_, err := s.List(t.Context(), valid, source.FetchState{})
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), secretToken) {
			t.Errorf("status %d: err = %v, want %q", status, err, want)
		}
	}
}

func TestRateLimitIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	s := gitlabrelease.New(srv.URL, sourcetest.Client(t, srv))
	_, err := s.List(t.Context(), valid, source.FetchState{})
	var se *netguard.StatusError
	if !errors.As(err, &se) || se.RetryAt.IsZero() {
		t.Errorf("err = %v, want a *netguard.StatusError with RetryAt", err)
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
	s := gitlabrelease.New(srv.URL, sourcetest.Client(t, srv))
	if _, err := s.List(t.Context(), valid, source.FetchState{}); err == nil {
		t.Error("expected a decode error")
	}
}

// The source works on top of the real netguard client (conditional requests),
// reaching loopback via Allow.
func TestWithNetguardClient(t *testing.T) {
	f := newFake(t, "")
	srv := httptest.NewServer(f)
	defer srv.Close()
	c, err := netguard.New(netguard.Config{Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	if err != nil {
		t.Fatal(err)
	}
	s := gitlabrelease.New(srv.URL, c.HTTPClient(), gitlabrelease.WithToken(secretToken), gitlabrelease.WithClock(fixedNow))
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
	if recs := f.records(); recs[0].PrivateToken != secretToken || recs[1].Status != http.StatusNotModified {
		t.Errorf("records = %+v", recs)
	}
}

// Acceptance: an ETag change that adds one release fetches page 1 only (the
// pages are ordered by created_at), while the daily full scan walks them all.
func TestETagChangeStopsAtKnownReleases(t *testing.T) {
	f := newFake(t, "")
	now := fixedNow()
	s, _ := newSource(t, f, gitlabrelease.WithPageSize(3), gitlabrelease.WithClock(func() time.Time { return now }))
	first, err := s.List(t.Context(), valid, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.State.Cursor, `"max_created_at":1791622800000`) { // rc.1, 2026-10-10T09:00Z
		t.Fatalf("cursor = %s, want max_created_at of the newest released release", first.State.Cursor)
	}
	f.add(t)
	f.reset()
	res, err := s.List(t.Context(), valid, first.State)
	if err != nil || res.NotModified {
		t.Fatalf("after a change: %+v, %v", res, err)
	}
	if got := pages(f.records()); !slices.Equal(got, []int{1}) {
		t.Errorf("pages = %v, want only [1]", got)
	}
	var raws []string
	for _, v := range res.Versions {
		raws = append(raws, v.Raw)
	}
	if !slices.Contains(raws, "v2.1.0") {
		t.Errorf("versions = %v, want the new v2.1.0", raws)
	}
	if !strings.Contains(res.State.Cursor, `"max_created_at":1792486800000`) || !strings.Contains(res.State.Cursor, `"upcoming_at":1799971200`) {
		t.Errorf("cursor = %s, want the new max and the pending upcoming release kept", res.State.Cursor)
	}

	// The daily full scan ignores the early stop.
	f.reset()
	now = now.Add(25 * time.Hour)
	if res, err = s.List(t.Context(), valid, res.State); err != nil || res.NotModified || len(res.Versions) != 6 {
		t.Fatalf("full scan: %d versions, %v", len(res.Versions), err)
	}
	if got := pages(f.records()); !slices.Equal(got, []int{1, 2, 3}) {
		t.Errorf("full scan pages = %v, want [1 2 3]", got)
	}
}

// A release backdated through released_at (a "historical" release) has a
// recent created_at, so the early stop still reports it.
func TestBackdatedReleaseIsFoundEarly(t *testing.T) {
	f := newFake(t, "")
	s, _ := newSource(t, f, gitlabrelease.WithPageSize(2), gitlabrelease.WithClock(fixedNow))
	first, err := s.List(t.Context(), valid, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.releases = slices.Insert(f.releases, 1, json.RawMessage(`{"tag_name":"v0.9.0","name":"old","created_at":"2026-10-30T09:00:00.000Z","released_at":"2020-01-01T00:00:00.000Z","upcoming_release":false}`))
	f.extra++
	f.mu.Unlock()
	f.reset()
	res, err := s.List(t.Context(), valid, first.State)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range res.Versions {
		if v.Raw == "v0.9.0" {
			found = true
			if v.PublishedAt.Year() != 2020 || v.Metadata["created_at"] != "2026-10-30T09:00:00Z" {
				t.Errorf("v0.9.0 = %+v", v)
			}
		}
	}
	if !found {
		t.Errorf("backdated release not reported: %+v", res.Versions)
	}
	if got := pages(f.records()); len(got) > 2 {
		t.Errorf("pages = %v, want an early stop", got)
	}
}

// The recorded release carries assets, author, evidences and more: they are
// ignored, and the fields we use are still mapped.
func TestRealShapedRecordIsMapped(t *testing.T) {
	f := newFake(t, "")
	f.add(t)
	s, _ := newSource(t, f, gitlabrelease.WithClock(fixedNow))
	res, err := s.List(t.Context(), valid, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range res.Versions {
		if v.Raw == "v2.1.0" {
			if v.Metadata["sha"] != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa7" || v.URL != "https://gitlab.com/example-group/sub/tool/-/releases/v2.1.0" {
				t.Errorf("v2.1.0 = %+v", v)
			}
			return
		}
	}
	t.Error("v2.1.0 not reported")
}
