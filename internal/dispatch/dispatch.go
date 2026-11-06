package dispatch

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"os"
	"runtime/debug"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/angelia-dev/angelia/internal/sink"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Defaults for zero Options fields.
const (
	DefaultWorkers     = 8
	DefaultMaxAttempts = 8
	DefaultRetryBase   = 12 * time.Minute
	DefaultRetryMax    = 12 * time.Hour
	DefaultLeaseTTL    = 2 * time.Minute
	// DefaultDeliverTimeout bounds one attempt (rendering and the sink
	// call). It stays well below DefaultLeaseTTL, so a holder gives up
	// before its lease can expire, and DefaultDeliverTimeout + writeTimeout
	// stays below supervisor.DefaultDrainTimeout (25 s), so an attempt in
	// progress at shutdown is still recorded.
	DefaultDeliverTimeout = 15 * time.Second
	DefaultPollInterval   = time.Second
	// writeTimeout bounds the store write that ends an attempt, which still
	// runs after shutdown has started. It stays above the SQLite busy
	// timeout (5 s; checked by a test), so write contention delays the
	// record instead of dropping it and causing a duplicate send (#176).
	writeTimeout = 8 * time.Second
	// minLeaseMargin is the least time between the end of DeliverTimeout
	// and the lease's expiry: the clock skew tolerated between hosts.
	minLeaseMargin = 30 * time.Second
	// maxErrorLen caps the stored last_error, in bytes.
	maxErrorLen = 1024
	// errorLogEvery limits how often a failing store is logged by Run.
	errorLogEvery = time.Minute
)

// Reasons a delivery is set dead without calling a sink (maintainer
// decision on #107: a deleted subscription or sink fails its deliveries).
const (
	ReasonSubscriptionDeleted = "subscription deleted"
	ReasonSinkDeleted         = "sink deleted"
)

// Outcomes of an attempt, as reported to Metrics.
const (
	OutcomeDelivered = store.DeliveryDelivered
	OutcomeRetry     = "retry" // failed, due again later
	OutcomeDead      = store.DeliveryDead
)

// Metrics receives dispatcher observations. Phase 1 exports them to
// Prometheus (internal/metrics); a nil Metrics discards them.
type Metrics interface {
	// AttemptFinished is called once an attempt's outcome (OutcomeDelivered,
	// OutcomeRetry or OutcomeDead) is recorded in the store. An attempt that
	// lost its lease, or whose result could not be written, is not reported.
	AttemptFinished(outcome string)
}

// Store is the part of store.Store the dispatcher uses.
type Store interface {
	LeaseDueDeliveries(ctx context.Context, holder string, now, until time.Time, limit int) ([]store.LeasedDelivery, error)
	HasDueDeliveries(ctx context.Context, now time.Time) (bool, error)
	FinishDelivery(ctx context.Context, f store.DeliveryFinish, holder string) (bool, error)
}

// Sinks finds the Sink of a kind; *sink.Registry implements it.
type Sinks interface {
	Get(kind string) (sink.Sink, error)
}

// Renderer builds the message of a delivery from its subscription's
// template and the event's stored CloudEvents JSON (envelope). The
// templating story (#114) provides the real one; a permanent error
// (sink.Permanent) sets the delivery dead. Render must not keep or modify
// envelope.
type Renderer interface {
	Render(ctx context.Context, template string, envelope []byte) (sink.Rendered, error)
}

// DefaultRenderer sends the CloudEvents JSON of the event, which is the
// default payload (architecture.md 3.5). It rejects a non-empty template
// as permanent until templating exists.
type DefaultRenderer struct{}

// Render implements Renderer.
func (DefaultRenderer) Render(_ context.Context, template string, envelope []byte) (sink.Rendered, error) {
	if template != "" {
		return sink.Rendered{}, sink.Permanent(errors.New("dispatch: subscription templates are not supported yet"))
	}
	return sink.Rendered{Body: bytes.Clone(envelope), ContentType: sink.CloudEventsContentType}, nil
}

// Options configures a Dispatcher. Zero fields take the Default* values.
type Options struct {
	// Workers is the number of deliveries sent at the same time.
	Workers int
	// MaxAttempts is the number of attempts before a delivery is dead.
	MaxAttempts int64
	// RetryBase and RetryMax shape the wait between attempts: after the
	// n-th failure it is drawn uniformly from [0, min(RetryBase·2^(n−1),
	// RetryMax)) (full jitter, see RetryDelay).
	RetryBase, RetryMax time.Duration
	// LeaseTTL is how long a delivery lease lasts; a crashed holder's
	// deliveries are sent again after it.
	LeaseTTL time.Duration
	// DeliverTimeout bounds one attempt (rendering and the sink call). It
	// must be at most LeaseTTL − max(30 s, LeaseTTL/4); that margin is the
	// clock skew tolerated between dispatchers. Zero means
	// DefaultDeliverTimeout, or that bound when it is lower. Keep
	// DeliverTimeout + writeTimeout (8 s) below the supervisor's drain timeout, so that
	// attempts in progress at shutdown are recorded.
	DeliverTimeout time.Duration
	// PollInterval is how often the store is asked for due deliveries.
	PollInterval time.Duration
	// Holder identifies this dispatcher in the lease table. Empty means the
	// host name plus a random suffix; two dispatchers must never share one.
	Holder string
	Clock  Clock
	// Rand returns a number in [0, 1); nil means math/rand/v2. Workers call
	// it concurrently, so it must be safe for concurrent use.
	Rand   func() float64
	Logger *slog.Logger
	// TracerProvider starts the root span of every delivery attempt. Nil
	// records nothing.
	TracerProvider trace.TracerProvider
	// Renderer builds messages; nil means DefaultRenderer.
	Renderer Renderer
	// DecryptConfig turns sink.config_json_encrypted into the sink's
	// configuration; nil passes it through (encryption at rest is not
	// implemented yet, maintainer decision on #107).
	DecryptConfig func([]byte) ([]byte, error)
	// Metrics receives attempt outcomes; nil discards them.
	Metrics Metrics
	// LookupEnv reads the environment variable named by each *_env key of a
	// sink's configuration, for sink.Request.Secrets (ADR 0023 section 4).
	// Nil means os.LookupEnv. It is called on every attempt: values are not
	// cached.
	LookupEnv func(string) (string, bool)
}

// Dispatcher is the "dispatcher" role: it leases due deliveries from the
// store, sends each through the sink of its subscription on a bounded
// worker pool, and records the outcome: delivered, retried with backoff, or
// dead after a permanent error or the last attempt.
//
// Delivery is at least once. A delivery is sent by one holder at a time
// (its lease, ADR 0017); a holder that crashes mid-send leaves it in flight
// and another holder sends it again once the lease expires. Consumers drop
// duplicates by the event id or the idempotency key.
type Dispatcher struct {
	store Store
	sinks Sinks
	opts  Options
	log   *slog.Logger

	mu       sync.Mutex
	inflight int
	sending  map[string]struct{} // ids of the deliveries being attempted
	wg       sync.WaitGroup
	freed    chan struct{} // a worker finished while every slot was busy

	// Owned by the poll loop.
	failingSince, lastErrLog time.Time
}

// New returns a Dispatcher. It does not start anything; call Run.
func New(st Store, sinks Sinks, opts Options) (*Dispatcher, error) {
	if st == nil || sinks == nil {
		return nil, errors.New("dispatch: nil store or sinks")
	}
	if opts.Workers < 0 || opts.MaxAttempts < 0 || opts.RetryBase < 0 || opts.RetryMax < 0 ||
		opts.LeaseTTL < 0 || opts.DeliverTimeout < 0 || opts.PollInterval < 0 {
		return nil, errors.New("dispatch: invalid options: negative value")
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	if opts.Workers == 0 {
		opts.Workers = DefaultWorkers
	}
	if opts.MaxAttempts == 0 {
		opts.MaxAttempts = DefaultMaxAttempts
	}
	def(&opts.RetryBase, DefaultRetryBase)
	def(&opts.RetryMax, DefaultRetryMax)
	def(&opts.LeaseTTL, DefaultLeaseTTL)
	def(&opts.PollInterval, DefaultPollInterval)
	maxDeliver := opts.LeaseTTL - max(minLeaseMargin, opts.LeaseTTL/4)
	if opts.DeliverTimeout == 0 {
		opts.DeliverTimeout = min(DefaultDeliverTimeout, maxDeliver)
	}
	if opts.DeliverTimeout <= 0 || opts.DeliverTimeout > maxDeliver {
		return nil, fmt.Errorf("dispatch: invalid options: deliver timeout %v must be positive and at most lease TTL %v minus max(%v, TTL/4)",
			opts.DeliverTimeout, opts.LeaseTTL, minLeaseMargin)
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
	if opts.Renderer == nil {
		opts.Renderer = DefaultRenderer{}
	}
	if opts.DecryptConfig == nil {
		opts.DecryptConfig = func(b []byte) ([]byte, error) { return b, nil }
	}
	if opts.LookupEnv == nil {
		opts.LookupEnv = os.LookupEnv
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Dispatcher{
		store: st, sinks: sinks, opts: opts, freed: make(chan struct{}, 1), sending: map[string]struct{}{},
		log: log.With("role", "dispatcher", "holder", opts.Holder),
	}, nil
}

// Name implements supervisor.Role.
func (d *Dispatcher) Name() string { return "dispatcher" }

// Run implements supervisor.Role. It polls for due deliveries until ctx is
// cancelled, then waits for the attempts already started: they are not
// cancelled (that would turn a send into a likely duplicate) but end within
// DeliverTimeout. It returns nil.
func (d *Dispatcher) Run(ctx context.Context) error {
	defer d.wg.Wait()
	for {
		d.poll(ctx)
		timer, stop := d.opts.Clock.Timer(d.opts.PollInterval)
		select {
		case <-ctx.Done():
			stop()
			return nil
		case <-timer:
		case <-d.freed:
			stop()
		}
	}
}

// poll leases as many due deliveries as there are free workers and starts
// an attempt for each. It returns the number of attempts started.
func (d *Dispatcher) poll(ctx context.Context) int {
	if ctx.Err() != nil {
		return 0
	}
	d.mu.Lock()
	free := d.opts.Workers - d.inflight
	d.mu.Unlock()
	if free <= 0 {
		return 0
	}
	now := d.opts.Clock.Now()
	// A cheap read decides whether the leasing write transaction is worth
	// running, so idle polls take no write lock (SQLite has only one).
	due, err := d.store.HasDueDeliveries(ctx, now)
	if err != nil {
		d.storeFailed(ctx, now, "check for due deliveries", err)
		return 0
	}
	if !due {
		d.storeOK(ctx)
		return 0
	}
	leased, err := d.store.LeaseDueDeliveries(ctx, d.opts.Holder, now, now.Add(d.opts.LeaseTTL), free)
	if err != nil {
		d.storeFailed(ctx, now, "lease due deliveries", err)
		return 0
	}
	d.storeOK(ctx)
	// A delivery whose earlier attempt here is still running (its sink
	// outlived the lease) is not started twice. The new lease stays: it
	// expires and the delivery is due again, while the old attempt's result
	// is fenced off by the attempt number.
	start := leased[:0]
	d.mu.Lock()
	for _, ld := range leased {
		if _, busy := d.sending[ld.Delivery.ID]; busy {
			d.log.WarnContext(ctx, "delivery leased again while its previous attempt is still running; skipped",
				"delivery", ld.Delivery.ID, "attempt", ld.Delivery.Attempts)
			continue
		}
		d.sending[ld.Delivery.ID] = struct{}{}
		start = append(start, ld)
	}
	d.inflight += len(start)
	d.mu.Unlock()
	for _, ld := range start {
		d.wg.Go(func() {
			d.attempt(context.WithoutCancel(ctx), ld)
			d.mu.Lock()
			delete(d.sending, ld.Delivery.ID)
			full := d.inflight == d.opts.Workers
			d.inflight--
			d.mu.Unlock()
			if full {
				select {
				case d.freed <- struct{}{}:
				default:
				}
			}
		})
	}
	return len(start)
}

// storeFailed logs a failed poll at most once per errorLogEvery while the
// store keeps failing.
func (d *Dispatcher) storeFailed(ctx context.Context, now time.Time, op string, err error) {
	if ctx.Err() != nil {
		return
	}
	if d.failingSince.IsZero() {
		d.failingSince = now
	}
	if !d.lastErrLog.IsZero() && now.Sub(d.lastErrLog) < errorLogEvery {
		return
	}
	d.lastErrLog = now
	d.log.ErrorContext(ctx, op, "error", err, "failing_for", now.Sub(d.failingSince))
}

// storeOK records that a poll reached the store again after failures.
func (d *Dispatcher) storeOK(ctx context.Context) {
	if d.failingSince.IsZero() {
		return
	}
	d.log.InfoContext(ctx, "store reachable again", "failing_for", d.opts.Clock.Now().Sub(d.failingSince))
	d.failingSince, d.lastErrLog = time.Time{}, time.Time{}
}

// attempt sends one leased delivery and records how it ended.
func (d *Dispatcher) attempt(ctx context.Context, ld store.LeasedDelivery) {
	del := ld.Delivery
	// A delivery is its own trace, linked to the check that created it.
	ctx, span := telemetry.StartRoot(ctx, d.opts.TracerProvider, "angelia.delivery", telemetry.LinkFromTraceParent(context.WithoutCancel(ctx), del.TraceParent),
		attribute.String("angelia.delivery.id", del.ID), attribute.String("angelia.event.id", ld.Event.ID),
		attribute.String("angelia.subscription.id", del.SubscriptionID), attribute.Int64("angelia.delivery.attempt", del.Attempts))
	defer span.End()
	if ld.Sink != nil {
		span.SetAttributes(attribute.String("angelia.sink.kind", ld.Sink.Kind))
	}
	log := d.log.With("delivery", del.ID, "event", ld.Event.ID, "subscription", del.SubscriptionID, "attempt", del.Attempts)
	f := store.DeliveryFinish{ID: del.ID, Attempt: del.Attempts}
	// A replay starts a new retry budget: only the attempts since the last
	// replay count (spec docs/api.md decision 28, ADR 0017 amendment).
	used := max(del.Attempts-del.ReplayBaseAttempts, 0) // a base above attempts is never written; guard anyway
	if used > d.opts.MaxAttempts {
		// The last attempt was leased but never recorded: its holder
		// crashed or stalled. Sending again could crash this process too
		// (a poison delivery), so stop here.
		f.Status = store.DeliveryDead
		f.LastError = fmt.Sprintf("giving up after %d attempts (previous attempt did not finish)", used-1)
		span.SetAttributes(attribute.String("angelia.delivery.outcome", f.Status))
		log.ErrorContext(ctx, "delivery dead: attempts exhausted, the last one did not finish")
		d.finish(ctx, f, log)
		return
	}
	res, err := d.safeSend(ctx, ld, log)
	msg := ""
	if err != nil {
		msg = sink.RedactURL(err)
	}

	now := d.opts.Clock.Now()
	switch {
	case err == nil:
		f.Status = store.DeliveryDelivered
		log.InfoContext(ctx, "delivered", "status_code", res.StatusCode)
	case sink.IsPermanent(err):
		f.Status, f.LastError = store.DeliveryDead, truncate(msg)
		log.ErrorContext(ctx, "delivery dead: permanent error", "error", msg)
	case used >= d.opts.MaxAttempts:
		f.Status = store.DeliveryDead
		f.LastError = truncate(fmt.Sprintf("giving up after %d attempts: %s", used, msg))
		log.ErrorContext(ctx, "delivery dead: attempts exhausted", "error", msg)
	default:
		wait := RetryDelay(used, d.opts.RetryBase, d.opts.RetryMax, d.opts.Rand())
		wait = max(wait, min(sink.RetryAfter(err), d.opts.RetryMax))
		f.Status, f.NextAttemptAt, f.LastError = store.DeliveryPending, now.Add(wait), truncate(msg)
		log.WarnContext(ctx, "delivery failed, will retry", "error", msg, "next_attempt_at", f.NextAttemptAt)
	}
	span.SetAttributes(attribute.String("angelia.delivery.outcome", f.Status))
	if res.StatusCode != 0 {
		span.SetAttributes(attribute.Int("http.response.status_code", res.StatusCode))
	}
	class := ""
	if sink.IsPermanent(err) {
		class = "permanent"
	}
	telemetry.RecordErrorAs(span, err, class, "delivery failed")
	d.finish(ctx, f, log)
}

// finish records the end of an attempt, fenced by its lease.
func (d *Dispatcher) finish(ctx context.Context, f store.DeliveryFinish, log *slog.Logger) {
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	held, err := d.store.FinishDelivery(wctx, f, d.opts.Holder)
	switch {
	case err != nil:
		// The delivery stays in flight under this holder's lease and is
		// sent again once the lease expires.
		log.ErrorContext(wctx, "record delivery attempt", "error", err)
	case !held:
		log.WarnContext(wctx, "lease lost before the attempt was recorded; leaving the delivery to its new holder")
	default:
		if d.opts.Metrics != nil {
			outcome := f.Status
			if outcome == store.DeliveryPending {
				outcome = OutcomeRetry
			}
			d.opts.Metrics.AttemptFinished(outcome)
		}
	}
}

// safeSend is send with a panic in a sink or renderer turned into a
// permanent error, so that one poison delivery cannot crash the process
// again and again.
func (d *Dispatcher) safeSend(ctx context.Context, ld store.LeasedDelivery, log *slog.Logger) (res sink.Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.ErrorContext(ctx, "sink panicked", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			res, err = sink.Result{}, sink.Permanent(fmt.Errorf("sink panicked: %v", r))
		}
	}()
	return d.send(ctx, ld)
}

// send resolves the delivery's subscription, sink, configuration and
// message, and calls the sink once.
func (d *Dispatcher) send(ctx context.Context, ld store.LeasedDelivery) (sink.Result, error) {
	switch {
	case ld.Subscription == nil:
		return sink.Result{}, sink.Permanent(errors.New(ReasonSubscriptionDeleted))
	case ld.Sink == nil:
		return sink.Result{}, sink.Permanent(errors.New(ReasonSinkDeleted))
	}
	s, err := d.sinks.Get(ld.Sink.Kind)
	if err != nil {
		// Retried: during a rolling upgrade an older binary may lease a
		// delivery for a kind only the newer one has.
		return sink.Result{}, fmt.Errorf("dispatch: %w", err)
	}
	cfg, err := d.opts.DecryptConfig(ld.Sink.ConfigEncrypted)
	if err != nil { // a key the operator can still fix: retried
		return sink.Result{}, fmt.Errorf("dispatch: decrypt config of sink %s: %w", ld.Sink.ID, err)
	}
	secrets, err := ResolveSecrets(cfg, d.opts.LookupEnv)
	if err != nil { // the operator fixes it by setting the variable: retried
		return sink.Result{}, err
	}
	ev := sink.ReleaseEvent{JSON: ld.Event.Payload}
	if err := json.Unmarshal(ld.Event.Payload, &ev.Envelope); err != nil {
		return sink.Result{}, sink.Permanent(fmt.Errorf("dispatch: decode event %s: %w", ld.Event.ID, err))
	}
	dctx, cancel := context.WithTimeout(ctx, d.opts.DeliverTimeout)
	defer cancel()
	rendered, err := d.opts.Renderer.Render(dctx, ld.Subscription.Template, ld.Event.Payload)
	if err != nil {
		return sink.Result{}, fmt.Errorf("dispatch: render: %w", err)
	}
	res, err := s.Deliver(dctx, sink.Request{
		Config: sink.Config(cfg), Secrets: secrets, Event: ev, Rendered: rendered,
		IdempotencyKey: sink.IdempotencyKey(ld.Event.ID, ld.Delivery.SubscriptionID),
		DeliveryID:     ld.Delivery.ID, SubscriptionID: ld.Delivery.SubscriptionID, SinkID: ld.Sink.ID, Attempt: ld.Delivery.Attempts,
	})
	if err != nil && errors.Is(dctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("dispatch: deliver timed out after %v: %w", d.opts.DeliverTimeout, err)
	}
	return res, err
}

// truncate cuts s to maxErrorLen bytes at a rune boundary.
func truncate(s string) string {
	if len(s) <= maxErrorLen {
		return s
	}
	cut := maxErrorLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
