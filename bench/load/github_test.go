package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/bench/fakereg"
	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

const repoList = `# a watch list
cli/cli

Owner/Repo
other/cli
# comment
`

func writeList(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "repos.txt")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadRepoList(t *testing.T) {
	got, err := LoadRepoList(writeList(t, repoList))
	if err != nil || strings.Join(got, " ") != "cli/cli Owner/Repo other/cli" {
		t.Fatalf("got %v, %v", got, err)
	}
	for body, want := range map[string]string{
		"a/b\nnot-a-repo\n": ":2:",
		"a/b\nA/B\n":        "already on line 1",
		"# only comments\n": "no repositories",
	} {
		if _, err := LoadRepoList(writeList(t, body)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", body, err, want)
		}
	}
	if _, err := LoadRepoList(filepath.Join(t.TempDir(), "missing.txt")); err == nil || !strings.Contains(err.Error(), "make bench-github-repos") {
		t.Errorf("missing list: %v", err)
	}
}

// Benchmark 3 runs at the default interval: the watch file sets none, and
// real repositories that share a name or are not valid watch names get
// numbered names.
func TestGitHubWatchFile(t *testing.T) {
	o := options{githubRepos: writeList(t, repoList), n: 3}
	w, err := githubWatches(o)
	if err != nil || len(w) != 3 || w[1].Coordinates["repo"] != "Owner/Repo" {
		t.Fatalf("watches %v, %v", w, err)
	}
	o.n = 4
	if _, err := githubWatches(o); err == nil || !strings.Contains(err.Error(), "has 3") {
		t.Errorf("-n above the list: %v", err)
	}
	o.githubFake, o.n = true, 2
	if w, _ := githubWatches(o); len(w) != 2 || w[1].Coordinates["repo"] != "bench/pkg-00001" {
		t.Errorf("fake watches %v", w)
	}
	o.githubFake, o.n = false, 3
	w, _ = githubWatches(o)
	dir := t.TempDir()
	if _, err := (watchFileLoader{interval: watchfile.DefaultInterval}).Prepare(context.Background(), w, dir, dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, watchFileName)); bytes.Contains(b, []byte("interval")) { //nolint:gosec // test temp dir
		t.Errorf("the default interval is written:\n%s", b)
	}
	f, err := watchfile.Load(filepath.Join(dir, watchFileName), watchfile.Options{Sources: benchSources(t)})
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, x := range f.Watches {
		names = append(names, x.Name)
		if x.Interval != watchfile.DefaultInterval {
			t.Errorf("%s: interval %v", x.Name, x.Interval)
		}
	}
	if strings.Join(names, " ") != "github-release-cli github-release-00001 github-release-00002" {
		t.Errorf("names = %v", names)
	}
}

// Live, the configuration names the token variable and nothing else: the
// default base URL, no allow_networks, no host limit (api.github.com's
// built-in budget applies).
func TestWriteGitHubConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "angelia.yaml")
	if err := WriteGitHubConfig(path, "127.0.0.1:9", "", dir, "", GitHubBudgetLimit); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(context.Background(), config.Options{Path: path, Environ: []string{githubTokenChildEnv + "=tok-for-test"}})
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Sources["github-release"]
	if len(cfg.Sources) != 1 || s.BaseURL != "" || s.TokenEnv != githubTokenChildEnv {
		t.Errorf("sources = %+v", cfg.Sources)
	}
	if len(cfg.HTTP.AllowPrefixes()) != 0 {
		t.Errorf("allow_networks = %v", cfg.HTTP.AllowPrefixes())
	}
	if hosts, _ := cfg.HTTP.HostLimits.Netguard(); len(hosts) != 0 {
		t.Errorf("host_limits = %v", hosts)
	}
	// The fake dry run: the registry's github endpoint, paced like GitHub.
	if err := WriteGitHubConfig(path, "127.0.0.1:9", "http://127.0.0.1:5000", dir, "127.0.0.1/32", GitHubBudgetLimit); err != nil {
		t.Fatal(err)
	}
	if cfg, err = config.Load(context.Background(), config.Options{Path: path}); err != nil {
		t.Fatal(err)
	}
	if s := cfg.Sources["github-release"]; s.BaseURL != "http://127.0.0.1:5000/github" || s.TokenEnv != "" {
		t.Errorf("fake source = %+v", s)
	}
	if hosts, _ := cfg.HTTP.HostLimits.Netguard(); hosts["127.0.0.1"].Anonymous.Burst != 10 || hosts["127.0.0.1"].Anonymous.Rate < 1.38 || hosts["127.0.0.1"].Anonymous.Rate > 1.39 {
		t.Errorf("fake host_limits = %+v", hosts)
	}
}

const metricsBody = `# HELP x
angelia_netguard_responses_total{class="2xx",host="api.github.com"} %d
angelia_netguard_responses_total{class="304",host="api.github.com"} %d
angelia_netguard_responses_total{class="403",host="api.github.com"} %d
angelia_netguard_responses_total{class="2xx",host="other"} 99
angelia_source_fetches_total{result="ok",source="github-release"} %d
angelia_source_fetches_total{result="not_modified",source="github-release"} %d
angelia_source_fetches_total{result="ok",source="pypi"} 7
angelia_netguard_server_pauses_total{host="api.github.com",reason="retry_after"} %d
angelia_netguard_deadline_refusals_total{host="api.github.com"} 2
`

func metrics(ok2xx, n304, n403, okF, nmF, pauses int) string {
	return fmt.Sprintf(metricsBody, ok2xx, n304, n403, okF, nmF, pauses)
}

func TestGitHubCountersAndFinish(t *testing.T) {
	t0 := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	s := githubCounters(metrics(10, 0, 0, 10, 0, 0), githubHost, t0)
	if s.Responses["2xx"] != 10 || s.Fetches["ok"] != 10 || s.Fetches["not_modified"] != 0 || len(s.Responses) != 3 || s.DeadlineRefusals != 2 {
		t.Fatalf("sample = %+v", s)
	}
	at := func(m time.Duration, body string, remaining int64) GitHubSample {
		x := githubCounters(body, githubHost, t0.Add(m))
		x.RateLimit = &RateLimitCore{Limit: 5000, Remaining: remaining}
		return x
	}
	g := &GitHubResult{Samples: []GitHubSample{
		{At: t0, Responses: map[string]float64{}, Fetches: map[string]float64{}},
		at(30*time.Minute, metrics(2000, 0, 0, 2000, 0, 0), 3000),
		at(90*time.Minute, metrics(2100, 1900, 1, 2100, 1900, 1), 4800),
		at(150*time.Minute, metrics(2150, 3850, 1, 2150, 3850, 1), 4900),
	}}
	cycle := g.Samples[1]
	g.finish(&cycle)
	if g.Run.Responses["403"] != 1 || g.Run.Pauses["retry_after"] != 1 || g.Charged != 2151 {
		t.Errorf("run = %+v, charged %v", g.Run, g.Charged)
	}
	// After the cycle: 150 ok, 3850 not modified.
	if g.ETagHitRate != 3850.0/4000 || g.RequestHitRate != 3850.0/4000 || g.WindowChecks != 4000 {
		t.Errorf("hit rate %v / %v over %v", g.ETagHitRate, g.RequestHitRate, g.WindowChecks)
	}
	// Busiest 60 minutes: the first 30 (2000 from the start), more than 30-90 (101) or 90-150 (50).
	if g.PeakHour != 2000 || !g.PeakHourEndedAt.Equal(t0.Add(30*time.Minute)) {
		t.Errorf("peak %v at %v", g.PeakHour, g.PeakHourEndedAt)
	}
	if g.RemainingLowWater.Remaining != 3000 || !g.RemainingLowAt.Equal(t0.Add(30*time.Minute)) {
		t.Errorf("low-water %+v at %v", g.RemainingLowWater, g.RemainingLowAt)
	}
	if g.ChargedPerHour != 2151/2.5 {
		t.Errorf("per hour %v", g.ChargedPerHour)
	}
}

func TestEvaluateGitHub(t *testing.T) {
	base := func() *Report {
		g := &GitHubResult{
			Mode: "live", Host: githubHost, CycleAt: time.Now(), PeakHour: 2500, ETagHitRate: 0.95,
			Samples: make([]GitHubSample, 3), RemainingLowWater: &RateLimitCore{Limit: 5000, Remaining: 2500},
		}
		g.Run.Responses = map[string]float64{"2xx": 2500, "304": 40000}
		g.Window.Fetches = map[string]float64{"ok": 100, "not_modified": 1900}
		return &Report{GitHub: g}
	}
	r := base()
	r.evaluateGitHub(24 * time.Hour)
	if r.Verdict != "pass" || len(r.Checks) != 5 || len(r.Notes) != 0 {
		t.Fatalf("verdict %s, checks %+v, notes %v", r.Verdict, r.Checks, r.Notes)
	}
	for name, edit := range map[string]func(g *GitHubResult){
		"403":    func(g *GitHubResult) { g.Run.Responses["403"] = 1 },
		"429":    func(g *GitHubResult) { g.Run.Responses["429"] = 2 },
		"budget": func(g *GitHubResult) { g.PeakHour = 5000 },
		"low":    func(g *GitHubResult) { g.RemainingLowWater.Remaining = 0 },
	} {
		r := base()
		edit(r.GitHub)
		if r.evaluateGitHub(24 * time.Hour); r.Verdict != "fail" {
			t.Errorf("%s: verdict %s", name, r.Verdict)
		}
	}
	// A low hit rate is information only (maintainer's decision, #64).
	r = base()
	r.GitHub.ETagHitRate = 0.6
	if r.evaluateGitHub(24 * time.Hour); r.Verdict != "pass" || r.Checks[4].Result != "below (information)" {
		t.Errorf("low hit rate: verdict %s, checks %+v", r.Verdict, r.Checks)
	}
	// No window after the first cycle and no /rate_limit: both optional,
	// so the budget checks decide; notes say what is missing.
	r = base()
	r.GitHub.CycleAt, r.GitHub.RemainingLowWater = time.Time{}, nil
	r.GitHub.Run.Responses["4xx"] = 3
	r.GitHub.Mode = "fake"
	r.evaluateGitHub(time.Hour)
	if r.Verdict != "pass" || len(r.Notes) != 4 {
		t.Errorf("verdict %s, notes %v", r.Verdict, r.Notes)
	}
}

func rateLimitServer(t *testing.T, remaining *atomic.Int64, reset int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rate_limit" || r.Header.Get("Authorization") != "Bearer sentinel-token-1234" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprintf(w, `{"resources":{"core":{"limit":5000,"remaining":%d,"used":%d,"reset":%d}}}`, remaining.Load(), 5000-remaining.Load(), reset)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAwaitFreshBudget(t *testing.T) {
	var remaining atomic.Int64
	remaining.Store(4990)
	reset := time.Now().Add(time.Hour).Unix()
	srv := rateLimitServer(t, &remaining, reset)
	log := slog.New(slog.DiscardHandler)
	ctx := context.Background()
	if rl, err := readRateLimit(ctx, srv.URL, "sentinel-token-1234"); err != nil || rl.Remaining != 4990 || rl.Reset != reset {
		t.Fatalf("readRateLimit = %+v, %v", rl, err)
	}
	if _, err := readRateLimit(ctx, srv.URL, "wrong"); err == nil || strings.Contains(err.Error(), "wrong") {
		t.Errorf("bad token: %v", err)
	}
	// The clock and the sleep are injected: nothing waits for real.
	var slept []time.Duration
	sleep := func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	clock := func() time.Time { return time.Unix(reset, 0).Add(-20 * time.Minute) }
	// Fresh (99% or more left): no wait.
	if err := awaitFreshBudget(ctx, srv.URL, "sentinel-token-1234", log, clock, sleep); err != nil || len(slept) != 0 {
		t.Fatalf("fresh: slept %v, %v", slept, err)
	}
	// Used: waits until the reset plus 5 s.
	remaining.Store(100)
	if err := awaitFreshBudget(ctx, srv.URL, "sentinel-token-1234", log, clock, sleep); err != nil || len(slept) != 1 || slept[0] != 20*time.Minute+5*time.Second {
		t.Errorf("used: slept %v, %v", slept, err)
	}
	// A reset already past: no wait.
	late := func() time.Time { return time.Unix(reset, 0).Add(time.Minute) }
	if err := awaitFreshBudget(ctx, srv.URL, "sentinel-token-1234", log, late, sleep); err != nil || len(slept) != 1 {
		t.Errorf("past reset: slept %v, %v", slept, err)
	}
	// The real sleep ends with the context.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := sleepCtx(cctx, time.Hour); err == nil {
		t.Error("canceled sleep: want error")
	}
	if err := sleepCtx(ctx, time.Millisecond); err != nil {
		t.Error(err)
	}
	if err := awaitFreshBudget(ctx, srv.URL, "wrong", log, clock, sleep); err == nil || !strings.Contains(err.Error(), githubTokenEnv) {
		t.Errorf("bad token: %v", err)
	}
}

// The token reaches angelia only through its environment: never as an
// argument, and never in a file of the output directory or the log.
func TestTokenStaysSecret(t *testing.T) {
	const tok = "sentinel-token-1234"
	var calls [][]string
	var gotEnv []string
	d := &dockerTarget{
		name: "b", image: "img", bin: "/abs/angelia", workdir: "/w", memory: "256m", cpus: "1", logPath: filepath.Join(t.TempDir(), "log"),
		secretEnv: []string{githubTokenChildEnv + "=" + tok},
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, append([]string{name}, args...))
			return nil, nil
		},
		runEnv: func(_ context.Context, env []string, name string, args ...string) ([]byte, error) {
			gotEnv = env
			calls = append(calls, append([]string{name}, args...))
			return nil, nil
		},
	}
	if err := d.Start(context.Background(), "/work/angelia.yaml", nil); err != nil {
		t.Fatal(err)
	}
	run := strings.Join(calls[len(calls)-1], " ")
	if !strings.Contains(run, "-e "+githubTokenChildEnv+" img") || strings.Contains(run, tok) {
		t.Errorf("docker run = %q", run)
	}
	if len(gotEnv) != 1 || gotEnv[0] != githubTokenChildEnv+"="+tok {
		t.Errorf("docker env = %v", gotEnv)
	}
	env := childEnv([]string{"PATH=/bin", githubTokenEnv + "=" + tok, "HOME=/h"}, []string{githubTokenChildEnv + "=" + tok})
	if strings.Join(env, " ") != "PATH=/bin HOME=/h "+githubTokenChildEnv+"="+tok {
		t.Errorf("child env = %v", env)
	}
	if s := fmt.Sprintf("%v %+v %#v", secretString(tok), options{GitHubToken: tok}, secretString(tok)); strings.Contains(s, tok) {
		t.Errorf("formatted: %s", s)
	}
}

// End to end with a fake angelia and a fake /rate_limit: live mode reads
// the token from the environment, hands it to the target only, and writes
// the report with every check.
func TestRunGitHubLive(t *testing.T) {
	const tok = "sentinel-token-1234"
	t.Setenv(githubTokenEnv, tok)
	var remaining atomic.Int64
	remaining.Store(5000)
	api := rateLimitServer(t, &remaining, time.Now().Add(time.Hour).Unix())
	var scrapes atomic.Int32
	ft := &fakeTarget{srv: httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			return
		}
		k := int(scrapes.Add(1))
		remaining.Store(int64(5000 - 3 - k))
		_, _ = io.WriteString(w, metrics(3, 100*(k-1), 0, 3, 100*(k-1), 0))
	}))}
	defer ft.srv.Close()
	out := t.TempDir()
	var gotSecret []string
	o := options{
		kind: "github", n: 3, githubRepos: writeList(t, repoList), duration: 60 * time.Millisecond, interval: 10 * time.Millisecond,
		bin: "bin/angelia", out: out, readyTimeout: 5 * time.Second, loader: watchFileLoader{interval: time.Hour},
		checkInterval: 5 * time.Millisecond, githubAPI: api.URL, waitBudget: true, metricsEvery: time.Millisecond,
		newTarget: func(o options, _ string) Target { gotSecret = secretEnv(o); return ft },
		probe:     fakeProbe(nil, map[string]string{"git rev-parse": "deadbeef"}),
	}
	var stderr bytes.Buffer
	o.GitHubToken = secretString(os.Getenv(githubTokenEnv))
	rep, err := benchGitHub(context.Background(), o, slog.New(slog.NewTextHandler(&stderr, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if len(gotSecret) != 1 || gotSecret[0] != githubTokenChildEnv+"="+tok {
		t.Errorf("target env = %v", gotSecret)
	}
	g := rep.GitHub
	if rep.Verdict != "pass" || g.CycleAt.IsZero() || g.ETagHitRate != 1 || g.RemainingStart == nil || g.RemainingLowWater == nil {
		t.Errorf("verdict %s, checks %+v, github %+v", rep.Verdict, rep.Checks, g)
	}
	md, _ := os.ReadFile(filepath.Join(out, "report.md")) //nolint:gosec // test temp dir
	for _, want := range []string{"Phase 0 benchmark 3", "## GitHub", "ETag hit rate after the first cycle", "403 responses from api.github.com", "low-water mark", "a smoke run"} {
		if !bytes.Contains(md, []byte(want)) {
			t.Errorf("report.md lacks %q:\n%s", want, md)
		}
	}
	cfg, _ := os.ReadFile(filepath.Join(out, "angelia.yaml")) //nolint:gosec // test temp dir
	if !bytes.Contains(cfg, []byte("token_env: "+githubTokenChildEnv)) || bytes.Contains(cfg, []byte("allow_networks")) {
		t.Errorf("angelia.yaml:\n%s", cfg)
	}
	files, _ := filepath.Glob(filepath.Join(out, "*"))
	for _, f := range files {
		if b, _ := os.ReadFile(f); bytes.Contains(b, []byte(tok)) { //nolint:gosec // test temp dir
			t.Errorf("%s holds the token", f)
		}
	}
	if bytes.Contains(stderr.Bytes(), []byte(tok)) {
		t.Error("the log holds the token")
	}
}

func TestRunGitHubFlags(t *testing.T) {
	t.Setenv(githubTokenEnv, "")
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"-benchmark", "github"}, &bytes.Buffer{}, &stderr); code != 2 || !strings.Contains(stderr.String(), githubTokenEnv) {
		t.Errorf("no token: exit %d, %s", code, stderr.String())
	}
	stderr.Reset()
	t.Setenv(githubTokenEnv, "tok-for-test")
	if code := run(context.Background(), []string{"-benchmark", "github", "-check-interval", "1h", "-duration", "2h"}, &bytes.Buffer{}, &stderr); code != 2 || !strings.Contains(stderr.String(), "2.1 x interval") {
		t.Errorf("duration below 2.1 x interval: exit %d, %s", code, stderr.String())
	}
	stderr.Reset()
	if code := run(context.Background(), []string{"-benchmark", "nope"}, &bytes.Buffer{}, &stderr); code != 2 || !strings.Contains(stderr.String(), "github") {
		t.Errorf("unknown benchmark: exit %d, %s", code, stderr.String())
	}
}

// The dry run's registry behaves like GitHub: weak ETags, the primary limit
// of one token (403 when used up) and its secondary limit (429).
func TestGitHubFakeregConfig(t *testing.T) {
	c, err := fakereg.LoadConfig("../fakereg/github.yaml")
	if err != nil {
		t.Fatal(err)
	}
	b := c.Ecosystems["github"]
	if b.PrimaryLimit.Requests != 5000 || b.PrimaryLimit.Window != time.Hour || b.ETag != fakereg.ETagWeak || b.RateLimit.RPS == 0 || b.ReleasesPerHour == 0 {
		t.Errorf("github = %+v", b)
	}
}

// flakyTarget is a fake angelia whose samples fail while failSample says so.
type flakyTarget struct {
	fakeTarget
	failSample func() bool
}

func (f *flakyTarget) Sample(ctx context.Context) (Sample, error) {
	if f.failSample() {
		return Sample{}, errors.New("docker stats: daemon gone")
	}
	return f.fakeTarget.Sample(ctx)
}

func liveOptions(t *testing.T, target Target, out string, duration time.Duration) options {
	t.Helper()
	var remaining atomic.Int64
	remaining.Store(5000)
	api := rateLimitServer(t, &remaining, time.Now().Add(time.Hour).Unix())
	return options{
		kind: "github", n: 3, githubRepos: writeList(t, repoList), duration: duration, interval: 5 * time.Millisecond,
		bin: "bin/angelia", out: out, readyTimeout: 5 * time.Second, loader: watchFileLoader{interval: time.Hour},
		checkInterval: 5 * time.Millisecond, githubAPI: api.URL, metricsEvery: time.Millisecond, GitHubToken: "sentinel-token-1234",
		newTarget: func(options, string) Target { return target },
		probe:     fakeProbe(nil, nil),
	}
}

func countingMetrics(t *testing.T, fail func() bool) *httptest.Server {
	t.Helper()
	var scrapes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			return
		}
		if fail != nil && fail() {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		k := int(scrapes.Add(1))
		_, _ = io.WriteString(w, metrics(3, 100*(k-1), 0, 3, 100*(k-1), 0))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Transient failures are logged and the run goes on; maxFailures in a row
// end it with a partial report whose verdict is incomplete.
func TestRunGitHubSurvivesAndAborts(t *testing.T) {
	// Every other sample and scrape fails: the run reaches its end.
	flipper := func() func() bool {
		var n atomic.Int32
		return func() bool { return n.Add(1)%2 == 0 }
	}
	ft := &flakyTarget{fakeTarget: fakeTarget{srv: countingMetrics(t, flipper())}, failSample: flipper()}
	out := t.TempDir()
	rep, err := benchGitHub(context.Background(), liveOptions(t, ft, out, 100*time.Millisecond), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if rep.GitHub.Stopped != "" || rep.Verdict != "pass" || len(rep.Samples) == 0 {
		t.Errorf("flaky run: stopped %q, verdict %s, checks %+v", rep.GitHub.Stopped, rep.Verdict, rep.Checks)
	}
	// Samples fail for good after a while: aborted, partial report written.
	var calls atomic.Int32
	ft = &flakyTarget{fakeTarget: fakeTarget{srv: countingMetrics(t, nil)}, failSample: func() bool { return calls.Add(1) > 3 }}
	out = t.TempDir()
	rep, err = benchGitHub(context.Background(), liveOptions(t, ft, out, time.Hour), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rep.GitHub.Stopped, "10 failed samples") || rep.Verdict != "incomplete" {
		t.Errorf("aborted run: stopped %q, verdict %s", rep.GitHub.Stopped, rep.Verdict)
	}
	md, err := os.ReadFile(filepath.Join(out, "report.md")) //nolint:gosec // test temp dir
	if err != nil || !bytes.Contains(md, []byte("a partial report")) || !bytes.Contains(md, []byte("**Verdict: incomplete**")) {
		t.Errorf("partial report: %v\n%s", err, md)
	}
	// Scrapes fail for good: aborted too.
	ft = &flakyTarget{fakeTarget: fakeTarget{srv: countingMetrics(t, func() bool { return true })}, failSample: func() bool { return false }}
	rep, err = benchGitHub(context.Background(), liveOptions(t, ft, t.TempDir(), time.Hour), slog.New(slog.DiscardHandler))
	if err != nil || !strings.Contains(rep.GitHub.Stopped, "scrapes") || rep.Verdict != "incomplete" {
		t.Errorf("scrapes failing: %v, stopped %q, verdict %s", err, rep.GitHub.Stopped, rep.Verdict)
	}
}

// SIGINT or SIGTERM (a canceled context) writes the partial report.
func TestRunGitHubInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var samples atomic.Int32
	ft := &flakyTarget{fakeTarget: fakeTarget{srv: countingMetrics(t, nil)}, failSample: func() bool {
		if samples.Add(1) == 5 {
			cancel()
		}
		return false
	}}
	out := t.TempDir()
	rep, err := benchGitHub(ctx, liveOptions(t, ft, out, time.Hour), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rep.GitHub.Stopped, "interrupted") || rep.Verdict != "incomplete" || len(rep.GitHub.Samples) < 2 {
		t.Errorf("stopped %q, verdict %s, %d scrapes", rep.GitHub.Stopped, rep.Verdict, len(rep.GitHub.Samples))
	}
	if _, err := os.Stat(filepath.Join(out, "report.json")); err != nil {
		t.Error(err)
	}
}

// The busiest hour is judged against the limit GitHub reports for the token.
func TestObservedTokenLimit(t *testing.T) {
	r := &Report{GitHub: &GitHubResult{Mode: "live", Host: githubHost, PeakHour: 6000, Samples: []GitHubSample{
		{Responses: map[string]float64{}, Fetches: map[string]float64{}},
		{At: time.Now(), Responses: map[string]float64{"2xx": 6000}, Fetches: map[string]float64{"ok": 6000}, RateLimit: &RateLimitCore{Limit: 15000, Remaining: 9000}},
	}}}
	r.GitHub.finish(nil)
	if r.GitHub.TokenLimit != 15000 || !strings.Contains(r.GitHub.TokenLimitSource, "rate_limit") {
		t.Fatalf("limit %d from %q", r.GitHub.TokenLimit, r.GitHub.TokenLimitSource)
	}
	r.evaluateGitHub(24 * time.Hour)
	if c := r.Checks[2]; c.Result != "pass" || !strings.Contains(c.Target, "15000") {
		t.Errorf("budget check %+v", c)
	}
	g := &GitHubResult{Samples: []GitHubSample{{}, {At: time.Now()}}}
	if g.finish(nil); g.TokenLimit != githubBudget || g.limit() != githubBudget {
		t.Errorf("fallback limit %d", g.TokenLimit)
	}
}

// The dry run: the fake registry's github endpoint, paced like GitHub, read
// back through GET /github/rate_limit; no token.
func TestRunGitHubFake(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "github.yaml")
	if err := os.WriteFile(cfgPath, []byte("ecosystems:\n  github: {latency: 0s, latency_jitter: 0s, primary_limit: {requests: 5000, window: 1h}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var scrapes atomic.Int32
	ft := &fakeTarget{srv: httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			k := int(scrapes.Add(1))
			_, _ = io.WriteString(w, strings.ReplaceAll(metrics(4, 50*(k-1), 0, 4, 50*(k-1), 0), "api.github.com", "127.0.0.1"))
		}
	}))}
	defer ft.srv.Close()
	out := t.TempDir()
	var secret []string
	o := options{
		kind: "github", githubFake: true, n: 4, fakeregConfig: cfgPath, duration: 60 * time.Millisecond, interval: 5 * time.Millisecond,
		bin: "bin/angelia", out: out, readyTimeout: 5 * time.Second, loader: watchFileLoader{interval: 5 * time.Minute},
		checkInterval: 5 * time.Millisecond, metricsEvery: time.Millisecond, hostLimit: GitHubBudgetLimit,
		newTarget: func(o options, _ string) Target { secret = secretEnv(o); return ft },
		probe:     fakeProbe(nil, nil),
	}
	rep, err := benchGitHub(context.Background(), o, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	g := rep.GitHub
	if g.Mode != "fake" || g.Host != "127.0.0.1" || secret != nil || rep.Verdict != "pass" || g.RemainingStart == nil || g.RemainingStart.Limit != 5000 {
		t.Errorf("fake run: verdict %s, github %+v, secret %v", rep.Verdict, g, secret)
	}
	cfg, _ := os.ReadFile(filepath.Join(out, "angelia.yaml")) //nolint:gosec // test temp dir
	if !bytes.Contains(cfg, []byte("/github")) || !bytes.Contains(cfg, []byte("127.0.0.1/32")) || bytes.Contains(cfg, []byte("token_env")) {
		t.Errorf("angelia.yaml:\n%s", cfg)
	}
	md, _ := os.ReadFile(filepath.Join(out, "report.md")) //nolint:gosec // test temp dir
	if !bytes.Contains(md, []byte("dry run against the fake registry")) || !bytes.Contains(md, []byte("## Fake registry (github)")) {
		t.Errorf("report.md:\n%s", md)
	}
}
