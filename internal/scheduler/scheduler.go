package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"os"
	"sync"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// Defaults for zero Options fields.
const (
	DefaultWorkers      = 4
	DefaultInterval     = time.Hour
	DefaultJitter       = 0.1
	DefaultBackoffBase  = time.Minute
	DefaultBackoffMax   = 24 * time.Hour
	DefaultLeaseTTL     = 5 * time.Minute
	DefaultPollInterval = time.Second
	// DefaultCheckTimeout bounds one check. It stays below DefaultLeaseTTL.
	DefaultCheckTimeout = 3 * time.Minute
	// MaxInterval caps every delay (interval, backoff), so that jitter
	// arithmetic cannot overflow time.Duration. Larger values are clamped.
	MaxInterval = 30 * 24 * time.Hour
	// writeTimeout bounds the final store writes of a check (target state,
	// lease release), which still run after shutdown has started.
	writeTimeout = 5 * time.Second
	// leasePruneEvery is how often expired target leases are deleted.
	leasePruneEvery = time.Hour
	// errorLogEvery limits how often a failing store is logged by Run.
	errorLogEvery = time.Minute
)

// errLeaseLost is the cancellation cause of a check whose lease could not be
// renewed: another holder may now own the target.
var errLeaseLost = errors.New("scheduler: target lease lost")

// errCheckTimeout is the cancellation cause of a check that ran longer than
// Options.CheckTimeout. It counts as a failed check.
var errCheckTimeout = errors.New("scheduler: check timed out")

// Checker checks one target. It is the seam between the scheduler and the
// detect pipeline (source registry, diff engine, policy), which plug in
// without the scheduler importing them.
type Checker interface {
	// Check fetches t once through the source registered for t.Kind, passing
	// t.FetchState, and commits what it found (new versions, events,
	// deliveries) before it returns. It returns the fetch state to keep for
	// the next check. The scheduler then stores that state, the next check
	// time and the error count, so Check does not write them.
	//
	// ctx is cancelled on shutdown and when the target's lease is lost; Check
	// must then return promptly with an error wrapping ctx.Err(), and the
	// scheduler leaves the target due without counting an error. ctx also
	// expires after Options.CheckTimeout, which counts as an error.
	//
	// The commit must be idempotent. A time-based lease cannot fully rule
	// out that two holders check one target at once (a stalled process, a
	// clock jump), and a crash between the commit and the scheduler's write
	// repeats the check. So Check inserts versions with ON CONFLICT DO
	// NOTHING, emits events only for the rows it actually inserted, and
	// does both in one transaction (ADR 0009 §5).
	Check(ctx context.Context, t store.Target) (Outcome, error)
}

// CheckerFunc adapts a function to Checker.
type CheckerFunc func(ctx context.Context, t store.Target) (Outcome, error)

// Check implements Checker.
func (f CheckerFunc) Check(ctx context.Context, t store.Target) (Outcome, error) { return f(ctx, t) }

// Outcome is the result of a successful check.
type Outcome struct {
	// FetchState replaces the target's fetch state (ETag, Last-Modified,
	// cursor). Nil keeps the current state.
	FetchState json.RawMessage
	// Interval overrides Options.Interval for this target's next check, for
	// example the shortest interval of the watches on it. Zero keeps it;
	// values above MaxInterval are clamped.
	Interval time.Duration
}

// Metrics receives scheduler observations. Phase 1 exports them to
// Prometheus; until then a nil Metrics discards them.
type Metrics interface {
	// DueBacklog is the number of due targets waiting for a worker (not
	// leased), observed on every poll.
	DueBacklog(n int64)
	// CheckLag is how late a check starts: lease time minus NextCheckAt.
	CheckLag(d time.Duration)
}

// Store is the part of store.Store the scheduler uses.
type Store interface {
	LeaseDueTargets(ctx context.Context, holder string, now, until time.Time, limit int) ([]store.Target, error)
	CountDueTargets(ctx context.Context, now time.Time) (int64, error)
	HasDueTargets(ctx context.Context, now time.Time) (bool, error)
	AcquireLease(ctx context.Context, name, holder string, now, until time.Time) (bool, error)
	ReleaseLease(ctx context.Context, name, holder string) error
	UpdateTargetLeased(ctx context.Context, t store.Target, leaseName, holder string, now time.Time) (bool, error)
	DeleteExpiredTargetLeases(ctx context.Context, now time.Time) (int64, error)
}

// Options configures a Scheduler. Zero fields take the Default* values.
type Options struct {
	// Workers is the number of targets checked at the same time.
	Workers int
	// Interval is the time between two checks of a target. Values above
	// MaxInterval are clamped.
	Interval time.Duration
	// Jitter is the fraction by which every delay is spread, up and down.
	// Zero means DefaultJitter; a negative value disables jitter.
	Jitter float64
	// BackoffBase is the delay after the first consecutive error; it doubles
	// with every further error up to BackoffMax (clamped to MaxInterval),
	// then jitter applies. A success resets it.
	BackoffBase time.Duration
	BackoffMax  time.Duration
	// LeaseTTL is how long a target lease lasts. A running check renews it
	// every LeaseTTL/3; a crashed holder's leases are taken over after it.
	LeaseTTL time.Duration
	// CheckTimeout bounds one check; a check that runs longer is cancelled
	// and counts as an error. It must be below LeaseTTL. Zero means
	// DefaultCheckTimeout, or LeaseTTL/2 when LeaseTTL is not above it.
	CheckTimeout time.Duration
	// PollInterval is how often the store is asked for due targets.
	PollInterval time.Duration
	// Holder identifies this scheduler in the lease table. Empty means the
	// host name plus a random suffix; two schedulers must never share one.
	Holder  string
	Clock   Clock
	Rand    func() float64 // in [0, 1); nil means math/rand/v2
	Logger  *slog.Logger
	Metrics Metrics
}

// Scheduler is the "scheduler" role: it leases due targets from the store,
// checks each through the Checker on a bounded worker pool, and records
// when to check it next. It schedules targets, not watches, so any number
// of watches on one target cost one fetch per interval.
//
// Per-host politeness is not its job: sources fetch through the netguard
// client, whose per-host limiter already paces requests. The scheduler only
// bounds concurrency.
type Scheduler struct {
	store   Store
	checker Checker
	opts    Options
	log     *slog.Logger

	mu       sync.Mutex
	inflight int
	wg       sync.WaitGroup
	freed    chan struct{} // a worker finished while every slot was busy

	// Owned by the poll loop (Run, or a test calling poll directly).
	lastPrune                time.Time // zero: prune on the first poll
	failingSince, lastErrLog time.Time // zero: the store is not failing

	// testHookIdle, if set, runs each time Run is about to wait.
	testHookIdle func()
}

// New returns a Scheduler. It does not start anything; call Run.
func New(st Store, c Checker, opts Options) (*Scheduler, error) {
	if st == nil || c == nil {
		return nil, errors.New("scheduler: nil store or checker")
	}
	if opts.Workers < 0 || opts.Interval < 0 || opts.BackoffBase < 0 || opts.BackoffMax < 0 ||
		opts.LeaseTTL < 0 || opts.PollInterval < 0 || opts.CheckTimeout < 0 || opts.Jitter >= 1 {
		return nil, fmt.Errorf("scheduler: invalid options: negative duration or worker count, or jitter >= 1")
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	if opts.Workers == 0 {
		opts.Workers = DefaultWorkers
	}
	def(&opts.Interval, DefaultInterval)
	def(&opts.BackoffBase, DefaultBackoffBase)
	def(&opts.BackoffMax, DefaultBackoffMax)
	def(&opts.LeaseTTL, DefaultLeaseTTL)
	def(&opts.PollInterval, DefaultPollInterval)
	if opts.CheckTimeout == 0 {
		opts.CheckTimeout = DefaultCheckTimeout
		if opts.CheckTimeout >= opts.LeaseTTL {
			opts.CheckTimeout = opts.LeaseTTL / 2
		}
	}
	if opts.CheckTimeout >= opts.LeaseTTL {
		return nil, fmt.Errorf("scheduler: invalid options: check timeout %v must be below lease TTL %v", opts.CheckTimeout, opts.LeaseTTL)
	}
	opts.Interval = min(opts.Interval, MaxInterval)
	opts.BackoffMax = min(opts.BackoffMax, MaxInterval)
	switch {
	case opts.Jitter == 0:
		opts.Jitter = DefaultJitter
	case opts.Jitter < 0:
		opts.Jitter = 0
	}
	if opts.Holder == "" {
		host, _ := os.Hostname()
		opts.Holder = host + "-" + rand.Text()[:10]
	}
	if opts.Clock == nil {
		opts.Clock = realClock{}
	}
	if opts.Rand == nil {
		opts.Rand = mrand.Float64 //nolint:gosec // G404: jitter needs no cryptographic randomness
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{
		store: st, checker: c, opts: opts, freed: make(chan struct{}, 1),
		log: log.With("role", "scheduler", "holder", opts.Holder),
	}, nil
}

// Name implements supervisor.Role.
func (s *Scheduler) Name() string { return "scheduler" }

// Run implements supervisor.Role. It polls for due targets until ctx is
// cancelled. Cancellation also cancels the running checks; each releases
// its lease without touching the target, which stays due, and Run returns
// nil once they have all stopped.
func (s *Scheduler) Run(ctx context.Context) error {
	defer s.wg.Wait()
	for {
		s.poll(ctx)
		timer, stop := s.opts.Clock.Timer(s.opts.PollInterval)
		if s.testHookIdle != nil {
			s.testHookIdle()
		}
		select {
		case <-ctx.Done():
			stop()
			return nil
		case <-timer:
		case <-s.freed:
			stop()
		}
	}
}

// poll leases as many due targets as there are free workers and starts a
// check for each. It returns the number of checks started.
func (s *Scheduler) poll(ctx context.Context) int {
	if ctx.Err() != nil {
		return 0
	}
	now := s.opts.Clock.Now()
	s.pruneLeases(ctx, now)
	// A cheap read decides whether the leasing write transaction is worth
	// running, so idle polls take no write lock (SQLite has only one).
	due := int64(-1) // unknown
	if s.opts.Metrics != nil {
		if n, err := s.store.CountDueTargets(ctx, now); err != nil {
			s.log.WarnContext(ctx, "count due targets", "error", err)
		} else {
			s.opts.Metrics.DueBacklog(n)
			due = n
		}
	}
	s.mu.Lock()
	free := s.opts.Workers - s.inflight
	s.mu.Unlock()
	if free <= 0 {
		return 0
	}
	if due < 0 {
		ok, err := s.store.HasDueTargets(ctx, now)
		if err != nil {
			s.storeFailed(ctx, now, "check for due targets", err)
			return 0
		}
		due = 0
		if ok {
			due = 1
		}
	}
	if due == 0 {
		s.storeOK(ctx)
		return 0
	}
	targets, err := s.store.LeaseDueTargets(ctx, s.opts.Holder, now, now.Add(s.opts.LeaseTTL), free)
	if err != nil {
		s.storeFailed(ctx, now, "lease due targets", err)
		return 0
	}
	s.storeOK(ctx)
	s.mu.Lock()
	s.inflight += len(targets)
	s.mu.Unlock()
	for _, t := range targets {
		if s.opts.Metrics != nil {
			s.opts.Metrics.CheckLag(now.Sub(t.NextCheckAt))
		}
		leasedUntil := now.Add(s.opts.LeaseTTL)
		s.wg.Go(func() {
			s.check(ctx, t, leasedUntil)
			s.mu.Lock()
			full := s.inflight == s.opts.Workers
			s.inflight--
			s.mu.Unlock()
			if full {
				select {
				case s.freed <- struct{}{}:
				default:
				}
			}
		})
	}
	return len(targets)
}

// storeFailed logs a failed poll at most once per errorLogEvery while the
// store keeps failing, so an outage does not log every PollInterval.
func (s *Scheduler) storeFailed(ctx context.Context, now time.Time, op string, err error) {
	if ctx.Err() != nil {
		return
	}
	if s.failingSince.IsZero() {
		s.failingSince = now
	}
	if !s.lastErrLog.IsZero() && now.Sub(s.lastErrLog) < errorLogEvery {
		return
	}
	s.lastErrLog = now
	s.log.ErrorContext(ctx, op, "error", err, "failing_for", now.Sub(s.failingSince))
}

// storeOK records that a poll reached the store again after failures.
func (s *Scheduler) storeOK(ctx context.Context) {
	if s.failingSince.IsZero() {
		return
	}
	s.log.InfoContext(ctx, "store reachable again", "failing_for", s.opts.Clock.Now().Sub(s.failingSince))
	s.failingSince, s.lastErrLog = time.Time{}, time.Time{}
}

// pruneLeases deletes expired target leases, left by crashed holders, once
// every leasePruneEvery. Leasing ignores expired rows anyway; this only
// keeps the lease table from growing.
func (s *Scheduler) pruneLeases(ctx context.Context, now time.Time) {
	if !s.lastPrune.IsZero() && now.Sub(s.lastPrune) < leasePruneEvery {
		return
	}
	s.lastPrune = now
	n, err := s.store.DeleteExpiredTargetLeases(ctx, now)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			s.log.WarnContext(ctx, "delete expired target leases", "error", err)
		}
	case n > 0:
		s.log.InfoContext(ctx, "deleted expired target leases", "count", n)
	}
}

// check runs one leased check, records the next check time and releases
// the lease. leasedUntil is when the lease taken by poll expires.
func (s *Scheduler) check(ctx context.Context, t store.Target, leasedUntil time.Time) {
	lease := store.TargetLeaseName(t.ID)
	log := s.log.With("target", t.ID, "kind", t.Kind)
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
		defer cancel()
		if err := s.store.ReleaseLease(rctx, lease, s.opts.Holder); err != nil {
			log.WarnContext(rctx, "release lease", "error", err)
		}
	}()

	cctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		s.keepLease(cctx, cancel, lease, leasedUntil, log)
	}()
	tctx, tcancel := context.WithTimeoutCause(cctx, s.opts.CheckTimeout, errCheckTimeout)
	out, err := s.checker.Check(tctx, t)
	timedOut := err != nil && errors.Is(context.Cause(tctx), errCheckTimeout)
	tcancel()
	cancel(nil)
	<-renewDone

	// A lost lease, or a check cut short by shutdown, leaves the target due
	// for the next holder and does not count as an error. A check that
	// completed before shutdown is still recorded.
	if lost := errors.Is(context.Cause(cctx), errLeaseLost); lost || (err != nil && ctx.Err() != nil) {
		log.InfoContext(ctx, "check abandoned", "lease_lost", lost, "error", err)
		return
	}

	now := s.opts.Clock.Now()
	next := t
	if err != nil {
		if timedOut {
			err = fmt.Errorf("%w after %v: %w", errCheckTimeout, s.opts.CheckTimeout, err)
		}
		next.ErrorCount++
		// Cap first, then jitter, so targets at the cap do not all land on
		// exactly BackoffMax.
		next.NextCheckAt = now.Add(s.jitter(Backoff(next.ErrorCount, s.opts.BackoffBase, s.opts.BackoffMax)))
		log.WarnContext(ctx, "check failed", "error", err, "errors", next.ErrorCount, "next_check_at", next.NextCheckAt)
	} else {
		interval := s.opts.Interval
		if t.Interval > 0 { // set by a watch file (docs/watch-file.md)
			interval = min(t.Interval, MaxInterval)
		}
		if out.Interval > 0 {
			interval = min(out.Interval, MaxInterval)
		}
		if out.FetchState != nil {
			next.FetchState = out.FetchState
		}
		next.ErrorCount = 0
		next.LastCheckedAt = now
		next.NextCheckAt = now.Add(s.jitter(interval))
	}
	wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer wcancel()
	// The write is fenced by the lease: if another holder took the target
	// over meanwhile, its schedule wins and this result is dropped.
	held, err := s.store.UpdateTargetLeased(wctx, next, lease, s.opts.Holder, now)
	switch {
	case err != nil:
		log.ErrorContext(wctx, "record check", "error", err)
	case !held:
		log.WarnContext(wctx, "lease lost before the check was recorded; leaving the target to its new holder")
	}
}

// keepLease renews the lease every LeaseTTL/3 until ctx is done. It cancels
// the check with errLeaseLost if the lease was taken over, or if renewals
// kept failing until the last renewed expiry passed: from then on another
// holder may own the target.
func (s *Scheduler) keepLease(ctx context.Context, cancel context.CancelCauseFunc, lease string, expires time.Time, log *slog.Logger) {
	every := s.opts.LeaseTTL / 3
	for {
		timer, stop := s.opts.Clock.Timer(every)
		select {
		case <-ctx.Done():
			stop()
			return
		case <-timer:
		}
		now := s.opts.Clock.Now()
		until := now.Add(s.opts.LeaseTTL)
		ok, err := s.store.AcquireLease(ctx, lease, s.opts.Holder, now, until)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			if !now.Before(expires) {
				log.WarnContext(ctx, "lease expired while renewals failed, abandoning check", "error", err)
				cancel(errLeaseLost)
				return
			}
			log.WarnContext(ctx, "renew lease", "error", err, "expires_at", expires)
		case !ok:
			log.WarnContext(ctx, "lease lost, abandoning check")
			cancel(errLeaseLost)
			return
		default:
			expires = until
		}
	}
}

func (s *Scheduler) jitter(d time.Duration) time.Duration {
	return Jittered(d, s.opts.Jitter, s.opts.Rand())
}
