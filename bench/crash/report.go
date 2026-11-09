package main

import (
	"context"
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Report is the result of a run, written as report.json and report.md.
type Report struct {
	Benchmark   string        `json:"benchmark"`
	Verdict     string        `json:"verdict"` // pass, fail or incomplete
	Started     time.Time     `json:"started"`
	Duration    time.Duration `json:"duration_ns"`
	Notes       []string      `json:"notes,omitempty"`
	Environment Environment   `json:"environment"`
	Params      Params        `json:"params"`
	Phases      Phases        `json:"phases"`
	Checks      []Check       `json:"checks"`
	Tally       Tally         `json:"tally"`
	Kills       []Kill        `json:"kills"`
}

// Params are the knobs of the run.
type Params struct {
	Workload     workload      `json:"workload"`
	Kills        int           `json:"kills"`
	MinUp, MaxUp time.Duration `json:"-"`
	Uptime       string        `json:"uptime"`
	ReceiverWait time.Duration `json:"receiver_delay_max_ns"`
	Seed         uint64        `json:"seed"`
	MinFlow      int           `json:"min_flow"`
	MinPending   int           `json:"min_pending"`
	Stages       string        `json:"stages"`
}

// Phases are the times the run spent in each phase.
type Phases struct {
	Warmup   time.Duration `json:"warmup_ns"`
	KillLoop time.Duration `json:"kill_loop_ns"`
	Drain    time.Duration `json:"drain_ns"`
	// ReceivedBeforeKills is what the receiver got before the first kill.
	ReceivedBeforeKills int `json:"received_before_kills"`
}

// Environment describes where the run happened.
type Environment struct {
	Note           string `json:"note,omitempty"`
	OS             string `json:"os"`
	Kernel         string `json:"kernel,omitempty"`
	CPUModel       string `json:"cpu_model,omitempty"`
	CPUs           int    `json:"cpus"`
	Memory         string `json:"memory,omitempty"`
	HarnessGo      string `json:"harness_go"`
	AngeliaGo      string `json:"angelia_go,omitempty"`
	Commit         string `json:"commit,omitempty"`
	CommitModified bool   `json:"commit_modified,omitempty"`
}

func environment(ctx context.Context, bin, note string) Environment {
	e := Environment{Note: note, OS: runtime.GOOS + "/" + runtime.GOARCH, CPUs: runtime.NumCPU(), HarnessGo: runtime.Version()}
	e.CPUModel = procField("/proc/cpuinfo", "model name")
	if kb, err := strconv.ParseInt(strings.TrimSuffix(procField("/proc/meminfo", "MemTotal"), " kB"), 10, 64); err == nil {
		e.Memory = fmt.Sprintf("%.1f GiB", float64(kb)/(1<<20))
	}
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		e.Kernel = strings.TrimSpace(string(b))
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
		if out, err := exec.CommandContext(ctx, "git", "rev-parse", "HEAD").Output(); err == nil {
			e.Commit = strings.TrimSpace(string(out))
		}
		if out, err := exec.CommandContext(ctx, "git", "status", "--porcelain").Output(); err == nil {
			e.CommitModified = len(strings.TrimSpace(string(out))) > 0
		}
	}
	return e
}

// procField returns the value of the first "key: value" line of a file.
func procField(path, key string) string {
	b, err := os.ReadFile(path) //nolint:gosec // fixed /proc paths
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

// setupChecks are the checks that the run did what the benchmark asks:
// the number of kills, each by SIGKILL, each while deliveries were waiting,
// enough of them while detection was running, and a drain that checked
// every target after the freeze and finished every delivery.
func setupChecks(want, minDetection int, kills []Kill, t Tally, drained bool) []Check {
	var busy, open, detecting int
	var idle []string
	for _, k := range kills {
		if k.Backlog.Pending+k.Backlog.InFlight > 0 {
			busy++
		} else {
			idle = append(idle, fmt.Sprintf("kill %d: no delivery pending or in flight", k.Index))
		}
		if k.Open > 0 {
			open++
		}
		if k.Checks > 0 {
			detecting++
		}
	}
	c := []Check{{
		Name: "SIGKILLs during the run", OK: len(kills) == want,
		Detail: fmt.Sprintf("%d of %d, each process ended by SIGKILL", len(kills), want),
	}}
	var ch checker
	ch.add("every kill during dispatch (deliveries pending or in flight)",
		fmt.Sprintf("%d of %d kills; %d cut requests off at the receiver", busy, len(kills), open), idle)
	c = append(c, ch.checks...)
	c = append(c, Check{
		Name: "kills during detection", OK: detecting >= minDetection,
		Detail: fmt.Sprintf("%d kills found checks committed since the previous kill (want at least %d)", detecting, minDetection),
	})
	return append(c, Check{
		Name: drainCheck, OK: drained && t.Unfinished == 0,
		Detail: fmt.Sprintf("%d deliveries pending or in flight at the end", t.Unfinished),
	})
}

// drainCheck names the setup check that the drain finished.
const drainCheck = "drain finished (every target checked after the freeze, every delivery finished)"

// decide sets the verdict: fail when a delivery check fails, incomplete
// when the run did not do what the benchmark asks, pass otherwise. When the
// drain did not finish, the registry checks can fail for versions angelia
// had no chance to fetch, so the run is incomplete whatever they say.
func (r *Report) decide(verify, setup []Check) {
	r.Checks = append(slices.Clone(setup), verify...)
	r.Verdict = "pass"
	drained := true
	for _, c := range setup {
		if !c.OK {
			r.Verdict = "incomplete"
		}
		if c.Name == drainCheck && !c.OK {
			drained = false
		}
	}
	if len(verify) == 0 {
		r.Verdict = "incomplete"
	}
	for _, c := range verify {
		if !c.OK && drained {
			r.Verdict = "fail"
		}
	}
}

// Write writes report.json and report.md into dir.
func (r *Report) Write(dir string) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), data, 0o600); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(dir, "report.md")) //nolint:gosec // the run's output directory
	if err != nil {
		return err
	}
	if err := r.Markdown(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func secs(d time.Duration) string { return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + "s" }

// Markdown writes the human report.
func (r *Report) Markdown(w io.Writer) error {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	p("# Benchmark 5: kill -9 during dispatch\n\n")
	p("**Verdict: %s.** Target ([architecture.md §6](../../docs/architecture.md#6-language-and-framework-phase-0-assumptions)): "+
		"zero lost deliveries, duplicates only within the idempotency key, over %d SIGKILLs during heavy dispatch.\n\n",
		strings.ToUpper(r.Verdict), r.Params.Kills)
	p("Run %s, %s in total (warm-up %s, kill loop %s, drain %s).\n\n",
		r.Started.UTC().Format(time.RFC3339), secs(r.Duration), secs(r.Phases.Warmup), secs(r.Phases.KillLoop), secs(r.Phases.Drain))
	for _, n := range r.Notes {
		p("- %s\n", n)
	}
	if len(r.Notes) > 0 {
		p("\n")
	}
	p("## Checks\n\n| Check | Result | Detail |\n|---|---|---|\n")
	for _, c := range r.Checks {
		res := "pass"
		if !c.OK {
			res = "**FAIL**"
		}
		p("| %s | %s | %s |\n", c.Name, res, c.Detail)
	}
	for _, c := range r.Checks {
		if len(c.Examples) > 0 {
			p("\n%s, first %d:\n\n", c.Name, len(c.Examples))
			for _, e := range c.Examples {
				p("- `%s`\n", e)
			}
		}
	}
	t := r.Tally
	p("\n## Counts\n\n| | |\n|---|---|\n")
	p("| Versions recorded (baseline, new) | %d, %d |\n", t.BaselineVersions, t.NewVersions)
	p("| Events | %d |\n| Deliveries (rows) | %d |\n", t.Events, t.Deliveries)
	p("| Deliveries attempted more than once | %d |\n", t.RetriedDeliveries)
	p("| Requests received | %d |\n| Distinct deliveries received | %d |\n", t.Received, t.Distinct)
	p("| Duplicate requests (deliveries received twice or more) | %d (%d) |\n", t.Duplicates, t.DuplicatedPairs)
	p("| Received before the first kill | %d |\n", r.Phases.ReceivedBeforeKills)
	p("| Expected from the registry: new versions, deliveries | %d, %d |\n", t.ExpectedVersions, t.Expected)
	if len(r.Kills) > 0 {
		var up, st []time.Duration
		var rec []int
		open, cut, early, detecting, withEvents := 0, 0, 0, 0, 0
		for _, k := range r.Kills {
			rec = append(rec, k.Received)
			if k.FromStart {
				early++
			} else {
				up = append(up, k.Uptime)
				if k.Startup > 0 {
					st = append(st, k.Startup)
				}
			}
			if k.Open > 0 {
				open++
				cut += k.Open
			}
			if k.Checks > 0 {
				detecting++
			}
			if k.Events > 0 {
				withEvents++
			}
		}
		slices.Sort(up)
		slices.Sort(rec)
		slices.Sort(st)
		p("\n## Kills\n\n| | |\n|---|---|\n")
		p("| Kills | %d |\n", len(r.Kills))
		p("| Kills timed from the process start, not waiting for /readyz (startup to ready: see below) | %d |\n", early)
		if len(up) > 0 {
			p("| Uptime after ready (drawn) | %s to %s, median %s |\n", secs(up[0]), secs(up[len(up)-1]), secs(up[len(up)/2]))
		}
		if len(st) > 0 {
			p("| Restart to ready | median %s, max %s |\n", secs(st[len(st)/2]), secs(st[len(st)-1]))
		}
		p("| Kills during detection (checks committed since the previous kill) | %d (%d with new events) |\n", detecting, withEvents)
		p("| Requests received per life | min %d, median %d, max %d |\n", rec[0], rec[len(rec)/2], rec[len(rec)-1])
		p("| Kills that cut requests off at the receiver | %d (%d requests) |\n", open, cut)
		p("\n<details><summary>Every kill</summary>\n\n| # | Timed from | Restart to ready | Uptime | Checks | Events | Received | Open at kill | Pending after | In flight after |\n|---|---|---|---|---|---|---|---|---|---|\n")
		for _, k := range r.Kills {
			from := "ready"
			if k.FromStart {
				from = "start"
			}
			p("| %d | %s | %s | %s | %d | %d | %d | %d | %d | %d |\n", k.Index, from, secs(k.Startup), secs(k.Uptime), k.Checks, k.Events, k.Received, k.Open, k.Backlog.Pending, k.Backlog.InFlight)
		}
		p("\n</details>\n")
	}
	w2 := r.Params.Workload
	p("\n## Parameters\n\n| | |\n|---|---|\n")
	p("| Watches (pypi, fake registry) | %d, interval 5m, %d versions each at start |\n", w2.Packages, w2.History)
	p("| Releases | %.0f per hour over all packages (one every %s per package) |\n", w2.ReleasesPerH,
		secs(time.Duration(float64(w2.Packages)/w2.ReleasesPerH*float64(time.Hour))))
	p("| Webhook sinks and subscriptions | %d, each matching every event |\n", w2.Subscriptions)
	p("| Kills | %d, uptime %s |\n", r.Params.Kills, r.Params.Uptime)
	p("| Warm-up | watches added in %s; a kill waits for %d pending deliveries |\n", r.Params.Stages, r.Params.MinPending)
	p("| Receiver delay | uniform in [0, %s) per request |\n", r.Params.ReceiverWait)
	p("| Dispatch starts the kills | after %d requests received |\n", r.Params.MinFlow)
	p("| Seed | %d |\n", r.Params.Seed)
	e := r.Environment
	p("\n## Environment\n\n| | |\n|---|---|\n")
	if e.Note != "" {
		p("| Machine | %s |\n", e.Note)
	}
	p("| Mode | process (angelia as a child process, no container limits) |\n")
	p("| OS, kernel | %s, %s |\n| CPU | %s, %d CPUs |\n| Memory | %s |\n", e.OS, e.Kernel, e.CPUModel, e.CPUs, e.Memory)
	p("| Go | harness %s, angelia %s |\n", e.HarnessGo, e.AngeliaGo)
	mod := ""
	if e.CommitModified {
		mod = " (with uncommitted changes)"
	}
	p("| Commit | %s%s |\n", e.Commit, mod)
	_, err := io.WriteString(w, b.String())
	return err
}
