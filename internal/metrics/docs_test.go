package metrics

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/dispatch"
	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
)

// full returns a Metrics on which every angelia metric has a sample.
func full(t *testing.T) *Metrics {
	t.Helper()
	m := New(nil)
	_, _ = m.Source(fakeSource{kind: "pypi"}).List(t.Context(), source.Target{}, source.FetchState{})
	m.RegisterDeliveryCounts(fakeCounts{})
	m.DueBacklog(1)
	m.CheckLag(time.Second)
	m.AttemptFinished(dispatch.OutcomeDelivered)
	m.RateLimitWait("pypi.org", time.Second)
	m.ServerPause("pypi.org", netguard.PauseRetryAfter)
	m.Response("pypi.org", netguard.ClassOK)
	m.DeadlineRefusal("pypi.org")
	table, err := netguard.EffectiveLimits(nil, netguard.HostLimit{})
	if err != nil {
		t.Fatal(err)
	}
	m.SetHostLimits(table)
	m.SetHostDemand(map[string]float64{"pypi.org": 1})
	return m
}

func angeliaFamilies(t *testing.T, m *Metrics) []string {
	t.Helper()
	var names []string
	for _, f := range gather(t, m) {
		if strings.HasPrefix(f.GetName(), "angelia_") {
			names = append(names, f.GetName())
		}
	}
	if len(names) < 8 {
		t.Fatalf("only %d angelia metrics: %v", len(names), names)
	}
	return names
}

// AC: metric names are documented. Every metric appears in the metrics page.
func TestEveryMetricIsDocumented(t *testing.T) {
	doc, err := os.ReadFile("../../docs/operations/metrics.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range angeliaFamilies(t, full(t)) {
		if !strings.Contains(string(doc), "`"+n+"`") {
			t.Errorf("docs/operations/metrics.md does not document %s", n)
		}
	}
}

// AC: the Grafana dashboard is valid JSON, and every metric it queries exists.
func TestGrafanaDashboard(t *testing.T) {
	b, err := os.ReadFile("../../deploy/grafana/angelia.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Title  string `json:"title"`
		Panels []struct {
			Title   string `json:"title"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("dashboard is not valid JSON: %v", err)
	}
	if d.Title == "" || len(d.Panels) == 0 {
		t.Fatalf("dashboard has title %q and %d panels", d.Title, len(d.Panels))
	}
	known := map[string]bool{}
	for _, n := range angeliaFamilies(t, full(t)) {
		known[n] = true
	}
	used := map[string]bool{}
	ref := regexp.MustCompile(`angelia_[a-z_]+`)
	for _, p := range d.Panels {
		if len(p.Targets) == 0 {
			t.Errorf("panel %q has no query", p.Title)
		}
		for _, tg := range p.Targets {
			for _, name := range ref.FindAllString(tg.Expr, -1) {
				base := name
				for _, suf := range []string{"_bucket", "_sum", "_count"} {
					if trimmed := strings.TrimSuffix(name, suf); known[trimmed] {
						base = trimmed
					}
				}
				if !known[base] {
					t.Errorf("panel %q queries unknown metric %s", p.Title, name)
				}
				used[base] = true
			}
		}
	}
	// The issue's list: lag, fetches, ETag, rate-limit waits, deliveries, DLQ.
	for _, n := range []string{
		"angelia_check_lag_seconds", "angelia_source_fetches_total", "angelia_netguard_rate_limit_wait_seconds",
		"angelia_delivery_attempts_total", "angelia_deliveries", "angelia_dlq_size",
	} {
		if !used[n] {
			t.Errorf("dashboard does not show %s", n)
		}
	}
}
