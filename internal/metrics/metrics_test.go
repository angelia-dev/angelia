package metrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil/promlint"
	dto "github.com/prometheus/client_model/go"

	"github.com/angelia-dev/angelia/internal/dispatch"
	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/scheduler"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/store"
)

// Compile-time checks: *Metrics plugs into the hooks of the other packages.
var (
	_ scheduler.Metrics = (*Metrics)(nil)
	_ dispatch.Metrics  = (*Metrics)(nil)
	_ netguard.Observer = (*Metrics)(nil)
)

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d: %s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// has reports whether the scrape contains the exact sample line.
func has(body, line string) bool {
	for _, l := range strings.Split(body, "\n") {
		if l == line {
			return true
		}
	}
	return false
}

func mustHave(t *testing.T, body string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !has(body, l) {
			t.Errorf("scrape lacks %q\n%s", l, body)
		}
	}
}

func gather(t *testing.T, m *Metrics) []*dto.MetricFamily {
	t.Helper()
	fams, err := m.reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	return fams
}

// AC: names follow Prometheus conventions (promlint) and every angelia
// metric is documented (has help text and appears in docs/operations/metrics.md).
func TestNamesFollowConventions(t *testing.T) {
	m := New()
	m.Source(fakeSource{kind: "pypi"})
	m.RegisterDeliveryCounts(fakeCounts{})
	m.DueBacklog(1)
	m.CheckLag(time.Second)
	m.AttemptFinished(dispatch.OutcomeDead)
	m.RateLimitWait("pypi.org", time.Second)

	body := scrape(t, m)
	problems, err := promlint.New(strings.NewReader(body)).Lint()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Errorf("promlint: %s: %s", p.Metric, p.Text)
	}
	for _, f := range gather(t, m) {
		if !strings.HasPrefix(f.GetName(), "angelia_") {
			continue
		}
		if f.GetHelp() == "" {
			t.Errorf("%s has no help text", f.GetName())
		}
	}
}

// AC: cardinality is bounded. Only the documented labels exist, and no
// label carries a package, watch or target name, however many are seen.
func TestCardinalityBounded(t *testing.T) {
	m := New()
	m.Source(fakeSource{kind: "pypi"})
	for i := range 500 {
		m.RateLimitWait(fmt.Sprintf("host%d.example", i), time.Millisecond)
	}
	allowed := map[string]bool{"source": true, "result": true, "host": true, "outcome": true, "status": true, "version": true, "le": true}
	series := 0
	for _, f := range gather(t, m) {
		if !strings.HasPrefix(f.GetName(), "angelia_") {
			continue
		}
		for _, mm := range f.GetMetric() {
			series++
			for _, l := range mm.GetLabel() {
				if !allowed[l.GetName()] {
					t.Errorf("%s has undocumented label %q", f.GetName(), l.GetName())
				}
			}
		}
	}
	hosts := map[string]bool{}
	for _, f := range gather(t, m) {
		if f.GetName() != "angelia_netguard_rate_limit_wait_seconds" {
			continue
		}
		for _, mm := range f.GetMetric() {
			for _, l := range mm.GetLabel() {
				if l.GetName() == "host" {
					hosts[l.GetValue()] = true
				}
			}
		}
	}
	if len(hosts) != maxHosts+1 {
		t.Errorf("distinct host label values = %d, want %d (the cap plus \"other\")", len(hosts), maxHosts+1)
	}
	if !hosts[otherHost] {
		t.Errorf("hosts past the cap are not folded into %q", otherHost)
	}
	if series > 2000 {
		t.Errorf("%d angelia series after 500 hosts, want a bounded count", series)
	}
}

func TestSchedulerMetrics(t *testing.T) {
	m := New()
	m.DueBacklog(7)
	m.CheckLag(90 * time.Second)
	m.CheckLag(2 * time.Second)
	body := scrape(t, m)
	mustHave(t, body,
		"angelia_scheduler_due_targets 7",
		"angelia_check_lag_seconds_count 2",
		"angelia_check_lag_seconds_sum 92",
		`angelia_check_lag_seconds_bucket{le="120"} 2`,
		`angelia_check_lag_seconds_bucket{le="60"} 1`,
	)
	m.CheckLag(-time.Second) // a check started early counts as no lag
	mustHave(t, scrape(t, m), "angelia_check_lag_seconds_sum 92")
}

func TestDispatchMetrics(t *testing.T) {
	m := New()
	for _, o := range []string{dispatch.OutcomeDelivered, dispatch.OutcomeDelivered, dispatch.OutcomeRetry, dispatch.OutcomeDead, "bogus"} {
		m.AttemptFinished(o)
	}
	mustHave(t, scrape(t, m),
		`angelia_delivery_attempts_total{outcome="delivered"} 2`,
		`angelia_delivery_attempts_total{outcome="retry"} 1`,
		`angelia_delivery_attempts_total{outcome="dead"} 1`,
		`angelia_delivery_attempts_total{outcome="other"} 1`,
	)
}

func TestRateLimitWait(t *testing.T) {
	m := New()
	m.RateLimitWait("API.GitHub.com", 0)
	m.RateLimitWait("api.github.com", 3*time.Second)
	mustHave(t, scrape(t, m),
		`angelia_netguard_rate_limit_wait_seconds_count{host="api.github.com"} 2`,
		`angelia_netguard_rate_limit_wait_seconds_sum{host="api.github.com"} 3`,
	)
}

type fakeSource struct {
	kind string
	res  source.Result
	err  error
}

func (f fakeSource) Kind() string                 { return f.kind }
func (f fakeSource) Validate(source.Target) error { return errors.New("validate called") }
func (f fakeSource) List(context.Context, source.Target, source.FetchState) (source.Result, error) {
	return f.res, f.err
}

// AC: fetch results by source and status; the ETag hit rate is
// not_modified / (ok + not_modified). The wrapper passes results and
// errors through unchanged.
func TestSourceFetchResults(t *testing.T) {
	m := New()
	cases := []struct {
		name string
		src  fakeSource
		want string
	}{
		{"ok", fakeSource{kind: "pypi", res: source.Result{Versions: []source.Version{{Raw: "1"}}}}, "ok"},
		{"not modified", fakeSource{kind: "pypi", res: source.Result{NotModified: true}}, "not_modified"},
		{"429", fakeSource{kind: "npm", err: fmt.Errorf("npm: %w", &netguard.StatusError{StatusCode: 429})}, "rate_limited"},
		{"403 with reset", fakeSource{kind: "npm", err: &netguard.StatusError{StatusCode: 403, RetryAt: time.Now()}}, "rate_limited"},
		{"500", fakeSource{kind: "npm", err: &netguard.StatusError{StatusCode: 500}}, "error"},
		{"canceled", fakeSource{kind: "npm", err: fmt.Errorf("fetch: %w", context.Canceled)}, "canceled"},
		{"other", fakeSource{kind: "npm", err: errors.New("boom")}, "error"},
	}
	for _, tc := range cases {
		src := m.Source(tc.src)
		if src.Kind() != tc.src.kind {
			t.Fatalf("%s: Kind = %q", tc.name, src.Kind())
		}
		res, err := src.List(t.Context(), source.Target{}, source.FetchState{})
		if !errors.Is(err, tc.src.err) {
			t.Errorf("%s: error changed: %v", tc.name, err)
		}
		if res.NotModified != tc.src.res.NotModified || len(res.Versions) != len(tc.src.res.Versions) {
			t.Errorf("%s: result changed: %+v", tc.name, res)
		}
	}
	if err := m.Source(fakeSource{kind: "x"}).Validate(source.Target{}); err == nil || err.Error() != "validate called" {
		t.Errorf("Validate not forwarded: %v", err)
	}
	mustHave(t, scrape(t, m),
		`angelia_source_fetches_total{result="ok",source="pypi"} 1`,
		`angelia_source_fetches_total{result="not_modified",source="pypi"} 1`,
		`angelia_source_fetches_total{result="rate_limited",source="npm"} 2`,
		`angelia_source_fetches_total{result="error",source="npm"} 2`,
		`angelia_source_fetches_total{result="canceled",source="npm"} 1`,
	)
}

// A source that is wrapped twice, or whose kind is registered after the
// first scrape, still yields one series per (source, result).
func TestSourceKindsBecomeSeriesOnUse(t *testing.T) {
	m := New()
	m.Source(fakeSource{kind: "pypi"})
	body := scrape(t, m)
	if strings.Contains(body, `source="pypi"`) {
		t.Error("a registered but unused source already has series")
	}
}

type fakeCounts struct {
	m   map[string]int64
	err error
}

func (f fakeCounts) CountDeliveriesByStatus(context.Context) (map[string]int64, error) {
	return f.m, f.err
}

// AC: deliveries by status and the DLQ size come from the store at scrape
// time, with all four statuses present even at zero.
func TestDeliveryCounts(t *testing.T) {
	m := New()
	m.RegisterDeliveryCounts(fakeCounts{m: map[string]int64{store.DeliveryPending: 3, store.DeliveryDead: 5}})
	mustHave(t, scrape(t, m),
		`angelia_deliveries{status="pending"} 3`,
		`angelia_deliveries{status="in_flight"} 0`,
		`angelia_deliveries{status="delivered"} 0`,
		`angelia_deliveries{status="dead"} 5`,
		"angelia_dlq_size 5",
	)
}

func TestDeliveryCountsCachedAndFailureIsolated(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	c := &deliveryCollector{
		src: countFunc(func(context.Context) (map[string]int64, error) {
			calls++
			if calls == 2 {
				return nil, errors.New("db down")
			}
			return map[string]int64{store.DeliveryDead: int64(calls)}, nil
		}),
		now: func() time.Time { return now },
	}
	m := New()
	m.reg.MustRegister(c)
	mustHave(t, scrape(t, m), "angelia_dlq_size 1")
	now = now.Add(time.Second)
	mustHave(t, scrape(t, m), "angelia_dlq_size 1") // cached
	if calls != 1 {
		t.Fatalf("store queried %d times within the cache window, want 1", calls)
	}
	now = now.Add(time.Minute)
	m.DueBacklog(4)
	body := scrape(t, m) // the store fails; everything else is still served
	mustHave(t, body, "angelia_scheduler_due_targets 4")
	if strings.Contains(body, "angelia_dlq_size") {
		t.Error("a failed store query still reported a DLQ size")
	}
}

type countFunc func(context.Context) (map[string]int64, error)

func (f countFunc) CountDeliveriesByStatus(ctx context.Context) (map[string]int64, error) {
	return f(ctx)
}

func TestHandlerServesTextAndProcessMetrics(t *testing.T) {
	m := New()
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q", ct)
	}
	for _, want := range []string{"go_goroutines", "angelia_build_info{"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("scrape lacks %s", want)
		}
	}
}

// Two instances do not share state: nothing registers globally.
func TestNoGlobalState(t *testing.T) {
	a, b := New(), New()
	a.DueBacklog(9)
	if strings.Contains(scrape(t, b), "angelia_scheduler_due_targets 9") {
		t.Error("instances share state")
	}
}
