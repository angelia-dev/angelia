package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// Host demand estimate (story #220, ADR 0027 §4 and question 9).
//
// The scheduler does not throttle per host (ADR 0009 §6), so a limit that is
// too low for the stored targets only shows up later, as rate-limit waits and
// check lag. The estimate makes it visible at once: for every host, the sum
// over its targets of requests per check ÷ interval, compared with the
// effective limit of the host's bucket.
const (
	// DemandWarnRatio is the share of a host's limit above which the
	// estimate logs a warning.
	DemandWarnRatio = 0.8
	// RequestsPerCheck is the number of requests a check of one target
	// sends to its host once the target is known and the upstream answers a
	// conditional request: one. Sources that page through a long listing, or
	// look up a new tag's commit, send more on the check that finds news, so
	// the estimate is a floor.
	RequestsPerCheck = 1
	// DefaultDemandEvery is how often the estimate is refreshed: the
	// watches change through the API or an apply, which the scheduler does
	// not hear about, so it asks the store again.
	DefaultDemandEvery = time.Minute
)

// DemandStore is the part of the store the estimate reads. *sqlite.Store
// implements it.
type DemandStore interface {
	TargetDemand(ctx context.Context) ([]store.TargetDemand, error)
}

// HostLimitInfo is the effective limit of one host's bucket, as the HTTP
// client applies it.
type HostLimitInfo struct {
	// Label is the host label of the metrics (ADR 0019): the host itself
	// when it has its own entry in the limits table, else "other".
	Label string
	// RPS and Burst are the refill rate and the size of the bucket.
	RPS   float64
	Burst int
	// Fixed is true when the limit is a published budget that
	// http.host_limits cannot raise (ADR 0027 §4).
	Fixed bool
}

// DemandOptions configures a Demand.
type DemandOptions struct {
	Store DemandStore
	// SourceHost returns the host a source (the name a target was created
	// under) sends its requests to, and whether it sends a credential, which
	// selects the authenticated bucket. ok is false for a source whose host
	// is unknown, such as a plugin; its targets are left out.
	SourceHost func(source string) (host string, authenticated, ok bool)
	// Limit returns the effective limit of a host's bucket. It must resolve
	// a host the way the HTTP client does (netguard.Client.LimitFor).
	Limit func(host string, authenticated bool) HostLimitInfo
	// Report receives the estimate by host label, in requests per second,
	// after every refresh. Hosts without targets are absent. Nil discards it.
	Report func(rps map[string]float64)
	Logger *slog.Logger
}

// Demand estimates the request demand per host from the stored targets and
// their intervals. It is not safe for concurrent use: the scheduler calls
// Refresh from its poll loop.
type Demand struct {
	opts   DemandOptions
	log    *slog.Logger
	warned map[string]string // bucket key -> the warning's numbers, while it holds
}

// NewDemand returns a Demand. Store, SourceHost and Limit are required.
func NewDemand(opts DemandOptions) (*Demand, error) {
	if opts.Store == nil || opts.SourceHost == nil || opts.Limit == nil {
		return nil, fmt.Errorf("scheduler: demand needs a store, SourceHost and Limit")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Demand{opts: opts, log: log.With("role", "scheduler"), warned: map[string]string{}}, nil
}

// bucketDemand is the demand on one bucket of one host.
type bucketDemand struct {
	host          string
	authenticated bool
	rps           float64
	targets       int64
	shortest      time.Duration
}

// Refresh reads the targets, reports the estimate and logs a warning for
// every bucket whose demand is above DemandWarnRatio of its limit. A warning
// is logged again only when its numbers change, and again after the demand
// has fallen below the threshold and risen over it. defaultInterval is the
// interval of a target that has none (the scheduler's Options.Interval).
func (d *Demand) Refresh(ctx context.Context, defaultInterval time.Duration) error {
	groups, err := d.opts.Store.TargetDemand(ctx)
	if err != nil {
		return fmt.Errorf("target demand: %w", err)
	}
	type key struct {
		host          string
		authenticated bool
	}
	buckets := map[key]*bucketDemand{}
	for _, g := range groups {
		host, authed, ok := d.opts.SourceHost(g.Name)
		if !ok || g.Targets <= 0 {
			continue
		}
		iv := g.Interval
		if iv <= 0 {
			iv = defaultInterval
		}
		iv = min(iv, MaxInterval)
		k := key{host, authed}
		b := buckets[k]
		if b == nil {
			b = &bucketDemand{host: host, authenticated: authed, shortest: iv}
			buckets[k] = b
		}
		b.rps += float64(g.Targets) * RequestsPerCheck / iv.Seconds()
		b.targets += g.Targets
		b.shortest = min(b.shortest, iv)
	}

	byLabel := map[string]float64{}
	still := map[string]bool{}
	order := slices.SortedFunc(maps.Values(buckets), func(a, b *bucketDemand) int {
		return strings.Compare(bucketKey(a), bucketKey(b))
	})
	for _, b := range order {
		lim := d.opts.Limit(b.host, b.authenticated)
		byLabel[lim.Label] += b.rps
		if !(b.rps > DemandWarnRatio*lim.RPS) {
			continue
		}
		k := bucketKey(b)
		still[k] = true
		sig := fmt.Sprintf("%.4g/%.4g/%d", b.rps, lim.RPS, b.targets)
		if d.warned[k] == sig {
			continue
		}
		d.warned[k] = sig
		d.warn(ctx, b, lim)
	}
	for k := range d.warned {
		if !still[k] {
			delete(d.warned, k)
		}
	}
	if d.opts.Report != nil {
		d.opts.Report(byLabel)
	}
	return nil
}

func bucketKey(b *bucketDemand) string {
	if b.authenticated {
		return b.host + "|authenticated"
	}
	return b.host + "|anonymous"
}

func (d *Demand) warn(ctx context.Context, b *bucketDemand, lim HostLimitInfo) {
	// ADR 0027 §4: rps >= targets on the host / shortest interval *
	// requests per check.
	need := float64(b.targets) * RequestsPerCheck / b.shortest.Seconds()
	bucket := "anonymous"
	if b.authenticated {
		bucket = "authenticated"
	}
	advice := fmt.Sprintf("size the limit as rps >= targets on the host / shortest interval * requests per check (here at least %.3g); set http.host_limits.hosts.%s.rps", need, b.host)
	if lim.Fixed {
		advice = fmt.Sprintf("the limit of %s is a published budget that http.host_limits cannot raise; lengthen the watch intervals or spread the targets over more hosts", b.host)
	}
	d.log.WarnContext(ctx, fmt.Sprintf("estimated request demand on %s is above %.0f%% of its rate limit; %s", b.host, DemandWarnRatio*100, advice),
		"host", b.host, "bucket", bucket,
		"demand_rps", round3(b.rps), "limit_rps", round3(lim.RPS), "burst", lim.Burst,
		"targets", b.targets, "shortest_interval", b.shortest.String(), "min_rps", round3(need))
}

func round3(f float64) float64 {
	return float64(int64(f*1000+0.5)) / 1000
}

// DemandEstimator refreshes the host demand estimate. *Demand implements it.
type DemandEstimator interface {
	Refresh(ctx context.Context, defaultInterval time.Duration) error
}

// refreshDemand runs the estimate on the first poll and then every
// Options.DemandEvery. A failure is logged and retried at the next poll
// after the delay, so a failing store cannot flood the log.
func (s *Scheduler) refreshDemand(ctx context.Context, now time.Time) {
	if s.opts.Demand == nil {
		return
	}
	if !s.lastDemand.IsZero() && now.Sub(s.lastDemand) < s.opts.DemandEvery {
		return
	}
	s.lastDemand = now
	if err := s.opts.Demand.Refresh(ctx, s.opts.Interval); err != nil && ctx.Err() == nil {
		s.log.WarnContext(ctx, "estimate host demand", "error", err)
	}
}
