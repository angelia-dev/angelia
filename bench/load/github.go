package main

// Phase 0 benchmark 3 (architecture.md section 6, story angelia-dev/angelia#64):
// github-release watches for real public repositories on one token, at the
// default interval, against api.github.com. -github-fake runs the same mode
// against the fake registry's github endpoint, as a dry run.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/angelia-dev/angelia/bench/fakereg"
	"github.com/angelia-dev/angelia/internal/scheduler"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/githubrelease"
)

const (
	// githubTokenEnv holds the token in the harness's environment
	// (bench/README.md, H-bench-env).
	githubTokenEnv = "ANGELIA_BENCH_GITHUB_TOKEN"
	// githubTokenChildEnv is the variable angelia reads it from
	// (sources.github-release.token_env); names starting with ANGELIA_ are
	// refused there.
	githubTokenChildEnv = "BENCH_GITHUB_TOKEN" //nolint:gosec // a variable name, not a credential
	// githubAPI is where the live run goes; the harness reads /rate_limit there.
	githubAPI  = "https://api.github.com"
	githubHost = "api.github.com"
	// githubBudget is GitHub's primary rate limit for one token, per hour.
	githubBudget = 5000
	// targetETagHitRate is benchmark 3's target: share of checks answered 304.
	targetETagHitRate = 0.90
	// githubRunHours is how long #64 asks the run to last.
	githubRunHours = 24
)

// GitHubBudgetLimit is api.github.com's built-in authenticated budget in
// angelia (5,000/h, burst 10); the fake dry run gives the fake registry's
// host the same, so it is paced like the live run.
var GitHubBudgetLimit = HostLimit{RPS: githubBudget / 3600.0, Burst: 10}

// secretString hides a token from fmt and slog.
type secretString string

func (secretString) String() string               { return "[REDACTED]" }
func (secretString) GoString() string             { return "[REDACTED]" }
func (secretString) LogValue() slog.Value         { return slog.StringValue("[REDACTED]") }
func (secretString) MarshalText() ([]byte, error) { return []byte("[REDACTED]"), nil }

// LoadRepoList reads a watch list: one owner/name per line; blank lines and
// lines starting with # are skipped. Every repository must be a valid
// github-release coordinate and appear once.
func LoadRepoList(path string) ([]string, error) {
	f, err := os.Open(path) //nolint:gosec // the operator names the list
	if err != nil {
		return nil, fmt.Errorf("watch list: %w (generate it with `make bench-github-repos`, bench/README.md)", err)
	}
	defer func() { _ = f.Close() }()
	var (
		repos []string
		seen  = map[string]int{}
		check = githubrelease.New("", http.DefaultClient)
	)
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		t := source.Target{Kind: githubrelease.Kind, Coordinates: map[string]string{"repo": line}}
		if err := check.Validate(t); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		key := strings.ToLower(line)
		if first, dup := seen[key]; dup {
			return nil, fmt.Errorf("%s:%d: %s already on line %d", path, n, line, first)
		}
		seen[key] = n
		repos = append(repos, line)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(repos) == 0 {
		return nil, fmt.Errorf("%s: no repositories", path)
	}
	return repos, nil
}

// githubWatches are the targets of benchmark 3: the first n repositories of
// the list, or, against the fake registry, n synthetic ones.
func githubWatches(o options) ([]source.Target, error) {
	var repos []string
	if o.githubFake {
		for i := range o.n {
			repos = append(repos, "bench/"+fakereg.PackageName(i))
		}
	} else {
		all, err := LoadRepoList(o.githubRepos)
		if err != nil {
			return nil, err
		}
		if o.n > len(all) {
			return nil, fmt.Errorf("-n %d: the watch list %s has %d repositories", o.n, o.githubRepos, len(all))
		}
		repos = all[:o.n]
	}
	out := make([]source.Target, len(repos))
	for i, r := range repos {
		out[i] = source.Target{Kind: githubrelease.Kind, Coordinates: map[string]string{"repo": r}}
	}
	return out, nil
}

// WriteGitHubConfig writes the instance configuration of benchmark 3: only
// the github-release source. Live (registry ""), it keeps the default base
// URL, adds nothing to http (api.github.com is public, and its built-in
// budget applies) and names the token variable, never the token. Against
// the fake registry it is set up like benchmark 1, without a token.
func WriteGitHubConfig(path, listen, registry, dataDir, allowNetwork string, limit HostLimit) error {
	c := angeliaConfig{
		Listen:   listen,
		Roles:    []string{"all"},
		Database: map[string]string{"dsn": "sqlite://" + filepath.ToSlash(filepath.Join(dataDir, "angelia.db"))},
		Log:      map[string]string{"level": "info", "format": "json"},
		Sources:  map[string]map[string]string{githubrelease.Kind: {"token_env": githubTokenChildEnv}},
	}
	if registry != "" {
		limit = limit.effective()
		c.HTTP = map[string]any{
			"allow_networks": strings.Split(allowNetwork, ","),
			"host_limits": map[string]any{"hosts": map[string]any{
				registryHost(registry): map[string]any{"rps": limit.RPS, "burst": limit.Burst},
			}},
		}
		c.Sources[githubrelease.Kind] = map[string]string{"base_url": registry + "/github"}
	}
	return writeYAML(path, c)
}

// GitHubSample is one scrape of angelia's counters for the GitHub host and
// source, plus GitHub's own view of the token's budget.
type GitHubSample struct {
	At time.Time `json:"at"`
	// Responses are angelia_netguard_responses_total for the host, by class.
	Responses map[string]float64 `json:"responses"`
	// Fetches are angelia_source_fetches_total for github-release, by result.
	Fetches map[string]float64 `json:"fetches"`
	// Pauses are angelia_netguard_server_pauses_total for the host, by reason.
	Pauses map[string]float64 `json:"server_pauses,omitempty"`
	// DeadlineRefusals is angelia_netguard_deadline_refusals_total for the host.
	DeadlineRefusals float64 `json:"deadline_refusals"`
	// RateLimit is GET /rate_limit's core resource; nil when not read.
	RateLimit *RateLimitCore `json:"rate_limit,omitempty"`
}

// RateLimitCore is the "core" resource of GitHub's GET /rate_limit.
type RateLimitCore struct {
	Limit     int64 `json:"limit"`
	Remaining int64 `json:"remaining"`
	Used      int64 `json:"used"`
	Reset     int64 `json:"reset"`
}

// GitHubResult is benchmark 3's measurement.
type GitHubResult struct {
	Mode string `json:"mode"` // live or fake
	Host string `json:"host"` // the host label of the counters
	// WatchList is the list the watches come from (live).
	WatchList string `json:"watch_list,omitempty"`
	// CycleAt is the end of the first cycle: the first scrape after the
	// warm-up at which every watch had been fetched at least once.
	CycleAt time.Time `json:"first_cycle_done_at"`
	// Run and Window are the counters over the whole run and after the
	// first cycle (zero when the cycle did not end).
	Run    GitHubSample `json:"run"`
	Window GitHubSample `json:"after_first_cycle"`
	// ETagHitRate is not_modified / (ok + not_modified) checks in Window;
	// RequestHitRate the same over responses (304 / (2xx + 304)).
	ETagHitRate    float64 `json:"etag_hit_rate"`
	RequestHitRate float64 `json:"request_304_rate"`
	WindowChecks   float64 `json:"window_checks"`
	// Charged are the responses GitHub charges to the primary limit: all
	// but 304 and attempts without a response. PeakHour is the most in any
	// 60 minutes of the run (the whole run if it is shorter).
	Charged         float64   `json:"charged_requests"`
	ChargedPerHour  float64   `json:"charged_requests_per_hour"`
	PeakHour        float64   `json:"charged_requests_peak_60m"`
	PeakHourEndedAt time.Time `json:"charged_requests_peak_60m_end"`
	// Remaining* are GitHub's view (GET /rate_limit), when it could be read.
	RemainingStart    *RateLimitCore `json:"rate_limit_start,omitempty"`
	RemainingLowWater *RateLimitCore `json:"rate_limit_low_water,omitempty"`
	RemainingLowAt    time.Time      `json:"rate_limit_low_water_at"`
	Samples           []GitHubSample `json:"samples"`
}

// chargedClasses are the response classes GitHub charges to the primary
// rate limit: every response except 304. Counting 403 and 429 is the
// conservative reading.
var chargedClasses = []string{"2xx", "3xx", "403", "4xx", "429", "5xx", "other"}

func (s GitHubSample) charged() float64 {
	var n float64
	for _, c := range chargedClasses {
		n += s.Responses[c]
	}
	return n
}

func (s GitHubSample) checks() float64 {
	var n float64
	for _, v := range s.Fetches {
		n += v
	}
	return n
}

// minus returns the counters of s since earlier.
func (s GitHubSample) minus(earlier GitHubSample) GitHubSample {
	sub := func(a, b map[string]float64) map[string]float64 {
		out := map[string]float64{}
		for k, v := range a {
			out[k] = v - b[k]
		}
		return out
	}
	return GitHubSample{
		At: s.At, Responses: sub(s.Responses, earlier.Responses), Fetches: sub(s.Fetches, earlier.Fetches),
		Pauses: sub(s.Pauses, earlier.Pauses), DeadlineRefusals: s.DeadlineRefusals - earlier.DeadlineRefusals,
	}
}

// counterLine is one sample line of a Prometheus text exposition.
type counterLine struct {
	labels map[string]string
	value  float64
}

// parseCounters returns the samples of metric name. Label values must not
// hold `",` (angelia's closed label sets do not).
func parseCounters(body, name string) []counterLine {
	var out []counterLine
	for line := range strings.SplitSeq(body, "\n") {
		rest, ok := strings.CutPrefix(line, name+"{")
		if !ok {
			continue
		}
		labels, value, ok := strings.Cut(rest, "} ")
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(strings.Fields(value)[0], 64)
		if err != nil {
			continue
		}
		l := map[string]string{}
		for kv := range strings.SplitSeq(labels, `",`) {
			k, v, ok := strings.Cut(kv, `="`)
			if ok {
				l[k] = strings.TrimSuffix(v, `"`)
			}
		}
		out = append(out, counterLine{l, v})
	}
	return out
}

// sumBy sums the samples of name whose label match equals value, by label key.
func sumBy(body, name, match, value, key string) map[string]float64 {
	out := map[string]float64{}
	for _, c := range parseCounters(body, name) {
		if c.labels[match] == value {
			out[c.labels[key]] += c.value
		}
	}
	return out
}

// githubCounters reads the benchmark's counters from a /metrics body.
func githubCounters(body, host string, at time.Time) GitHubSample {
	return GitHubSample{
		At:               at,
		Responses:        sumBy(body, "angelia_netguard_responses_total", "host", host, "class"),
		Fetches:          sumBy(body, "angelia_source_fetches_total", "source", githubrelease.Kind, "result"),
		Pauses:           sumBy(body, "angelia_netguard_server_pauses_total", "host", host, "reason"),
		DeadlineRefusals: sumBy(body, "angelia_netguard_deadline_refusals_total", "host", host, "host")[host],
	}
}

// scrapeMetrics returns the body of base+"/metrics".
func scrapeMetrics(ctx context.Context, base string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, scrapeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/metrics", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // loopback URL of the instance under test
	if err != nil {
		return "", fmt.Errorf("scrape metrics: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("scrape metrics: %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return string(b), err
}

// readRateLimit calls GET base/rate_limit, which does not count against
// GitHub's primary rate limit. The token is sent only to base.
func readRateLimit(ctx context.Context, base string, tok secretString) (*RateLimitCore, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/rate_limit", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+string(tok))
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // GitHub's API, or the fake registry
	if err != nil {
		return nil, fmt.Errorf("GET /rate_limit: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /rate_limit: %s", resp.Status)
	}
	var body struct {
		Resources struct {
			Core *RateLimitCore `json:"core"`
		} `json:"resources"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil || body.Resources.Core == nil {
		return nil, errors.New("GET /rate_limit: no core resource in the answer")
	}
	return body.Resources.Core, nil
}

// awaitFreshBudget waits, before the run, until the token's window has
// (nearly) its whole budget: a run that starts on a budget another tool
// used up would meet 403s that are not angelia's.
func awaitFreshBudget(ctx context.Context, base string, tok secretString, log *slog.Logger, now func() time.Time) error {
	rl, err := readRateLimit(ctx, base, tok)
	if err != nil {
		return fmt.Errorf("cannot read the token's rate limit (is %s a valid token?): %w", githubTokenEnv, err)
	}
	if rl.Limit <= 0 || float64(rl.Remaining) >= 0.99*float64(rl.Limit) {
		return nil
	}
	wait := time.Unix(rl.Reset, 0).Add(5 * time.Second).Sub(now())
	if wait <= 0 {
		return nil
	}
	log.Info("the token's rate-limit window is partly used; waiting for it to reset", "remaining", rl.Remaining, "limit", rl.Limit, "wait", wait.Round(time.Second))
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}

// benchGitHub runs benchmark 3.
func benchGitHub(ctx context.Context, o options, log *slog.Logger) (*Report, error) {
	var err error
	if o.bin, err = filepath.Abs(o.bin); err != nil {
		return nil, err
	}
	workdir, err := filepath.Abs(o.out)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(workdir, 0o750); err != nil {
		return nil, err
	}
	watches, err := githubWatches(o)
	if err != nil {
		return nil, err
	}
	gh := &GitHubResult{Mode: "live", Host: githubHost}
	registry, listen, angeliaDir, allow, apiBase := "", "", workdir, "", o.githubAPI
	var reg *fakereg.Server
	if o.githubFake {
		cfg, err := fakereg.LoadConfig(o.fakeregConfig)
		if err != nil {
			return nil, err
		}
		SizeRegistry(cfg, map[string]int{"github": len(watches)})
		host := "127.0.0.1"
		if o.container {
			host = bindHost(ctx, o.probe.run, o.probe.goos, log)
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
		if err != nil {
			return nil, err
		}
		reg = fakereg.New(cfg)
		srv := &http.Server{Handler: reg, ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = srv.Serve(ln) }()
		defer func() { _ = srv.Close() }()
		port := ln.Addr().(*net.TCPAddr).Port
		registry = fmt.Sprintf("http://127.0.0.1:%d", port)
		apiBase = registry + "/github"
		if o.container {
			registry = fmt.Sprintf("http://host.docker.internal:%d", port)
		}
		if allow = o.allowNetwork; allow == "" {
			if allow, err = registryNetwork(o.container, host); err != nil {
				if allow, err = discoverHostGateway(ctx, o.probe.run); err != nil {
					return nil, err
				}
			}
		}
		gh.Mode, gh.Host = "fake", registryHost(registry)
	} else {
		gh.WatchList = o.githubRepos
	}
	if o.container {
		listen, angeliaDir = ":8080", "/work"
	}
	t := o.newTarget(o, workdir)
	if pt, ok := t.(*processTarget); ok {
		listen = pt.listen
	}
	serveArgs, err := o.loader.Prepare(ctx, watches, workdir, angeliaDir)
	if err != nil {
		return nil, err
	}
	if err := WriteGitHubConfig(filepath.Join(workdir, "angelia.yaml"), listen, registry, angeliaDir, allow, o.hostLimit); err != nil {
		return nil, err
	}
	title := "Phase 0 benchmark 3: GitHub, one token"
	if o.githubFake {
		title += " (dry run against the fake registry)"
	}
	rep := &Report{
		Benchmark: title,
		Params: Params{
			Kind: o.kind, Watches: len(watches), PerEcosystem: map[string]int{"github": len(watches)}, Loader: o.loader.Name(),
			Duration: o.duration.String(), SampleInterval: o.interval.String(), StartedAt: time.Now().UTC(),
			CheckInterval: o.checkInterval.String(), SchedulerWorkers: scheduler.DefaultWorkers,
		},
		GitHub: gh,
	}
	if o.githubFake {
		rep.Params.FakeregConfig = o.fakeregConfig
		rep.Params.HostLimit = registryHostLimit(registry, o.hostLimit)
	}
	rep.Environment = o.probe.environment(ctx, o.bin, t, o.container)
	if o.waitBudget && !o.githubFake {
		if err := awaitFreshBudget(ctx, apiBase, o.GitHubToken, log, time.Now); err != nil {
			return nil, err
		}
	}
	readLimit := func() *RateLimitCore {
		rl, err := readRateLimit(ctx, apiBase, o.GitHubToken)
		if err != nil {
			return nil
		}
		return rl
	}
	gh.RemainingStart = readLimit()
	log.Info("starting angelia", "benchmark", "github", "mode", rep.Environment.Mode, "github", gh.Mode, "watches", len(watches), "duration", o.duration)
	startedAt := time.Now()
	if err := t.Start(ctx, filepath.Join(angeliaDir, "angelia.yaml"), serveArgs); err != nil {
		return nil, err
	}
	defer func() { _ = t.Stop(context.WithoutCancel(ctx)) }()
	if err := waitReady(ctx, t, o.readyTimeout); err != nil {
		return nil, err
	}
	readyAt := time.Now()
	end := readyAt.Add(o.duration)
	rep.Params.ReadyAt = readyAt.UTC()
	base, err := t.HealthURL(ctx)
	if err != nil {
		return nil, err
	}
	// The counters start at zero with the process.
	gh.Samples = []GitHubSample{{At: startedAt.UTC(), Responses: map[string]float64{}, Fetches: map[string]float64{}}}
	var cycle *GitHubSample
	scrape := func() bool {
		body, err := scrapeMetrics(ctx, base)
		if err != nil {
			log.Warn("cannot read angelia's metrics", "err", err)
			return false
		}
		s := githubCounters(body, gh.Host, time.Now().UTC())
		s.RateLimit = readLimit()
		gh.Samples = append(gh.Samples, s)
		if cycle == nil && !s.At.Before(readyAt.Add(warmup(o.checkInterval))) && s.checks() >= float64(len(watches)) {
			c := s
			cycle, gh.CycleAt = &c, s.At
		}
		return true
	}
	tick := time.NewTicker(o.interval)
	defer tick.Stop()
	var lastScrape time.Time
	for {
		s, err := t.Sample(ctx)
		if err != nil {
			return nil, fmt.Errorf("sample: %w", err)
		}
		rep.Samples = append(rep.Samples, s)
		done := !time.Now().Before(end)
		if done || time.Since(lastScrape) >= o.metricsEvery {
			if scrape() {
				lastScrape = time.Now()
			}
		}
		if done {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
	gh.finish(cycle)
	if reg != nil {
		rep.Registry = reg.StatsIn(fakereg.Window{From: readyAt, To: end})
	}
	rep.Resources = summarizeSamples(rep.Samples)
	rep.Resources.MemorySource = "RSS (VmRSS)"
	if o.container {
		rep.Resources.MemorySource = "container memory (docker stats)"
	}
	rep.evaluateGitHub(o.duration)
	return rep, rep.Write(workdir)
}

// finish computes the totals from the samples; cycle is the sample that
// ended the first cycle, nil if it did not end.
func (g *GitHubResult) finish(cycle *GitHubSample) {
	if len(g.Samples) < 2 {
		return
	}
	last := g.Samples[len(g.Samples)-1]
	g.Run = last.minus(g.Samples[0])
	g.Charged = g.Run.charged()
	if h := last.At.Sub(g.Samples[0].At).Hours(); h > 0 {
		g.ChargedPerHour = g.Charged / h
	}
	for i, s := range g.Samples {
		// The latest sample at least 60 minutes before s, else the start.
		j := 0
		for k := i; k >= 0; k-- {
			if s.At.Sub(g.Samples[k].At) >= time.Hour {
				j = k
				break
			}
		}
		if d := s.charged() - g.Samples[j].charged(); d > g.PeakHour {
			g.PeakHour, g.PeakHourEndedAt = d, s.At
		}
		if rl := s.RateLimit; rl != nil && (g.RemainingLowWater == nil || rl.Remaining < g.RemainingLowWater.Remaining) {
			g.RemainingLowWater, g.RemainingLowAt = rl, s.At
		}
	}
	if cycle == nil {
		return
	}
	g.Window = last.minus(*cycle)
	f, r := g.Window.Fetches, g.Window.Responses
	g.WindowChecks = g.Window.checks()
	if n := f["ok"] + f["not_modified"]; n > 0 {
		g.ETagHitRate = f["not_modified"] / n
	}
	if n := r["2xx"] + r["304"]; n > 0 {
		g.RequestHitRate = r["304"] / n
	}
}

// evaluateGitHub fills the checks of benchmark 3 (#64's acceptance
// criteria) and the verdict.
func (r *Report) evaluateGitHub(duration time.Duration) {
	g := r.GitHub
	measured := len(g.Samples) > 1
	windowed := !g.CycleAt.IsZero() && g.Window.Fetches["ok"]+g.Window.Fetches["not_modified"] > 0
	r.Checks = []Check{
		check("403 responses from "+g.Host, "0", fmt.Sprintf("%.0f", g.Run.Responses["403"]), measured, g.Run.Responses["403"] == 0),
		check("429 responses from "+g.Host, "0", fmt.Sprintf("%.0f", g.Run.Responses["429"]), measured, g.Run.Responses["429"] == 0),
		check("ETag hit rate after the first cycle", fmt.Sprintf("> %.0f%%", targetETagHitRate*100),
			fmt.Sprintf("%.2f%% of %.0f checks", g.ETagHitRate*100, g.Window.Fetches["ok"]+g.Window.Fetches["not_modified"]),
			windowed, g.ETagHitRate > targetETagHitRate),
		check("requests charged to the token, busiest 60 min", fmt.Sprintf("< %d (GitHub's limit per hour)", githubBudget),
			fmt.Sprintf("%.0f", g.PeakHour), measured, g.PeakHour < githubBudget),
	}
	low := check("rate limit remaining, low-water (GET /rate_limit)", "> 0", "-", g.RemainingLowWater != nil,
		g.RemainingLowWater != nil && g.RemainingLowWater.Remaining > 0)
	if g.RemainingLowWater != nil {
		low.Actual = fmt.Sprintf("%d of %d", g.RemainingLowWater.Remaining, g.RemainingLowWater.Limit)
	}
	low.Optional = true
	r.Checks = append(r.Checks, low)
	if g.Mode == "fake" {
		r.Notes = append(r.Notes, "dry run against the fake registry's github endpoint: it tests the harness and angelia's pacing, not api.github.com")
	}
	if duration < githubRunHours*time.Hour {
		r.Notes = append(r.Notes, fmt.Sprintf("measured for %v, less than the %d h of angelia-dev/angelia#64: a smoke run, not the benchmark", duration, githubRunHours))
	}
	if g.CycleAt.IsZero() && measured {
		r.Notes = append(r.Notes, "the first cycle did not end (not every watch was fetched after the warm-up): the ETag hit rate is not measured; raise BENCH_DURATION")
	}
	if g.Run.Responses["4xx"] > 0 {
		r.Notes = append(r.Notes, fmt.Sprintf("%.0f other 4xx responses (404: a repository renamed away or deleted; see angelia.log)", g.Run.Responses["4xx"]))
	}
	r.verdict()
}

// githubMarkdown renders benchmark 3's section.
func (g *GitHubResult) markdown(interval string, watches int) string {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	p("\n## GitHub\n")
	src := "api.github.com"
	if g.Mode == "fake" {
		src = "the fake registry (" + g.Host + ")"
	} else {
		p("Watch list `%s`.\n", g.WatchList)
	}
	p("%d github-release watches against %s at a check interval of %s. Counts are angelia's own metrics (`angelia_netguard_responses_total{host=%q}`, one per attempt; `angelia_source_fetches_total{source=\"github-release\"}`, one per check).\n",
		watches, src, interval, g.Host)
	if g.CycleAt.IsZero() {
		p("The first cycle did not end, so there is no window after it.\n")
	} else {
		p("First cycle done at %s (every watch fetched at least once, after the warm-up).\n", g.CycleAt.Format(time.RFC3339))
	}
	classes := []string{"2xx", "304", "3xx", "403", "4xx", "429", "5xx", "error"}
	p("| Responses | %s |\n|---|%s", strings.Join(classes, " | "), strings.Repeat("---|", len(classes)))
	row := func(name string, m map[string]float64) {
		var cells []string
		for _, c := range classes {
			cells = append(cells, fmt.Sprintf("%.0f", m[c]))
		}
		p("| %s | %s |", name, strings.Join(cells, " | "))
	}
	row("whole run", g.Run.Responses)
	row("after the first cycle", g.Window.Responses)
	results := []string{"ok", "not_modified", "rate_limited", "error", "canceled"}
	p("\n| Checks | %s |\n|---|%s", strings.Join(results, " | "), strings.Repeat("---|", len(results)))
	rowF := func(name string, m map[string]float64) {
		var cells []string
		for _, c := range results {
			cells = append(cells, fmt.Sprintf("%.0f", m[c]))
		}
		p("| %s | %s |", name, strings.Join(cells, " | "))
	}
	rowF("whole run", g.Run.Fetches)
	rowF("after the first cycle", g.Window.Fetches)
	p("\nETag hit rate after the first cycle: **%.2f%%** of checks answered 304 (%.2f%% of 2xx+304 responses; later pages of a changed listing are never conditional).\n",
		g.ETagHitRate*100, g.RequestHitRate*100)
	pauses := slices.Sorted(maps.Keys(g.Run.Pauses))
	var ps []string
	for _, k := range pauses {
		ps = append(ps, fmt.Sprintf("%s %.0f", k, g.Run.Pauses[k]))
	}
	if len(ps) == 0 {
		ps = []string{"none"}
	}
	hours := 0.0
	if len(g.Samples) > 1 {
		hours = g.Samples[len(g.Samples)-1].At.Sub(g.Samples[0].At).Hours()
	}
	p("Budget: %.0f requests charged to the token over the run's %.2f h (every response but 304; the first cycle included), %.0f per hour on average; busiest 60 minutes (the whole run if shorter) %.0f, ending %s, against GitHub's %d per hour. Server pauses: %s. Deadline refusals: %.0f.\n",
		g.Charged, hours, g.ChargedPerHour, g.PeakHour, g.PeakHourEndedAt.Format(time.RFC3339), githubBudget, strings.Join(ps, ", "), g.Run.DeadlineRefusals)
	if g.RemainingStart != nil || g.RemainingLowWater != nil {
		p("GitHub's view (`GET /rate_limit`, core, free to call), sampled with the metrics:")
		if s := g.RemainingStart; s != nil {
			p("- at the start: %d of %d remaining", s.Remaining, s.Limit)
		}
		if l := g.RemainingLowWater; l != nil {
			p("- low-water mark: %d of %d remaining at %s", l.Remaining, l.Limit, g.RemainingLowAt.Format(time.RFC3339))
		}
		p("\nThe budget belongs to the token's user: anything else using it during the run shows here, not in angelia's counters.")
	} else {
		p("GitHub's `GET /rate_limit` could not be read: the budget is judged from angelia's counters alone.")
	}
	return b.String()
}
