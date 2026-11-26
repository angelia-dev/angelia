package repolink_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/repolink"
	"github.com/angelia-dev/angelia/internal/source/sourcetest"
)

const canary = "ghp_CANARY0123456789"

// fakeGitHub serves one repository, example-org/widget, and records requests.
type fakeGitHub struct {
	mu       sync.Mutex
	paths    []string // path?query of every request
	auths    []string
	releases string // body of /releases; "" means 200 with []
	repo     string // body of GET /repos/example-org/widget; "" means public
	status   int    // when set, every request answers it
	block    chan struct{}
}

func (g *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.paths = append(g.paths, r.URL.RequestURI())
	g.auths = append(g.auths, r.Header.Get("Authorization"))
	status, releases, repo, block := g.status, g.releases, g.repo, g.block
	g.mu.Unlock()
	if block != nil {
		<-block
	}
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/repos/example-org/widget/releases":
		if releases == "" {
			releases = "[]"
		}
		_, _ = fmt.Fprint(w, releases)
	case "/repos/example-org/widget":
		if repo == "" {
			repo = `{"private":false}`
		}
		_, _ = fmt.Fprint(w, repo)
	default:
		http.NotFound(w, r)
	}
}

func (g *fakeGitHub) count(substr string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, p := range g.paths {
		if strings.Contains(p, substr) {
			n++
		}
	}
	return n
}

func (g *fakeGitHub) total() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.paths)
}

func (g *fakeGitHub) authSeen() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, a := range g.auths {
		if a != "" {
			return true
		}
	}
	return false
}

type rec struct {
	mu sync.Mutex
	m  map[string]int
}

func (r *rec) NotesEnrichment(kind, result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]int{}
	}
	r.m[kind+"/"+result]++
}

func (r *rec) get(k string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[k]
}

type env struct {
	gh   *fakeGitHub
	srv  *httptest.Server
	rec  *rec
	logs *bytes.Buffer
}

func newEnv(t *testing.T, gh *fakeGitHub) *env {
	t.Helper()
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)
	return &env{gh: gh, srv: srv, rec: &rec{}, logs: &bytes.Buffer{}}
}

func (e *env) filler(t *testing.T, hc *http.Client, opts ...repolink.Option) *repolink.Filler {
	t.Helper()
	if hc == nil {
		hc = sourcetest.Client(t, e.srv)
	}
	log := slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return repolink.NewFiller(e.srv.URL, hc, append([]repolink.Option{repolink.WithLogger(log), repolink.WithRecorder(e.rec)}, opts...)...)
}

func pypiTarget() source.Target {
	return source.Target{Kind: "pypi", Coordinates: map[string]string{"name": "widget"}}
}

func npmTarget(name string) source.Target {
	return source.Target{Kind: "npm", Coordinates: map[string]string{"name": name}}
}

func pv(raw string) source.Version {
	return source.Version{Raw: raw, Metadata: map[string]any{
		"repository_url": "https://github.com/example-org/widget", "project": "widget", "project_name": "Widget",
	}}
}

const rel123 = `[{"tag_name":"v1.2.3","html_url":"https://github.com/example-org/widget/releases/tag/v1.2.3","draft":false,"body":"## 1.2.3\r\n\r\n- Fixed a thing\r\n"}]`

func TestFillMatchesATag(t *testing.T) {
	t.Parallel()
	e := newEnv(t, &fakeGitHub{releases: rel123})
	in := []source.Version{pv("1.2.3")}
	in[0].NotesURL = "https://example.org/changes"
	out := e.filler(t, nil).Fill(context.Background(), pypiTarget(), in)
	if len(out) != 1 || out[0].Notes == nil {
		t.Fatalf("no notes: %+v", out)
	}
	if out[0].Notes.Format != "markdown" || out[0].Notes.Body != "## 1.2.3\n\n- Fixed a thing" || out[0].Notes.Truncated {
		t.Errorf("notes = %+v", out[0].Notes)
	}
	if out[0].NotesURL != "https://github.com/example-org/widget/releases/tag/v1.2.3" {
		t.Errorf("NotesURL = %q, want the release page (it overrides the declared changelog)", out[0].NotesURL)
	}
	if in[0].Notes != nil || in[0].NotesURL != "https://example.org/changes" {
		t.Error("the input slice was modified")
	}
	if e.gh.total() != 1 || e.gh.count("/repos/example-org/widget/releases?per_page=30") != 1 {
		t.Errorf("requests = %v, want exactly one releases request with per_page=30", e.gh.paths)
	}
	if e.gh.authSeen() {
		t.Error("an anonymous filler sent an Authorization header")
	}
	if e.rec.get("pypi/matched") != 1 {
		t.Errorf("metric = %v", e.rec.m)
	}
}

func TestFillMatching(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		target  source.Target
		version string
		tags    []string
		drafts  []string
		want    string // matched tag, "" for none
	}{
		{"plain tag", pypiTarget(), "1.2.3", []string{"1.2.3"}, nil, "1.2.3"},
		{"v tag", pypiTarget(), "1.2.3", []string{"v1.2.3"}, nil, "v1.2.3"},
		{"plain before v", pypiTarget(), "1.2.3", []string{"v1.2.3", "1.2.3"}, nil, "1.2.3"},
		{"scoped npm", npmTarget("@scope/pkg"), "1.2.3", []string{"@scope/pkg@1.2.3"}, nil, "@scope/pkg@1.2.3"},
		{"bare npm name", npmTarget("@scope/pkg"), "1.2.3", []string{"pkg@1.2.3"}, nil, "pkg@1.2.3"},
		{"pypi name-v", source.Target{Kind: "pypi", Coordinates: map[string]string{"name": "pydantic-core"}}, "2.1.0", []string{"pydantic-core-v2.1.0"}, nil, "pydantic-core-v2.1.0"},
		{"pypi name-", pypiTarget(), "1.2.3", []string{"widget-1.2.3"}, nil, "widget-1.2.3"},
		{"pypi name_v", pypiTarget(), "1.2.3", []string{"widget_v1.2.3"}, nil, "widget_v1.2.3"},
		{"pypi name@", pypiTarget(), "1.2.3", []string{"widget@1.2.3"}, nil, "widget@1.2.3"},
		{"project name as PyPI reports it", pypiTarget(), "1.2.3", []string{"Widget-v1.2.3"}, nil, "Widget-v1.2.3"},
		{"draft never matches", pypiTarget(), "1.2.3", []string{"v1.2.3"}, []string{"v1.2.3"}, ""},
		{"another package of the monorepo", npmTarget("pkg"), "1.2.3", []string{"other-pkg@1.2.3"}, nil, ""},
		{"longer version", pypiTarget(), "1.2.3", []string{"v1.2.30", "1.2.30"}, nil, ""},
		{"prefix of another version", pypiTarget(), "1.2.30", []string{"v1.2.3"}, nil, ""},
		{"no release at all", pypiTarget(), "1.2.3", nil, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var rs []string
			for _, tag := range c.tags {
				rs = append(rs, fmt.Sprintf(`{"tag_name":%q,"html_url":"https://github.com/example-org/widget/releases/tag/%s","draft":%v,"body":"notes of %s"}`,
					tag, tag, contains(c.drafts, tag), tag))
			}
			e := newEnv(t, &fakeGitHub{releases: "[" + strings.Join(rs, ",") + "]"})
			out := e.filler(t, nil).Fill(context.Background(), c.target, []source.Version{pv(c.version)})
			if c.want == "" {
				if out[0].Notes != nil || out[0].NotesURL != "" {
					t.Fatalf("matched %+v, want no match", out[0])
				}
				if e.rec.get(c.target.Kind+"/no_match") != 1 {
					t.Errorf("metric = %v, want no_match", e.rec.m)
				}
				return
			}
			if out[0].Notes == nil || out[0].Notes.Body != "notes of "+c.want {
				t.Fatalf("notes = %+v, want those of %s", out[0].Notes, c.want)
			}
		})
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestFillEmptyBodyKeepsNotesURL(t *testing.T) {
	t.Parallel()
	e := newEnv(t, &fakeGitHub{releases: `[{"tag_name":"1.0.0","html_url":"https://github.com/example-org/widget/releases/tag/1.0.0","draft":false,"body":null}]`})
	v := pv("1.0.0")
	v.NotesURL = "https://example.org/changes"
	out := e.filler(t, nil).Fill(context.Background(), pypiTarget(), []source.Version{v})
	if out[0].Notes != nil || out[0].NotesURL != "https://example.org/changes" {
		t.Errorf("got %+v, want nil notes and the declared changelog kept", out[0])
	}
	if e.rec.get("pypi/matched") != 1 {
		t.Errorf("metric = %v: a matched release with an empty body still matched", e.rec.m)
	}
}

func TestFillLeavesSetNotesYankedAndOtherKinds(t *testing.T) {
	t.Parallel()
	e := newEnv(t, &fakeGitHub{releases: rel123})
	f := e.filler(t, nil)
	have := pv("1.2.3")
	have.Notes = source.NewNotes("markdown", "from the source")
	yanked := pv("1.2.3")
	yanked.Yanked = true
	out := f.Fill(context.Background(), pypiTarget(), []source.Version{have, yanked})
	if out[0].Notes.Body != "from the source" || out[1].Notes != nil {
		t.Errorf("out = %+v", out)
	}
	out = f.Fill(context.Background(), source.Target{Kind: "github-release", Coordinates: map[string]string{"repo": "example-org/widget"}}, []source.Version{pv("1.2.3")})
	if out[0].Notes != nil {
		t.Error("a github-release version was enriched")
	}
	if e.gh.total() != 0 {
		t.Errorf("%d requests, want none", e.gh.total())
	}
}

func TestFillNeverRequestsAnotherHost(t *testing.T) {
	t.Parallel()
	e := newEnv(t, &fakeGitHub{releases: rel123})
	f := e.filler(t, nil)
	for repo, want := range map[string]string{
		"https://evil.example/example-org/widget":            "no_repository",
		"https://github.com.evil.example/example-org/widget": "no_repository",
		"https://gitlab.com/example-org/widget":              "not_github",
		"":                                                   "no_repository",
		"http://127.0.0.1:1/example-org/widget":              "no_repository",
		"https://github.com/example-org/widget/tree/x":       "matched", // only owner and name are read
	} {
		v := pv("1.2.3")
		if repo == "" {
			delete(v.Metadata, "repository_url")
		} else {
			v.Metadata["repository_url"] = repo
		}
		before := e.gh.total()
		out := f.Fill(context.Background(), pypiTarget(), []source.Version{v})
		if want == "matched" {
			if out[0].Notes == nil || e.gh.total() != before+1 {
				t.Errorf("%q: notes %v, requests %d", repo, out[0].Notes, e.gh.total()-before)
			}
			continue
		}
		if out[0].Notes != nil || e.gh.total() != before {
			t.Errorf("%q: notes %v, %d requests, want none", repo, out[0].Notes, e.gh.total()-before)
		}
		if e.rec.get("pypi/"+want) == 0 {
			t.Errorf("%q: metric %v lacks %s", repo, e.rec.m, want)
		}
	}
	for _, p := range e.gh.paths {
		if !strings.HasPrefix(p, "/repos/example-org/widget") {
			t.Errorf("unexpected request %q", p)
		}
	}
}

func TestFillPrivateRepositoryCheck(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	clock := &atomic.Pointer[time.Time]{}
	clock.Store(&now)
	gh := &fakeGitHub{releases: rel123, repo: `{"private":true}`}
	e := newEnv(t, gh)
	f := e.filler(t, nil, repolink.WithToken(canary), repolink.WithClock(func() time.Time { return *clock.Load() }))
	ctx := context.Background()

	out := f.Fill(ctx, pypiTarget(), []source.Version{pv("1.2.3")})
	if out[0].Notes != nil || gh.count("/releases") != 0 || gh.count("/repos/example-org/widget") != 1 {
		t.Fatalf("private repository: notes %v, requests %v", out[0].Notes, gh.paths)
	}
	if e.rec.get("pypi/private") != 1 {
		t.Errorf("metric = %v", e.rec.m)
	}
	f.Fill(ctx, pypiTarget(), []source.Version{pv("1.2.3")})
	if gh.count("/repos/example-org/widget") != 1 {
		t.Errorf("the answer is not cached: %v", gh.paths)
	}

	// Public after the entry expires: the check runs again and the notes come.
	gh.mu.Lock()
	gh.repo = `{"private":false}`
	gh.mu.Unlock()
	later := now.Add(repolink.PublicCheckTTL - time.Minute)
	clock.Store(&later)
	if out = f.Fill(ctx, pypiTarget(), []source.Version{pv("1.2.3")}); out[0].Notes != nil || gh.count("/repos/example-org/widget") != 1 {
		t.Fatalf("a 23h59m old answer was not used: %v", gh.paths)
	}
	later = now.Add(repolink.PublicCheckTTL)
	clock.Store(&later)
	out = f.Fill(ctx, pypiTarget(), []source.Version{pv("1.2.3")})
	if out[0].Notes == nil || gh.count("/repos/example-org/widget?") != 0 || gh.total() != 3 {
		t.Fatalf("after 24h: notes %v, requests %v", out[0].Notes, gh.paths)
	}
	// Warm cache: only the releases request.
	f.Fill(ctx, pypiTarget(), []source.Version{pv("1.2.3")})
	if gh.total() != 4 || gh.count("/releases") != 2 {
		t.Errorf("warm cache: requests %v", gh.paths)
	}
	if !gh.authSeen() {
		t.Error("the token was not sent")
	}
	if strings.Contains(e.logs.String(), canary) {
		t.Errorf("the token is in the log:\n%s", e.logs.String())
	}
}

func TestFillWithoutTokenSkipsPublicCheck(t *testing.T) {
	t.Parallel()
	gh := &fakeGitHub{releases: rel123, repo: `{"private":true}`}
	e := newEnv(t, gh)
	out := e.filler(t, nil).Fill(context.Background(), pypiTarget(), []source.Version{pv("1.2.3")})
	if out[0].Notes == nil || gh.count("/repos/example-org/widget") != 1 || gh.count("/releases") != 1 || gh.authSeen() {
		t.Errorf("notes %v, requests %v, auth seen %v", out[0].Notes, gh.paths, gh.authSeen())
	}
}

func TestFillAuthenticatedClientChecksToo(t *testing.T) {
	t.Parallel()
	gh := &fakeGitHub{releases: rel123, repo: `{"private":true}`}
	e := newEnv(t, gh)
	out := e.filler(t, nil, repolink.WithAuthenticated(true)).Fill(context.Background(), pypiTarget(), []source.Version{pv("1.2.3")})
	if out[0].Notes != nil || gh.count("/releases") != 0 {
		t.Errorf("a stored-secret client read a private repository: %v", gh.paths)
	}
}

func TestFillOneRequestForManyVersions(t *testing.T) {
	t.Parallel()
	e := newEnv(t, &fakeGitHub{releases: `[{"tag_name":"v1.1.0","html_url":"https://github.com/example-org/widget/releases/tag/v1.1.0","body":"one"},{"tag_name":"v1.0.0","html_url":"https://github.com/example-org/widget/releases/tag/v1.0.0","body":"zero"}]`})
	out := e.filler(t, nil).Fill(context.Background(), pypiTarget(), []source.Version{pv("1.2.0"), pv("1.1.0"), pv("1.0.0")})
	if out[0].Notes != nil || out[1].Notes.Body != "one" || out[2].Notes.Body != "zero" {
		t.Errorf("out = %+v", out)
	}
	if e.gh.total() != 1 {
		t.Errorf("%d requests, want 1", e.gh.total())
	}
}

func TestFillFailuresLeaveVersionsAndNeverLeakTheToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		gh   *fakeGitHub
		want string
	}{
		{"500", &fakeGitHub{status: 500}, "error"},
		{"404", &fakeGitHub{status: 404}, "no_repository"},
		{"429", &fakeGitHub{status: 429}, "rate_limited"},
		{"invalid JSON", &fakeGitHub{releases: `{"not":"a list"`}, "error"},
		{"wrong JSON", &fakeGitHub{releases: `{"not":"a list"}`}, "error"},
		{"private flag missing", &fakeGitHub{repo: `{}`, releases: rel123}, "error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, c.gh)
			f := e.filler(t, nil, repolink.WithToken(canary))
			in := []source.Version{pv("1.2.3")}
			out := f.Fill(context.Background(), pypiTarget(), in)
			if out[0].Notes != nil || out[0].NotesURL != "" {
				t.Fatalf("got %+v", out[0])
			}
			if e.rec.get("pypi/"+c.want) != 1 {
				t.Errorf("metric = %v, want %s", e.rec.m, c.want)
			}
			if strings.Contains(e.logs.String(), canary) {
				t.Errorf("the token is in the log:\n%s", e.logs.String())
			}
			if c.want == "error" && strings.Count(e.logs.String(), "level=WARN") != 1 {
				t.Errorf("want exactly one Warn:\n%s", e.logs.String())
			}
		})
	}
}

func loopback() []netip.Prefix {
	return []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
}

func TestFillBodyOverTheCap(t *testing.T) {
	t.Parallel()
	big := `[{"tag_name":"1.2.3","body":"` + strings.Repeat("x", 4096) + `"}]`
	e := newEnv(t, &fakeGitHub{releases: big})
	nc, err := netguard.New(netguard.Config{Allow: loopback(), MaxBodyBytes: 1024, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	out := e.filler(t, nc.HTTPClient()).Fill(context.Background(), pypiTarget(), []source.Version{pv("1.2.3")})
	if out[0].Notes != nil || e.rec.get("pypi/error") != 1 {
		t.Errorf("notes %v, metric %v", out[0].Notes, e.rec.m)
	}
}

func TestFillTimeout(t *testing.T) {
	t.Parallel()
	gh := &fakeGitHub{releases: rel123, block: make(chan struct{})}
	e := newEnv(t, gh)
	t.Cleanup(func() { close(gh.block) })
	start := time.Now()
	out := e.filler(t, nil, repolink.WithTimeout(100*time.Millisecond)).Fill(context.Background(), pypiTarget(), []source.Version{pv("1.2.3")})
	if out[0].Notes != nil || e.rec.get("pypi/error") != 1 {
		t.Errorf("notes %v, metric %v", out[0].Notes, e.rec.m)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("the step outlived its deadline")
	}
}

// With the anonymous bucket of the host empty, the limiter refuses the
// request at once instead of making the check wait (D81).
func TestFillRateLimited(t *testing.T) {
	t.Parallel()
	e := newEnv(t, &fakeGitHub{releases: rel123})
	nc, err := netguard.New(netguard.Config{
		Allow: loopback(), MaxAttempts: 1,
		HostLimits: map[string]netguard.HostLimit{"127.0.0.1": {
			Anonymous: netguard.Limit{Rate: 1.0 / 3600, Burst: 1}, Authenticated: netguard.Limit{Rate: 1, Burst: 1},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := e.filler(t, nc.HTTPClient())
	ctx := context.Background()
	if out := f.Fill(ctx, pypiTarget(), []source.Version{pv("1.2.3")}); out[0].Notes == nil {
		t.Fatalf("the first request, within the burst, failed:\n%s", e.logs.String())
	}
	start := time.Now()
	out := f.Fill(ctx, pypiTarget(), []source.Version{pv("1.2.3")})
	if out[0].Notes != nil || e.rec.get("pypi/rate_limited") != 1 {
		t.Fatalf("notes %v, metric %v\n%s", out[0].Notes, e.rec.m, e.logs.String())
	}
	if time.Since(start) > 5*time.Second {
		t.Error("the refused request waited")
	}
	if strings.Contains(e.logs.String(), "level=WARN") {
		t.Errorf("a rate-limit refusal is not a warning:\n%s", e.logs.String())
	}
}

func TestFillConcurrent(t *testing.T) {
	t.Parallel()
	e := newEnv(t, &fakeGitHub{releases: rel123})
	f := e.filler(t, nil, repolink.WithToken(canary))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if out := f.Fill(context.Background(), pypiTarget(), []source.Version{pv("1.2.3")}); out[0].Notes == nil {
				t.Error("no notes")
			}
		})
	}
	wg.Wait()
}

func TestTagCandidates(t *testing.T) {
	t.Parallel()
	got := strings.Join(repolink.TagCandidates("1.2.3", []string{"pkg"}), " ")
	if want := "1.2.3 v1.2.3 pkg@1.2.3 pkg-v1.2.3 pkg-1.2.3 pkg_v1.2.3"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if repolink.TagCandidates("", []string{"p"}) != nil {
		t.Error("an empty version has candidates")
	}
}

func TestWants(t *testing.T) {
	t.Parallel()
	f := repolink.NewFiller("", &http.Client{})
	for kind, want := range map[string]bool{"pypi": true, "npm": true, "github-release": false, "crates": false, "": false} {
		if f.Wants(kind) != want {
			t.Errorf("Wants(%q) = %v", kind, !want)
		}
	}
}
