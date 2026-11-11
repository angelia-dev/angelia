// Package metrics exposes Angelia's Prometheus metrics (ADR 0019).
//
// Metrics holds a private registry (no global state) and implements the
// observation hooks of the scheduler (scheduler.Metrics), the dispatcher
// (dispatch.Metrics) and the netguard client (netguard.Observer). Source
// fetches are counted by wrapping each source (Metrics.Source); the delivery
// table is counted by a collector that queries the store at scrape time
// (Metrics.RegisterDeliveryCounts); the scheduler's per-host demand estimate is set with Metrics.SetHostDemand. Handler serves the registry.
//
// Cardinality is bounded by design. The only labels are source kind,
// fetch result, delivery outcome and status, fixed lists (bucket, origin,
// reason, class), and the host of the netguard limits table: the built-in
// hosts plus those of http.host_limits.hosts (any other host is "other",
// because sinks share the client and their hosts can be secret). No label ever
// carries a package, watch, target, subscription or sink name.
// docs/operations/metrics.md documents every metric.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/angelia-dev/angelia/internal/buildinfo"
	"github.com/angelia-dev/angelia/internal/dispatch"
	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
)

// Fetch results, the values of the result label of angelia_source_fetches_total.
const (
	ResultOK          = "ok"           // new data (HTTP 2xx)
	ResultNotModified = "not_modified" // conditional request answered 304
	ResultRateLimited = "rate_limited" // HTTP 429, or a rate limit with a reset time
	ResultCanceled    = "canceled"     // shutdown or lost lease, not a source failure
	ResultError       = "error"        // anything else
)

// Metrics is the set of Angelia metrics. Use New; it is safe for concurrent
// use.
type Metrics struct {
	reg *prometheus.Registry

	dueTargets prometheus.Gauge
	streams    prometheus.Gauge
	checkLag   prometheus.Histogram
	fetches    *prometheus.CounterVec
	rateWait   *prometheus.HistogramVec
	attempts   *prometheus.CounterVec
	log        *slog.Logger

	mu         sync.Mutex
	limitHosts map[string]bool // hosts of the limits table, the closed set of the host label
	demandSet  map[string]bool // host labels currently set on hostDemand

	hostDemand       *prometheus.GaugeVec
	hostLimitRate    *prometheus.GaugeVec
	hostLimitBurst   *prometheus.GaugeVec
	serverPauses     *prometheus.CounterVec
	responses        *prometheus.CounterVec
	deadlineRefusals *prometheus.CounterVec
}

// Values of the bucket label of the host limit gauges.
const (
	BucketAuthenticated = "authenticated"
	BucketAnonymous     = "anonymous"
)

// New returns Metrics with its own registry, which also carries the Go
// runtime and process collectors. Scrape errors are logged to log (nil
// means slog.Default()).
//
// Pass the result to scheduler.Options, dispatch.Options and netguard.Config
// only as a non-nil *Metrics: a typed nil pointer stored in their interface
// fields is not a nil interface and would panic on the first call.
func New(log *slog.Logger) *Metrics {
	if log == nil {
		log = slog.Default()
	}
	m := &Metrics{reg: prometheus.NewRegistry(), log: log}
	m.dueTargets = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "angelia_scheduler_due_targets",
		Help: "Targets due for a check and not leased, as last counted by the scheduler (at most once per poll interval).",
	})
	m.streams = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "angelia_api_event_streams",
		Help: "Open event streams (GET /v1/events/stream) of this process.",
	})
	m.checkLag = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "angelia_check_lag_seconds",
		Help: "How late a check started: lease time minus the time the target was due.",
		// The Phase 0 target is P95 < 2 minutes (benchmark 1 reads this
		// histogram): 120 s is a bucket, with 30 to 60 s steps around it so
		// that a P95 between 0 and 5 minutes is resolved.
		Buckets: []float64{0.5, 1, 2, 5, 10, 15, 30, 60, 90, 120, 180, 240, 300, 600, 900, 3600},
	})
	m.fetches = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "angelia_source_fetches_total",
		Help: "Source fetches by source kind and result: ok, not_modified, rate_limited, canceled or error.",
	}, []string{"source", "result"})
	m.rateWait = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "angelia_netguard_rate_limit_wait_seconds",
		Help:    "Time an outgoing request waited for the per-host rate limit, by upstream host.",
		Buckets: []float64{0.001, 0.01, 0.1, 0.5, 1, 5, 30, 120},
	}, []string{"host"})
	m.attempts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "angelia_delivery_attempts_total",
		Help: "Delivery attempts recorded, by outcome: delivered, retry or dead.",
	}, []string{"outcome"})
	m.hostLimitRate = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "angelia_netguard_host_limit_rate",
		Help: "Effective per-host rate limit in requests per second, by host, bucket (authenticated or anonymous) and origin (builtin, config or default). Set at start; per process.",
	}, []string{"host", "bucket", "origin"})
	m.hostLimitBurst = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "angelia_netguard_host_limit_burst",
		Help: "Effective per-host burst (token bucket size), by host, bucket and origin. Set at start; per process.",
	}, []string{"host", "bucket", "origin"})
	m.hostDemand = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "angelia_host_demand_rps",
		Help: "Estimated request demand per host in requests per second: stored targets of the host divided by their check intervals. Hosts outside the rate-limit table are summed as other. Refreshed by the scheduler.",
	}, []string{"host"})
	m.serverPauses = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "angelia_netguard_server_pauses_total",
		Help: "Pauses of a host's rate limit set by the server, by host and reason: retry_after or ratelimit_reset.",
	}, []string{"host", "reason"})
	m.responses = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "angelia_netguard_responses_total",
		Help: "Outgoing HTTP attempts by host and outcome class: 2xx, 304, 3xx, 403, 4xx (other than 403 and 429), 429, 5xx or error.",
	}, []string{"host", "class"})
	m.deadlineRefusals = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "angelia_netguard_deadline_refusals_total",
		Help: "Outgoing requests refused at once because the host's next rate limit slot came after the caller's deadline, by host.",
	}, []string{"host"})
	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "angelia_build_info",
		Help: "Always 1; the version label carries the Angelia version.",
	}, []string{"version"})
	info.WithLabelValues(buildinfo.Version()).Set(1)

	m.reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		info, m.dueTargets, m.streams, m.checkLag, m.fetches, m.rateWait, m.attempts,
		m.hostLimitRate, m.hostLimitBurst, m.hostDemand, m.serverPauses, m.responses, m.deadlineRefusals,
	)
	return m
}

// Handler returns the /metrics handler. A collector that fails is left out
// of the scrape; the rest is still served.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{
		ErrorLog:            errorLog{m.log},
		ErrorHandling:       promhttp.ContinueOnError,
		MaxRequestsInFlight: 4,
	})
}

// errorLog adapts slog to promhttp.Logger.
type errorLog struct{ log *slog.Logger }

func (e errorLog) Println(v ...any) { e.log.Warn("metrics scrape", "error", fmt.Sprint(v...)) }

// DueBacklog implements scheduler.Metrics.
func (m *Metrics) DueBacklog(n int64) { m.dueTargets.Set(float64(n)) }

// EventStreamOpened and EventStreamClosed implement api.StreamMetrics.
func (m *Metrics) EventStreamOpened() { m.streams.Inc() }

// EventStreamClosed implements api.StreamMetrics.
func (m *Metrics) EventStreamClosed() { m.streams.Dec() }

// CheckLag implements scheduler.Metrics. A negative lag counts as zero.
func (m *Metrics) CheckLag(d time.Duration) { m.checkLag.Observe(max(0, d).Seconds()) }

// AttemptFinished implements dispatch.Metrics. An unknown outcome is
// counted as "other" so that the label set stays closed.
func (m *Metrics) AttemptFinished(outcome string) {
	switch outcome {
	case dispatch.OutcomeDelivered, dispatch.OutcomeRetry, dispatch.OutcomeDead:
	default:
		outcome = "other"
	}
	m.attempts.WithLabelValues(outcome).Inc()
}

// RateLimitWait implements netguard.Observer. netguard already folds hosts
// outside its limits table into "other", so the label set is bounded by
// configuration; host is lower-cased here only as a safeguard.
func (m *Metrics) RateLimitWait(host string, d time.Duration) {
	m.rateWait.WithLabelValues(strings.ToLower(host)).Observe(max(0, d).Seconds())
}

// ServerPause implements netguard.Observer. An unknown reason is counted
// as "other" so that the label set stays closed.
func (m *Metrics) ServerPause(host, reason string) {
	switch reason {
	case netguard.PauseRetryAfter, netguard.PauseRateLimitReset:
	default:
		reason = "other"
	}
	m.serverPauses.WithLabelValues(strings.ToLower(host), reason).Inc()
}

// Response implements netguard.Observer. An unknown class is counted as
// "other" so that the label set stays closed.
func (m *Metrics) Response(host, class string) {
	switch class {
	case netguard.ClassOK, netguard.ClassNotModified, netguard.ClassRedirect, netguard.ClassForbidden, netguard.ClassClientError,
		netguard.ClassTooManyRequests, netguard.ClassServerError, netguard.ClassError:
	default:
		class = "other"
	}
	m.responses.WithLabelValues(strings.ToLower(host), class).Inc()
}

// DeadlineRefusal implements netguard.Observer.
func (m *Metrics) DeadlineRefusal(host string) {
	m.deadlineRefusals.WithLabelValues(strings.ToLower(host)).Inc()
}

// SetHostLimits sets the host limit gauges from the limits table of the
// netguard client (netguard.Client.Limits), replacing earlier values. Its
// hosts are the table's, the same closed set as the host label of the
// netguard metrics, with "other" for the fallback (ADR 0027 §8).
func (m *Metrics) SetHostLimits(table []netguard.EffectiveLimit) {
	m.hostLimitRate.Reset()
	m.hostLimitBurst.Reset()
	hosts := map[string]bool{}
	defer func() {
		m.mu.Lock()
		m.limitHosts = hosts
		m.mu.Unlock()
	}()
	for _, e := range table {
		host := strings.ToLower(e.Host)
		hosts[host] = true
		for _, b := range []struct {
			bucket, origin string
			limit          netguard.Limit
		}{
			{BucketAuthenticated, e.AuthenticatedOrigin, e.Authenticated},
			{BucketAnonymous, e.AnonymousOrigin, e.Anonymous},
		} {
			origin := b.origin
			switch origin {
			case netguard.OriginBuiltin, netguard.OriginConfig, netguard.OriginDefault:
			default:
				origin = "other"
			}
			m.hostLimitRate.WithLabelValues(host, b.bucket, origin).Set(b.limit.Rate)
			m.hostLimitBurst.WithLabelValues(host, b.bucket, origin).Set(float64(b.limit.Burst))
		}
	}
}

// SetHostDemand sets angelia_host_demand_rps from the scheduler's estimate,
// by host label (netguard.Client.LimitFor), replacing earlier values. A host
// that is not in the limits table given to SetHostLimits is folded into
// "other", so the label set stays the closed one of ADR 0019 whatever the
// caller passes.
func (m *Metrics) SetHostDemand(rps map[string]float64) {
	m.mu.Lock()
	known := m.limitHosts
	m.mu.Unlock()
	sum := map[string]float64{}
	for host, v := range rps {
		host = strings.ToLower(host)
		if !known[host] {
			host = netguard.OtherHost
		}
		sum[host] += v
	}
	// Set the new values first and delete only the labels that disappeared,
	// so a scrape never sees a gap.
	m.mu.Lock()
	defer m.mu.Unlock()
	for host, v := range sum {
		m.hostDemand.WithLabelValues(host).Set(v)
	}
	for host := range m.demandSet {
		if _, ok := sum[host]; !ok {
			m.hostDemand.DeleteLabelValues(host)
		}
	}
	m.demandSet = map[string]bool{}
	for host := range sum {
		m.demandSet[host] = true
	}
}

// Source wraps src so that every List is counted by result in
// angelia_source_fetches_total. Results and errors pass through unchanged.
func (m *Metrics) Source(src source.Source) source.Source {
	return &countedSource{Source: src, fetches: m.fetches, kind: src.Kind()}
}

type countedSource struct {
	source.Source
	fetches *prometheus.CounterVec
	kind    string
}

func (s *countedSource) List(ctx context.Context, t source.Target, fs source.FetchState) (source.Result, error) {
	res, err := s.Source.List(ctx, t, fs)
	s.fetches.WithLabelValues(s.kind, fetchResult(res, err)).Inc()
	return res, err
}

// fetchResult classifies the outcome of a List call.
func fetchResult(res source.Result, err error) string {
	if err == nil {
		if res.NotModified {
			return ResultNotModified
		}
		return ResultOK
	}
	var se *netguard.StatusError
	switch {
	case errors.As(err, &se) && (se.StatusCode == http.StatusTooManyRequests || !se.RetryAt.IsZero()):
		return ResultRateLimited
	case errors.Is(err, context.Canceled):
		return ResultCanceled
	default:
		return ResultError
	}
}
