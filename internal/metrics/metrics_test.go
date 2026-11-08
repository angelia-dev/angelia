package metrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
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
	m := New(nil)
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
	m := New(nil)
	m.Source(fakeSource{kind: "pypi"})
	// netguard folds hosts outside its limits table into "other" before the
	// observer sees them (TestObserverHostsFromLimitsTableOnly); what reaches
	// here is a small configured set.
	for _, h := range []string{"api.github.com", "pypi.org", "other", "API.GitHub.com"} {
		m.RateLimitWait(h, time.Millisecond)
	}
	for _, h := range []string{"api.github.com", "pypi.org", "other"} {
		m.ServerPause(h, netguard.PauseRetryAfter)
		m.Response(h, netguard.ClassOK)
		m.DeadlineRefusal(h)
	}
	allowed := map[string]bool{
		"source": true, "result": true, "host": true, "outcome": true, "status": true, "version": true, "le": true,
		"bucket": true, "origin": true, "reason": true, "class": true,
	}
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
	if series > 200 {
		t.Errorf("%d angelia series with four hosts, want a bounded count", series)
	}
}

func TestSchedulerMetrics(t *testing.T) {
	m := New(nil)
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
	// The buckets resolve a P95 anywhere from 0 to 5 minutes, around the 2 minute target.
	// Observations are 2 s and 90 s.
	for le, n := range map[string]int{
		"0.5": 0, "1": 0, "2": 1, "5": 1, "10": 1, "15": 1, "30": 1, "60": 1, "90": 2, "120": 2,
		"180": 2, "240": 2, "300": 2, "600": 2, "900": 2, "3600": 2, "+Inf": 2,
	} {
		mustHave(t, body, fmt.Sprintf(`angelia_check_lag_seconds_bucket{le=%q} %d`, le, n))
	}
	m.CheckLag(-time.Second) // a check started early counts as no lag
	mustHave(t, scrape(t, m), "angelia_check_lag_seconds_sum 92")
}

func TestDispatchMetrics(t *testing.T) {
	m := New(nil)
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
	m := New(nil)
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
	m := New(nil)
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

// A source that is wrapped but has not fetched yet has no series: series
// appear on use, so a registered kind costs nothing until it is checked.
func TestSourceKindsBecomeSeriesOnUse(t *testing.T) {
	m := New(nil)
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
	m := New(nil)
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
		log: slog.New(slog.DiscardHandler),
	}
	m := New(nil)
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
	// The failure is cached for the TTL: a busy database is not hit again.
	now = now.Add(time.Second)
	scrape(t, m)
	if calls != 2 {
		t.Errorf("store queried %d times, want 2 (one success, one cached failure)", calls)
	}
}

type countFunc func(context.Context) (map[string]int64, error)

func (f countFunc) CountDeliveriesByStatus(ctx context.Context) (map[string]int64, error) {
	return f(ctx)
}

func TestHandlerServesTextAndProcessMetrics(t *testing.T) {
	m := New(nil)
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
	a, b := New(nil), New(nil)
	a.DueBacklog(9)
	if strings.Contains(scrape(t, b), "angelia_scheduler_due_targets 9") {
		t.Error("instances share state")
	}
}

// A failed scrape is logged, not silent.
func TestScrapeErrorsAreLogged(t *testing.T) {
	var buf strings.Builder
	m := New(slog.New(slog.NewTextHandler(&buf, nil)))
	m.RegisterDeliveryCounts(fakeCounts{err: errors.New("db down")})
	scrape(t, m)
	if !strings.Contains(buf.String(), "db down") {
		t.Errorf("failure not logged: %q", buf.String())
	}
}

// Observers are called from many goroutines while Prometheus scrapes.
func TestConcurrentUseUnderRace(t *testing.T) {
	m := New(nil)
	src := m.Source(fakeSource{kind: "pypi"})
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for range 200 {
				m.RateLimitWait([]string{"pypi.org", "other", "api.github.com"}[i%3], time.Millisecond)
				m.AttemptFinished(dispatch.OutcomeRetry)
				m.DueBacklog(int64(i))
				m.CheckLag(time.Second)
				_, _ = src.List(t.Context(), source.Target{}, source.FetchState{})
			}
		})
	}
	for range 20 {
		scrape(t, m)
	}
	wg.Wait()
	mustHave(t, scrape(t, m), `angelia_delivery_attempts_total{outcome="retry"} 1600`)
}

// ADR 0027 §8: the gauges carry the effective table, one series per host
// and bucket, with the origin of each limit; "other" is the fallback.
func TestHostLimitGauges(t *testing.T) {
	m := New(nil)
	table, err := netguard.EffectiveLimits(map[string]netguard.HostLimit{
		"127.0.0.1":      {Authenticated: netguard.Limit{Rate: 50, Burst: 50}, Anonymous: netguard.Limit{Rate: 50, Burst: 50}},
		"api.github.com": {Authenticated: netguard.Limit{Rate: 100, Burst: 100}, Anonymous: netguard.Limit{Rate: 100, Burst: 100}},
	}, netguard.HostLimit{Authenticated: netguard.Limit{Rate: 2, Burst: 2}, Anonymous: netguard.Limit{Rate: 2, Burst: 2}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetHostLimits(table)
	body := scrape(t, m)
	mustHave(t, body,
		`angelia_netguard_host_limit_rate{bucket="anonymous",host="127.0.0.1",origin="config"} 50`,
		`angelia_netguard_host_limit_burst{bucket="authenticated",host="127.0.0.1",origin="config"} 50`,
		`angelia_netguard_host_limit_burst{bucket="authenticated",host="api.github.com",origin="builtin"} 10`,
		`angelia_netguard_host_limit_burst{bucket="anonymous",host="api.github.com",origin="builtin"} 1`,
		`angelia_netguard_host_limit_rate{bucket="anonymous",host="crates.io",origin="builtin"} 1`,
		`angelia_netguard_host_limit_rate{bucket="anonymous",host="other",origin="default"} 2`,
	)
	// Setting the table again replaces it.
	defaults, _ := netguard.EffectiveLimits(nil, netguard.HostLimit{})
	m.SetHostLimits(defaults)
	body = scrape(t, m)
	if strings.Contains(body, `host="127.0.0.1"`) {
		t.Error("a host of the previous table is still exposed")
	}
	mustHave(t, body, `angelia_netguard_host_limit_rate{bucket="anonymous",host="other",origin="builtin"} 5`)
}

// ADR 0027 §8: the counters, with closed reason and class labels.
func TestNetguardCounters(t *testing.T) {
	m := New(nil)
	m.ServerPause("pypi.org", netguard.PauseRetryAfter)
	m.ServerPause("pypi.org", netguard.PauseRateLimitReset)
	m.ServerPause("pypi.org", "surprise")
	for _, c := range []string{"2xx", "304", "3xx", "4xx", "429", "5xx", "error", "1xx"} {
		m.Response("API.GitHub.com", c)
	}
	m.DeadlineRefusal("other")
	m.DeadlineRefusal("other")
	mustHave(t, scrape(t, m),
		`angelia_netguard_server_pauses_total{host="pypi.org",reason="retry_after"} 1`,
		`angelia_netguard_server_pauses_total{host="pypi.org",reason="ratelimit_reset"} 1`,
		`angelia_netguard_server_pauses_total{host="pypi.org",reason="other"} 1`,
		`angelia_netguard_responses_total{class="304",host="api.github.com"} 1`,
		`angelia_netguard_responses_total{class="429",host="api.github.com"} 1`,
		`angelia_netguard_responses_total{class="error",host="api.github.com"} 1`,
		`angelia_netguard_responses_total{class="other",host="api.github.com"} 1`,
		`angelia_netguard_deadline_refusals_total{host="other"} 2`,
	)
}

// The counters are fed by a real netguard client: a sink host outside the
// table is "other", never its name.
func TestNetguardClientFeedsCounters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	m := New(nil)
	c, err := netguard.New(netguard.Config{
		Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, Observer: m, MaxAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.SetHostLimits(c.Limits())
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if _, err := netguard.Fetch(t.Context(), c.HTTPClient(), req, netguard.Validators{}); err == nil {
		t.Fatal("want a status error")
	}
	body := scrape(t, m)
	mustHave(t, body,
		`angelia_netguard_responses_total{class="429",host="other"} 1`,
		`angelia_netguard_server_pauses_total{host="other",reason="retry_after"} 1`,
	)
	if strings.Contains(body, "127.0.0.1") {
		t.Error("an unlisted host reached a label")
	}
}
