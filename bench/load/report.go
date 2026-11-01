package main

import (
	"context"
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/angelia-dev/angelia/bench/fakereg"
)

// Targets of benchmark 1, architecture.md section 6.
const (
	targetLagP95 = 2 * time.Minute
	targetRSSMiB = 100.0
	targetCPUPct = 20.0
)

// Report is the benchmark result, written as report.json and report.md.
type Report struct {
	Benchmark   string        `json:"benchmark"`
	Verdict     string        `json:"verdict"` // pass, fail or incomplete
	Notes       []string      `json:"notes,omitempty"`
	Environment Environment   `json:"environment"`
	Params      Params        `json:"params"`
	Resources   Resources     `json:"resources"`
	Registry    fakereg.Stats `json:"registry"`
	Checks      []Check       `json:"checks"`
	Samples     []Sample      `json:"samples"`
}

// Environment is where the benchmark ran.
type Environment struct {
	Mode           string `json:"mode"` // container or process
	CPUModel       string `json:"cpu_model"`
	HostCPUs       int    `json:"host_cpus"`
	HostMemory     string `json:"host_memory"`
	CPULimit       string `json:"cpu_limit"`
	MemoryLimit    string `json:"memory_limit"`
	OS             string `json:"os"`
	Kernel         string `json:"kernel,omitempty"`
	Docker         string `json:"docker,omitempty"`
	AngeliaGo      string `json:"angelia_go_version"`
	HarnessGo      string `json:"harness_go_version"`
	Commit         string `json:"commit"`
	CommitModified bool   `json:"commit_modified"`
}

// Params are the inputs of the run.
type Params struct {
	Watches        int       `json:"watches"`
	PerEcosystem   int       `json:"packages_per_ecosystem"`
	Loader         string    `json:"watch_loader"`
	Duration       string    `json:"duration"`
	SampleInterval string    `json:"sample_interval"`
	FakeregConfig  string    `json:"fakereg_config"`
	StartedAt      time.Time `json:"started_at"`
}

// Resources summarizes the samples.
type Resources struct {
	RSSMaxMiB  float64 `json:"rss_max_mib"`
	RSSP95MiB  float64 `json:"rss_p95_mib"`
	CPUMeanPct float64 `json:"cpu_mean_percent"`
	CPUP95Pct  float64 `json:"cpu_p95_percent"`
}

// Check is one pass/fail target.
type Check struct {
	Name   string `json:"name"`
	Target string `json:"target"`
	Actual string `json:"actual"`
	Result string `json:"result"` // pass, fail, not measured
}

func summarizeSamples(s []Sample) Resources {
	if len(s) == 0 {
		return Resources{}
	}
	rss, cpu := make([]float64, len(s)), make([]float64, 0, len(s))
	var sum float64
	for i, x := range s {
		rss[i] = float64(x.RSSBytes) / (1 << 20)
		if i > 0 { // the first sample has no CPU interval
			cpu = append(cpu, x.CPUPercent)
			sum += x.CPUPercent
		}
	}
	r := Resources{RSSMaxMiB: slices.Max(rss), RSSP95MiB: p95(rss)}
	if len(cpu) > 0 {
		r.CPUMeanPct, r.CPUP95Pct = sum/float64(len(cpu)), p95(cpu)
	}
	return r
}

func p95(v []float64) float64 {
	v = slices.Clone(v)
	slices.Sort(v)
	i := max(int(float64(len(v))*0.95+0.999999)-1, 0)
	return v[min(i, len(v)-1)]
}

// evaluate fills Checks and Verdict. Lag is only measured when angelia had
// watches and releases were published during the run.
func (r *Report) evaluate(loaderErr error) {
	lag := r.Registry.Total.Lag
	lagMeasured := loaderErr == nil && r.Registry.Total.Published > 0
	r.Checks = []Check{
		check("check lag P95", "< 2m0s", fmt.Sprintf("%.1fs over %d releases", lag.P95Seconds, lag.Samples),
			lagMeasured, lag.P95Seconds < targetLagP95.Seconds()),
		check("RSS max", fmt.Sprintf("< %.0f MiB", targetRSSMiB), fmt.Sprintf("%.1f MiB", r.Resources.RSSMaxMiB),
			len(r.Samples) > 0, r.Resources.RSSMaxMiB < targetRSSMiB),
		check("CPU mean", fmt.Sprintf("< %.0f%%", targetCPUPct), fmt.Sprintf("%.1f%%", r.Resources.CPUMeanPct),
			len(r.Samples) > 1, r.Resources.CPUMeanPct < targetCPUPct),
	}
	if loaderErr != nil {
		r.Notes = append(r.Notes, loaderErr.Error()+". Resources below are of an idle instance.")
	} else if r.Registry.Total.Published == 0 {
		r.Notes = append(r.Notes, "no release was published during the run: raise -duration or releases_per_hour")
	}
	r.Verdict = "pass"
	for _, c := range r.Checks {
		switch c.Result {
		case "fail":
			r.Verdict = "fail"
		case "not measured":
			if r.Verdict == "pass" {
				r.Verdict = "incomplete"
			}
		}
	}
	if loaderErr != nil && r.Verdict == "pass" {
		r.Verdict = "incomplete"
	}
}

func check(name, target, actual string, measured, ok bool) Check {
	c := Check{Name: name, Target: target, Actual: actual, Result: "fail"}
	switch {
	case !measured:
		c.Result, c.Actual = "not measured", "-"
	case ok:
		c.Result = "pass"
	}
	return c
}

// Write writes report.json and report.md into dir.
func (r *Report) Write(dir string) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), append(data, '\n'), 0o600); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(dir, "report.md")) //nolint:gosec // the operator names the output directory
	if err != nil {
		return err
	}
	if err := r.Markdown(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Markdown renders the human-readable report.
func (r *Report) Markdown(w io.Writer) error {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	e := r.Environment
	p("# %s\n\n**Verdict: %s**\n", r.Benchmark, r.Verdict)
	for _, n := range r.Notes {
		p("> %s\n", n)
	}
	p("## Checks\n\n| Check | Target | Actual | Result |\n|---|---|---|---|")
	for _, c := range r.Checks {
		p("| %s | %s | %s | %s |", c.Name, c.Target, c.Actual, c.Result)
	}
	p("\n## Environment\n\n| | |\n|---|---|")
	for _, kv := range [][2]string{
		{"Mode", e.Mode},
		{"CPU", e.CPUModel},
		{"Host CPUs", strconv.Itoa(e.HostCPUs)},
		{"Host memory", e.HostMemory},
		{"CPU limit", e.CPULimit},
		{"Memory limit", e.MemoryLimit},
		{"OS", e.OS},
		{"Kernel", e.Kernel},
		{"Docker", e.Docker},
		{"Go (angelia)", e.AngeliaGo},
		{"Go (harness)", e.HarnessGo},
		{"Commit", e.Commit + map[bool]string{true: " (modified)"}[e.CommitModified]},
	} {
		if kv[1] != "" {
			p("| %s | %s |", kv[0], kv[1])
		}
	}
	pr := r.Params
	p("\n## Run\n\n%d watches (%d packages per ecosystem), watch loader `%s`, %s, sampled every %s, fake registry config `%s`, started %s.",
		pr.Watches, pr.PerEcosystem, pr.Loader, pr.Duration, pr.SampleInterval, pr.FakeregConfig, pr.StartedAt.Format(time.RFC3339))
	rs := r.Resources
	p("\nRSS max %.1f MiB, P95 %.1f MiB; CPU mean %.1f%%, P95 %.1f%% (%d samples).",
		rs.RSSMaxMiB, rs.RSSP95MiB, rs.CPUMeanPct, rs.CPUP95Pct, len(r.Samples))
	p("\n## Fake registry\n\n| Ecosystem | Requests | 200 | 304 | 429 | 404 | Published | Seen | Lag P50 | Lag P95 | Lag max |\n|---|---|---|---|---|---|---|---|---|---|---|")
	row := func(name string, s fakereg.EcoStats) {
		p("| %s | %d | %d | %d | %d | %d | %d | %d | %.1fs | %.1fs | %.1fs |", name, s.Requests, s.OK, s.NotModified,
			s.Throttled, s.NotFound, s.Published, s.Seen, s.Lag.P50Seconds, s.Lag.P95Seconds, s.Lag.MaxSeconds)
	}
	for _, name := range fakereg.Ecosystems {
		row(name, r.Registry.Ecosystems[name])
	}
	row("**total**", r.Registry.Total)
	_, err := io.WriteString(w, b.String())
	return err
}

// probe gathers environment facts; tests replace its functions.
type probe struct {
	readFile func(string) ([]byte, error)
	run      runFunc
	goos     string
}

func (pr probe) environment(ctx context.Context, bin string, t Target, container bool) Environment {
	e := Environment{Mode: "process", HostCPUs: runtime.NumCPU(), OS: pr.goos + "/" + runtime.GOARCH, HarnessGo: runtime.Version()}
	if container {
		e.Mode = "container"
		e.Docker = pr.out(ctx, "docker", "version", "--format", "{{.Server.Version}} ({{.Server.Os}}/{{.Server.Arch}})")
	}
	e.CPULimit, e.MemoryLimit = t.Limits()
	switch pr.goos {
	case "linux":
		e.CPUModel = procField(pr.readFile, "/proc/cpuinfo", "model name")
		if kb, err := strconv.ParseInt(strings.TrimSuffix(procField(pr.readFile, "/proc/meminfo", "MemTotal"), " kB"), 10, 64); err == nil {
			e.HostMemory = fmt.Sprintf("%.1f GiB", float64(kb)/(1<<20))
		}
		if b, err := pr.readFile("/proc/sys/kernel/osrelease"); err == nil {
			e.Kernel = strings.TrimSpace(string(b))
		}
	case "darwin":
		e.CPUModel = pr.out(ctx, "sysctl", "-n", "machdep.cpu.brand_string")
		if n, err := strconv.ParseInt(pr.out(ctx, "sysctl", "-n", "hw.memsize"), 10, 64); err == nil {
			e.HostMemory = fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
		}
	}
	if bi, err := buildinfo.ReadFile(bin); err == nil {
		e.AngeliaGo = bi.GoVersion
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				e.Commit = s.Value
			case "vcs.modified":
				e.CommitModified = s.Value == "true"
			}
		}
	}
	if e.Commit == "" {
		e.Commit = pr.out(ctx, "git", "rev-parse", "HEAD")
		e.CommitModified = pr.out(ctx, "git", "status", "--porcelain") != ""
	}
	return e
}

func (pr probe) out(ctx context.Context, name string, args ...string) string {
	b, err := pr.run(ctx, name, args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// procField returns the value of the first "key: value" line.
func procField(read func(string) ([]byte, error), path, key string) string {
	b, err := read(path)
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
