package scheduler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

type fakeDemandStore struct {
	groups []store.TargetDemand
	err    error
}

func (f *fakeDemandStore) TargetDemand(context.Context) ([]store.TargetDemand, error) {
	return f.groups, f.err
}

// demandFixture wires a Demand with two sources: "pypi" on pypi.org (own
// entry, 10 rps) and "mirror" on an unlisted host (fallback, 5 rps, label
// other). "gh" is a credentialed source on a fixed built-in budget.
type demandFixture struct {
	st     *fakeDemandStore
	d      *Demand
	logs   *bytes.Buffer
	report map[string]float64
}

func newDemandFixture(t *testing.T) *demandFixture {
	t.Helper()
	f := &demandFixture{st: &fakeDemandStore{}, logs: &bytes.Buffer{}}
	hosts := map[string]struct {
		host   string
		authed bool
	}{
		"pypi": {"pypi.org", false}, "mirror": {"mirror.example", false}, "mirror2": {"mirror2.example", false},
		"gh": {"api.github.com", true},
	}
	d, err := NewDemand(DemandOptions{
		Store: f.st,
		SourceHost: func(name string) (string, bool, bool) {
			h, ok := hosts[name]
			return h.host, h.authed, ok
		},
		Limit: func(host string, _ bool) HostLimitInfo {
			switch host {
			case "pypi.org":
				return HostLimitInfo{Label: "pypi.org", RPS: 10, Burst: 10}
			case "api.github.com":
				return HostLimitInfo{Label: "api.github.com", RPS: 1.39, Burst: 10, Fixed: true}
			}
			return HostLimitInfo{Label: "other", RPS: 5, Burst: 5}
		},
		Report: func(m map[string]float64) { f.report = m },
		Logger: slog.New(slog.NewTextHandler(f.logs, nil)),
	})
	must(t, err)
	f.d = d
	return f
}

// AC: the estimated demand per host is computed from stored targets and
// their intervals; targets without an interval use the default; hosts
// outside the limits table are summed under "other".
func TestDemandEstimate(t *testing.T) {
	f := newDemandFixture(t)
	f.st.groups = []store.TargetDemand{
		{Name: "pypi", Interval: time.Minute, Targets: 120},      // 2 rps
		{Name: "pypi", Interval: 10 * time.Minute, Targets: 60},  // 0.1 rps
		{Name: "pypi", Targets: 3600},                            // default 1h: 1 rps
		{Name: "mirror", Interval: time.Hour, Targets: 3600},     // 1 rps, other
		{Name: "mirror2", Interval: time.Hour, Targets: 7200},    // 2 rps, other
		{Name: "plugin-kind", Interval: time.Minute, Targets: 9}, // unknown host: left out
	}
	must(t, f.d.Refresh(t.Context(), time.Hour))
	want := map[string]float64{"pypi.org": 3.1, "other": 3}
	if len(f.report) != len(want) {
		t.Fatalf("report = %v, want %v", f.report, want)
	}
	for h, w := range want {
		if got := f.report[h]; got < w-1e-9 || got > w+1e-9 {
			t.Errorf("demand[%s] = %v, want %v", h, got, w)
		}
	}
	if f.logs.Len() != 0 {
		t.Errorf("no host is above 80%%, yet logged: %s", f.logs)
	}
}

// AC: a warning is logged above 80% of the host's effective limit, with the
// sizing rule; not at or below it.
func TestDemandWarnsAboveEightyPercent(t *testing.T) {
	f := newDemandFixture(t)
	f.st.groups = []store.TargetDemand{{Name: "pypi", Interval: time.Second, Targets: 8}} // exactly 8 = 80% of 10
	must(t, f.d.Refresh(t.Context(), time.Hour))
	if f.logs.Len() != 0 {
		t.Fatalf("exactly 80%% must not warn: %s", f.logs)
	}
	f.st.groups = []store.TargetDemand{{Name: "pypi", Interval: time.Second, Targets: 9}}
	must(t, f.d.Refresh(t.Context(), time.Hour))
	out := f.logs.String()
	for _, want := range []string{
		"level=WARN", "host=pypi.org", "demand_rps=9", "limit_rps=10", "targets=9", "shortest_interval=1s",
		"rps >= targets on the host / shortest interval * requests per check", "http.host_limits.hosts.pypi.org.rps",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("warning lacks %q:\n%s", want, out)
		}
	}
}

// A fixed built-in budget is not advised to be raised; the unlisted host
// is judged against the fallback per host, not summed with other hosts.
func TestDemandWarnsFixedBudgetAndPerHost(t *testing.T) {
	f := newDemandFixture(t)
	f.st.groups = []store.TargetDemand{
		{Name: "gh", Interval: time.Second, Targets: 2}, // 2 rps > 0.8 * 1.39
		{Name: "mirror", Interval: time.Second, Targets: 3},
		{Name: "mirror2", Interval: time.Second, Targets: 3}, // each 3 < 4: no warning, though 6 > 4 summed
	}
	must(t, f.d.Refresh(t.Context(), time.Hour))
	out := f.logs.String()
	if !strings.Contains(out, "host=api.github.com") || !strings.Contains(out, "cannot raise") || strings.Contains(out, "http.host_limits.hosts.api.github.com.rps") {
		t.Errorf("fixed budget warning wrong:\n%s", out)
	}
	if strings.Contains(out, "mirror") {
		t.Errorf("per-host demand under the limit warned:\n%s", out)
	}
	if !strings.Contains(out, "bucket=authenticated") {
		t.Errorf("credentialed source must use the authenticated bucket:\n%s", out)
	}
	if f.report["other"] != 6 {
		t.Errorf("other = %v, want 6 (sum over unlisted hosts)", f.report["other"])
	}
}

// The warning is logged at start-up and when the numbers change, not at
// every refresh, and again after the demand dropped and rose.
func TestDemandWarningIsNotRepeated(t *testing.T) {
	f := newDemandFixture(t)
	over := []store.TargetDemand{{Name: "pypi", Interval: time.Second, Targets: 9}}
	f.st.groups = over
	count := func() int { return strings.Count(f.logs.String(), "level=WARN") }
	must(t, f.d.Refresh(t.Context(), time.Hour))
	must(t, f.d.Refresh(t.Context(), time.Hour))
	if count() != 1 {
		t.Fatalf("unchanged demand: %d warnings, want 1", count())
	}
	f.st.groups = []store.TargetDemand{{Name: "pypi", Interval: time.Second, Targets: 10}} // a watch was added
	must(t, f.d.Refresh(t.Context(), time.Hour))
	if count() != 2 {
		t.Fatalf("changed demand: %d warnings, want 2", count())
	}
	f.st.groups = nil
	must(t, f.d.Refresh(t.Context(), time.Hour))
	f.st.groups = over
	must(t, f.d.Refresh(t.Context(), time.Hour))
	if count() != 3 {
		t.Fatalf("crossed again: %d warnings, want 3", count())
	}
	if len(f.report) != 1 {
		t.Fatalf("report = %v", f.report)
	}
}

func TestDemandRefreshStoreError(t *testing.T) {
	f := newDemandFixture(t)
	f.st.err = errors.New("boom")
	if err := f.d.Refresh(t.Context(), time.Hour); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if f.report != nil {
		t.Fatal("reported despite the error")
	}
}

func TestNewDemandRequiresParts(t *testing.T) {
	if _, err := NewDemand(DemandOptions{}); err == nil {
		t.Fatal("want an error")
	}
}

type countingDemand struct {
	n        int
	interval time.Duration
	err      error
}

func (c *countingDemand) Refresh(_ context.Context, iv time.Duration) error {
	c.n++
	c.interval = iv
	return c.err
}

// AC: the scheduler estimates at start-up (first poll) and again every
// DemandEvery, with its own default interval.
func TestSchedulerRefreshesDemand(t *testing.T) {
	st := openStore(t)
	clk := newFakeClock(t0)
	cd := &countingDemand{err: errors.New("store down")}
	s := newTestScheduler(t, st, newCounter(), clk, func(o *Options) {
		o.Demand, o.DemandEvery, o.Interval = cd, 5*time.Minute, 30*time.Minute
	})
	ctx := t.Context()
	s.poll(ctx)
	if cd.n != 1 || cd.interval != 30*time.Minute {
		t.Fatalf("first poll: %d refreshes, interval %v", cd.n, cd.interval)
	}
	clk.Advance(4 * time.Minute)
	s.poll(ctx)
	if cd.n != 1 {
		t.Fatalf("before DemandEvery: %d refreshes", cd.n)
	}
	clk.Advance(time.Minute)
	s.poll(ctx)
	if cd.n != 2 {
		t.Fatalf("after DemandEvery: %d refreshes", cd.n)
	}
}

func TestSchedulerRejectsNegativeDemandEvery(t *testing.T) {
	if _, err := New(openStore(t), newCounter(), Options{DemandEvery: -1}); err == nil {
		t.Fatal("want an error")
	}
}
