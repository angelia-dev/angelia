package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/pypi"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

func busyKills(n int) []Kill {
	ks := make([]Kill, n)
	for i := range ks {
		ks[i] = Kill{Index: i + 1, Uptime: time.Second, Received: 50, Open: i % 2, Backlog: Backlog{Pending: 100}}
	}
	return ks
}

func TestVerdict(t *testing.T) {
	ok := []Check{{Name: "a", OK: true}}
	bad := []Check{{Name: "a", OK: false}}
	for _, tc := range []struct {
		name          string
		verify        []Check
		kills         []Kill
		want          string
		wantSetupFail bool
	}{
		{"pass", ok, busyKills(100), "pass", false},
		{"a lost delivery fails", bad, busyKills(100), "fail", false},
		{"too few kills is incomplete", ok, busyKills(99), "incomplete", true},
		{"no verification is incomplete", nil, busyKills(100), "incomplete", false},
		{"a fault wins over an incomplete run", bad, busyKills(10), "fail", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r Report
			r.decide(tc.verify, setupChecks(100, tc.kills, Tally{}))
			if r.Verdict != tc.want {
				t.Errorf("verdict %s, want %s", r.Verdict, tc.want)
			}
			if r.Checks[0].OK == tc.wantSetupFail {
				t.Errorf("kill count check %+v", r.Checks[0])
			}
		})
	}
}

func TestSetupChecksNeedDispatchAtEveryKill(t *testing.T) {
	ks := busyKills(100)
	ks[41].Backlog = Backlog{}
	c := setupChecks(100, ks, Tally{})
	if !c[0].OK || c[1].OK || !strings.Contains(c[1].Examples[0], "kill 42") {
		t.Errorf("checks %+v", c)
	}
}

// Acceptance criterion 3: the report holds the verdict, the checks, the
// kills and the environment.
func TestReportWrite(t *testing.T) {
	dir := t.TempDir()
	r := &Report{
		Benchmark: "kill-9", Started: time.Date(2026, 11, 9, 20, 0, 0, 0, time.UTC), Duration: 15 * time.Minute,
		Environment: environment(context.Background(), "/nonexistent", "test machine"),
		Params:      Params{Workload: workload{Packages: 300, Subscriptions: 5, ReleasesPerH: 108000, History: 3}, Kills: 100, Uptime: "uniform"},
		Kills:       busyKills(100),
		Tally:       Tally{Events: 10, Deliveries: 50, Received: 52, Distinct: 50, Duplicates: 2, DuplicatedPairs: 1},
	}
	r.decide([]Check{{Name: "every delivery received at least once (zero lost)", OK: true, Detail: "50 of 50"}}, setupChecks(100, r.Kills, r.Tally))
	if err := r.Write(dir); err != nil {
		t.Fatal(err)
	}
	md, err := os.ReadFile(filepath.Join(dir, "report.md")) //nolint:gosec // the test directory
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"**Verdict: PASS.**", "zero lost", "| Kills | 100 |", "test machine", "| 100 | 0.0s | 1.0s | 50 | 1 | 100 | 0 |", "one every 10.0s per package"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("report.md lacks %q:\n%s", want, md)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "report.json")); err != nil {
		t.Error(err)
	}
}

// The generated files are valid for angelia: the watch file parses and the
// instance configuration loads.
func TestGeneratedConfiguration(t *testing.T) {
	dir := t.TempDir()
	w := workload{Packages: 3, Subscriptions: 2, ReleasesPerH: 100, History: 3}
	wf, cf := filepath.Join(dir, "watches.yaml"), filepath.Join(dir, "angelia.yaml")
	if err := writeWatchFile(wf, "http://127.0.0.2:4000", w); err != nil {
		t.Fatal(err)
	}
	if err := writeInstanceConfig(cf, "127.0.0.1:8080", dir, "http://127.0.0.1:5000", "http://127.0.0.2:4000"); err != nil {
		t.Fatal(err)
	}
	reg := source.NewRegistry()
	if err := reg.Register(pypi.New("", nil)); err != nil {
		t.Fatal(err)
	}
	f, err := watchfile.Load(wf, watchfile.Options{Sources: reg, AllowHTTPHosts: []string{"127.0.0.2"}, LookupEnv: func(string) (string, bool) { return "x", true }})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Watches) != 3 || len(f.Sinks) != 2 || len(f.Subscriptions) != 2 {
		t.Errorf("watch file: %d watches, %d sinks, %d subscriptions", len(f.Watches), len(f.Sinks), len(f.Subscriptions))
	}
	c, err := config.Load(context.Background(), config.Options{Path: cf})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(c.HTTP.AllowHTTPHosts, ","), "127.0.0.2") {
		t.Errorf("allow_http_hosts %v", c.HTTP.AllowHTTPHosts)
	}
	rc := registryConfig(w)
	if b := rc.Ecosystems["pypi"]; b.Packages != 3 || b.ReleasesPerHour != 100 || b.History != 3 {
		t.Errorf("pypi behaviour %+v", b)
	}
	if err := rc.Validate(); err != nil {
		t.Error(err)
	}
}
