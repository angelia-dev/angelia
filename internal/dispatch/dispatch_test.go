package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/sink"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/supervisor"
)

func TestRetryCeiling(t *testing.T) {
	want := []time.Duration{
		12 * time.Minute, 24 * time.Minute, 48 * time.Minute, 96 * time.Minute,
		192 * time.Minute, 384 * time.Minute, 12 * time.Hour,
	}
	var total time.Duration
	for i, w := range want {
		got := RetryCeiling(int64(i+1), DefaultRetryBase, DefaultRetryMax)
		if got != w {
			t.Errorf("RetryCeiling(%d) = %v, want %v", i+1, got, w)
		}
		total += got
	}
	// Default 8 attempts over ~24 h: 7 waits whose ceilings add up to 24h36m.
	if total != 24*time.Hour+36*time.Minute {
		t.Errorf("default retry window = %v", total)
	}
	for _, tc := range []struct {
		n         int64
		base, max time.Duration
		want      time.Duration
	}{
		{0, time.Minute, time.Hour, time.Minute},
		{100, time.Minute, time.Hour, time.Hour},
		{3, 0, time.Hour, time.Hour},
		{2, time.Hour, time.Minute, time.Minute},
	} {
		if got := RetryCeiling(tc.n, tc.base, tc.max); got != tc.want {
			t.Errorf("RetryCeiling(%d, %v, %v) = %v, want %v", tc.n, tc.base, tc.max, got, tc.want)
		}
	}
}

func TestRetryDelayFullJitter(t *testing.T) {
	for _, tc := range []struct {
		r    float64
		want time.Duration
	}{
		{0, 0}, {0.5, 12 * time.Minute}, {0.25, 6 * time.Minute}, {-1, 0}, {2, 24 * time.Minute},
	} {
		if got := RetryDelay(2, DefaultRetryBase, DefaultRetryMax, tc.r); got != tc.want {
			t.Errorf("RetryDelay(2, r=%v) = %v, want %v", tc.r, got, tc.want)
		}
	}
}

// Acceptance criterion: the retry schedule, with a fake clock. Every attempt
// fails with a 500; each retry is due at now + r·ceiling(n), nothing is
// sent before that, and the eighth failure is dead.
func TestRetryScheduleWithFakeClock(t *testing.T) {
	st, _ := newStore(t)
	addEvents(t, st, 1)
	clock := &fakeClock{now: t0}
	ts := &testSink{fn: func(context.Context, sink.Request) error { return sink.HTTPStatus(500, 0) }}
	rs := []float64{0.5, 0.1, 0.9, 0.3, 0.7, 0.2, 0.6}
	var calls int
	d := newDispatcher(t, st, registry(t, ts), Options{Clock: clock, Rand: func() float64 {
		r := rs[calls%len(rs)]
		calls++
		return r
	}})

	now := t0
	for n := int64(1); n <= DefaultMaxAttempts; n++ {
		clock.Set(now)
		if got := pollWait(t, d); got != 1 {
			t.Fatalf("attempt %d: poll started %d", n, got)
		}
		del := delivery(t, st, "E0000")
		if del.Attempts != n {
			t.Fatalf("attempt %d: Attempts = %d", n, del.Attempts)
		}
		if n == DefaultMaxAttempts {
			if del.Status != store.DeliveryDead || !strings.HasPrefix(del.LastError, "giving up after 8 attempts: sink: HTTP 500") {
				t.Fatalf("after the last attempt: %+v", del)
			}
			break
		}
		wait := time.Duration(rs[n-1] * float64(RetryCeiling(n, DefaultRetryBase, DefaultRetryMax)))
		if del.Status != store.DeliveryPending || !del.NextAttemptAt.Equal(now.Add(wait).Truncate(time.Millisecond)) {
			t.Fatalf("attempt %d: %+v, want pending until %v", n, del, now.Add(wait))
		}
		if del.LastError != "sink: HTTP 500 Internal Server Error" {
			t.Errorf("attempt %d: LastError = %q", n, del.LastError)
		}
		// Nothing is sent before the retry is due.
		clock.Set(del.NextAttemptAt.Add(-time.Millisecond))
		if got := pollWait(t, d); got != 0 {
			t.Fatalf("attempt %d: retried %d before due", n, got)
		}
		now = del.NextAttemptAt
	}
	if got := len(ts.requests()); got != DefaultMaxAttempts {
		t.Errorf("sink called %d times", got)
	}
	for i, req := range ts.requests() {
		if req.Attempt != int64(i+1) || req.IdempotencyKey != "E0000:s1" {
			t.Errorf("request %d: attempt %d key %q", i, req.Attempt, req.IdempotencyKey)
		}
	}
	// Dead is final.
	clock.Set(now.Add(48 * time.Hour))
	if got := pollWait(t, d); got != 0 {
		t.Errorf("dead delivery leased again")
	}
}

func TestRetryAfterIsHonoured(t *testing.T) {
	st, _ := newStore(t)
	addEvents(t, st, 1)
	clock := &fakeClock{now: t0}
	ts := &testSink{fn: func(context.Context, sink.Request) error { return sink.HTTPStatus(429, 2*time.Hour) }}
	d := newDispatcher(t, st, registry(t, ts), Options{Clock: clock, Rand: func() float64 { return 0.5 }})
	pollWait(t, d)
	if del := delivery(t, st, "E0000"); !del.NextAttemptAt.Equal(t0.Add(2 * time.Hour)) {
		t.Errorf("429 with Retry-After 2h: next attempt %v", del.NextAttemptAt)
	}
}

// Acceptance criterion: permanent errors (4xx except 408 and 429) are dead
// at the first attempt; 408, 429 and 5xx are retried.
func TestPermanentErrorsAreDeadImmediately(t *testing.T) {
	for _, tc := range []struct {
		err      error
		wantDead bool
	}{
		{sink.HTTPStatus(400, 0), true},
		{sink.HTTPStatus(401, 0), true},
		{sink.HTTPStatus(404, 0), true},
		{sink.HTTPStatus(410, 0), true},
		{sink.HTTPStatus(408, 0), false},
		{sink.HTTPStatus(429, 0), false},
		{sink.HTTPStatus(500, 0), false},
		{sink.HTTPStatus(503, 0), false},
		{sink.Permanent(errors.New("invalid sink config")), true},
		{errors.New("connection refused"), false},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			st, _ := newStore(t)
			addEvents(t, st, 1)
			ts := &testSink{fn: func(context.Context, sink.Request) error { return tc.err }}
			d := newDispatcher(t, st, registry(t, ts), Options{Clock: &fakeClock{now: t0}})
			pollWait(t, d)
			del := delivery(t, st, "E0000")
			want := store.DeliveryPending
			if tc.wantDead {
				want = store.DeliveryDead
			}
			if del.Status != want || del.Attempts != 1 || del.LastError != tc.err.Error() {
				t.Errorf("after %v: %+v, want status %s", tc.err, del, want)
			}
		})
	}
}

// Deliveries that cannot be sent at all are dead without calling the sink.
func TestUnsendableDeliveriesAreDead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(ctx context.Context, t *testing.T, st store.Store) Sinks
		mutate func(ld *store.LeasedDelivery)
		want   string
	}{
		{
			name: "subscription deleted",
			setup: func(ctx context.Context, t *testing.T, st store.Store) Sinks {
				must(t, st.DeleteSubscription(ctx, "s1"))
				return nil
			},
			want: ReasonSubscriptionDeleted,
		},
		{name: "sink deleted", mutate: func(ld *store.LeasedDelivery) { ld.Sink = nil }, want: ReasonSinkDeleted},
		{
			name: "template",
			setup: func(ctx context.Context, t *testing.T, st store.Store) Sinks {
				must(t, st.InConfigTx(ctx, func(tx store.ConfigTx) error {
					return tx.UpdateSubscription(ctx, store.Subscription{ID: "s1", SinkID: "k1", Template: "{{ .data.version }}"})
				}))
				return nil
			},
			want: "dispatch: render: dispatch: subscription templates are not supported yet",
		},
		{
			name:   "undecodable event",
			mutate: func(ld *store.LeasedDelivery) { ld.Event.Payload = []byte(`[]`) },
			want:   "dispatch: decode event E0000: json: cannot unmarshal array into Go value of type event.Envelope",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			st, _ := newStore(t)
			addEvents(t, st, 1)
			ts := &testSink{}
			var sinks Sinks = registry(t, ts)
			if tc.setup != nil {
				if s := tc.setup(ctx, t, st); s != nil {
					sinks = s
				}
			}
			d := newDispatcher(t, st, sinks, Options{Clock: &fakeClock{now: t0}})
			leased, err := st.LeaseDueDeliveries(ctx, d.opts.Holder, t0, t0.Add(time.Minute), 1)
			if err != nil || len(leased) != 1 {
				t.Fatalf("lease: %v, %v", leased, err)
			}
			if tc.mutate != nil {
				tc.mutate(&leased[0])
			}
			d.attempt(ctx, leased[0])
			del := delivery(t, st, "E0000")
			if del.Status != store.DeliveryDead || !strings.HasPrefix(del.LastError, tc.want) {
				t.Errorf("got %s %q, want dead %q", del.Status, del.LastError, tc.want)
			}
			if n := len(ts.requests()); n != 0 {
				t.Errorf("sink called %d times", n)
			}
		})
	}
}

func TestDecryptErrorIsRetried(t *testing.T) {
	st, _ := newStore(t)
	addEvents(t, st, 1)
	ts := &testSink{}
	d := newDispatcher(t, st, registry(t, ts), Options{
		Clock:         &fakeClock{now: t0},
		DecryptConfig: func([]byte) ([]byte, error) { return nil, errors.New("no key") },
	})
	pollWait(t, d)
	if del := delivery(t, st, "E0000"); del.Status != store.DeliveryPending || !strings.Contains(del.LastError, "no key") {
		t.Errorf("after a decrypt error: %+v", del)
	}
}

func TestRequestContent(t *testing.T) {
	st, _ := newStore(t)
	addEvents(t, st, 1)
	ts := &testSink{}
	d := newDispatcher(t, st, registry(t, ts), Options{
		Clock:         &fakeClock{now: t0},
		DecryptConfig: func(b []byte) ([]byte, error) { return append([]byte(nil), b...), nil },
	})
	pollWait(t, d)
	reqs := ts.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests: %d", len(reqs))
	}
	r := reqs[0]
	if string(r.Config) != `{"url":"https://example.com/hook"}` || string(r.Event.JSON) != string(payload("E0000")) ||
		r.Event.Envelope.ID != "E0000" || r.Event.Envelope.Subject != "pypi:requests@2.33.0" ||
		string(r.Rendered.Body) != string(payload("E0000")) || r.Rendered.ContentType != sink.CloudEventsContentType ||
		r.Rendered.Templated || sink.CloudEventsContentType != "application/cloudevents+json; charset=utf-8" ||
		r.IdempotencyKey != "E0000:s1" || r.DeliveryID != "DE0000" || r.SubscriptionID != "s1" || r.Attempt != 1 {
		t.Errorf("request = %+v", r)
	}
	if del := delivery(t, st, "E0000"); del.Status != store.DeliveryDelivered || del.LastError != "" {
		t.Errorf("delivery = %+v", del)
	}
}

func TestDeliverTimeout(t *testing.T) {
	st, _ := newStore(t)
	addEvents(t, st, 1)
	ts := &testSink{fn: func(ctx context.Context, _ sink.Request) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	d := newDispatcher(t, st, registry(t, ts), Options{Clock: &fakeClock{now: t0}, DeliverTimeout: 10 * time.Millisecond})
	pollWait(t, d)
	if del := delivery(t, st, "E0000"); del.Status != store.DeliveryPending ||
		del.LastError != "dispatch: deliver timed out after 10ms: context deadline exceeded" {
		t.Errorf("after a timeout: %+v", del)
	}
}

// Acceptance criterion: concurrent dispatchers never send one delivery in
// parallel. Four dispatchers, each with its own connection pool on one
// database file (as four processes would be), drain 200 deliveries; the
// sink checks that no idempotency key is ever being sent twice at once.
//
// The test stops the dispatchers only once every outcome is recorded: the sink
// returning is not enough, as the dispatcher records the outcome afterwards and
// cancelling in between would leave that delivery in_flight. The pools also
// poll gently: four SQLite pools leasing every 2 ms starve FinishDelivery of the
// single write lock until it exceeds writeTimeout, and the outcome is lost.
func TestConcurrentDispatchersNeverOverlap(t *testing.T) {
	st, path := newStore(t)
	const n = 200
	addEvents(t, st, n)
	ts := &testSink{fn: func(context.Context, sink.Request) error {
		time.Sleep(time.Millisecond)
		return nil
	}}
	sinks := registry(t, ts)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 4)
	var finished atomic.Int32
	allFinished := make(chan struct{})
	for i := range 4 {
		d := newDispatcher(t, &finishCounter{Store: openStore(t, path), count: &finished, want: n, done: allFinished}, sinks, Options{
			Holder: fmt.Sprintf("d%d", i), Workers: 2, PollInterval: 20 * time.Millisecond,
		})
		go func() { done <- d.Run(ctx) }()
	}
	select {
	case <-allFinished:
	case <-time.After(30 * time.Second):
		t.Error("timed out waiting for every outcome to be recorded")
	}
	cancel()
	for range 4 {
		must(t, <-done)
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if len(ts.parallel) != 0 {
		t.Errorf("sent in parallel: %v", ts.parallel)
	}
	seen := map[string]int{}
	for _, r := range ts.reqs {
		seen[r.IdempotencyKey]++
	}
	if len(seen) != n {
		t.Errorf("%d distinct deliveries sent, want %d", len(seen), n)
	}
	for k, c := range seen {
		if c != 1 {
			t.Errorf("%s sent %d times without any failure", k, c)
		}
	}
	for i := range n {
		if del := delivery(t, st, fmt.Sprintf("E%04d", i)); del.Status != store.DeliveryDelivered {
			t.Errorf("%s: %s", del.ID, del.Status)
		}
	}
}

// While one dispatcher is sending a delivery, another sees nothing due,
// even with no time passing between them.
func TestLeasedDeliveryIsNotSentTwice(t *testing.T) {
	st, path := newStore(t)
	addEvents(t, st, 1)
	clock := &fakeClock{now: t0}
	entered, release := make(chan struct{}), make(chan struct{})
	ts := &testSink{fn: func(context.Context, sink.Request) error {
		close(entered)
		<-release
		return nil
	}}
	a := newDispatcher(t, st, registry(t, ts), Options{Clock: clock, Holder: "a"})
	b := newDispatcher(t, openStore(t, path), registry(t, ts), Options{Clock: clock, Holder: "b"})
	if a.poll(t.Context()) != 1 {
		t.Fatal("a leased nothing")
	}
	<-entered
	clock.Set(t0.Add(DefaultLeaseTTL - time.Millisecond))
	if got := pollWait(t, b); got != 0 {
		t.Errorf("b leased %d while a was sending", got)
	}
	close(release)
	a.wg.Wait()
	if del := delivery(t, st, "E0000"); del.Status != store.DeliveryDelivered || del.Attempts != 1 {
		t.Errorf("delivery = %+v", del)
	}
}

// Acceptance criterion: a crash during delivery leads to a redelivery after
// the lease expires (a duplicate is allowed, a loss is not). Dispatcher a
// "crashes" right after its sink received the delivery: its database
// handle is closed, so it can record nothing, as if the process died.
// Dispatcher b, on its own handle, sends the delivery again once a's lease
// has expired, and not before.
func TestCrashDuringDeliveryRedelivers(t *testing.T) {
	st, path := newStore(t)
	addEvents(t, st, 1)
	clock := &fakeClock{now: t0}
	crashed := openStore(t, path)
	var calls atomic.Int32
	ts := &testSink{fn: func(context.Context, sink.Request) error {
		if calls.Add(1) == 1 {
			_ = crashed.Close() // the process dies after the sink got the request
		}
		return nil
	}}
	a := newDispatcher(t, crashed, registry(t, ts), Options{Clock: clock, Holder: "a"})
	b := newDispatcher(t, st, registry(t, ts), Options{Clock: clock, Holder: "b"})
	if got := pollWait(t, a); got != 1 {
		t.Fatalf("a started %d", got)
	}
	if del := delivery(t, st, "E0000"); del.Status != store.DeliveryInFlight {
		t.Fatalf("after the crash: %+v, want in flight", del)
	}
	clock.Set(t0.Add(DefaultLeaseTTL - time.Millisecond))
	if got := pollWait(t, b); got != 0 {
		t.Fatalf("b redelivered %d before the lease expired", got)
	}
	clock.Set(t0.Add(DefaultLeaseTTL))
	if got := pollWait(t, b); got != 1 {
		t.Fatalf("b redelivered %d after the lease expired", got)
	}
	del := delivery(t, st, "E0000")
	if del.Status != store.DeliveryDelivered || del.Attempts != 2 {
		t.Errorf("after redelivery: %+v", del)
	}
	reqs := ts.requests()
	if len(reqs) != 2 || reqs[0].IdempotencyKey != reqs[1].IdempotencyKey || reqs[1].Attempt != 2 {
		t.Errorf("requests = %+v, want the same key twice", reqs)
	}
}

// The crashed holder's late write cannot undo the new holder's work.
func TestLateFinishIsFenced(t *testing.T) {
	st, _ := newStore(t)
	addEvents(t, st, 1)
	ctx := t.Context()
	if _, err := st.LeaseDueDeliveries(ctx, "stalled", t0, t0.Add(DefaultLeaseTTL), 1); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: t0.Add(DefaultLeaseTTL)}
	d := newDispatcher(t, st, registry(t, &testSink{}), Options{Clock: clock, Holder: "b"})
	pollWait(t, d)
	ok, err := st.FinishDelivery(ctx, store.DeliveryFinish{ID: "DE0000", Status: store.DeliveryDead, LastError: "late"}, "stalled")
	if err != nil || ok {
		t.Errorf("late finish = %v, %v", ok, err)
	}
	if del := delivery(t, st, "E0000"); del.Status != store.DeliveryDelivered {
		t.Errorf("delivery = %+v", del)
	}
}

// Held deliveries (ADR 0014) are pending rows due at the end of the hold:
// the dispatcher sends them then, with no special case.
func TestHeldDeliveryIsSentWhenTheHoldEnds(t *testing.T) {
	st, _ := newStore(t)
	until := t0.Add(72 * time.Hour)
	must(t, st.AppendEvent(t.Context(), store.Event{ID: "E0000", WatchID: "w1", Type: "t", Payload: payload("E0000"), CreatedAt: t0},
		[]store.Delivery{{ID: "DE0000", SubscriptionID: "s1", Status: store.DeliveryPending, Hold: &store.Hold{Until: until, StartedAt: t0}}}))
	clock := &fakeClock{now: until.Add(-time.Second)}
	d := newDispatcher(t, st, registry(t, &testSink{}), Options{Clock: clock})
	if got := pollWait(t, d); got != 0 {
		t.Fatalf("held delivery sent early")
	}
	clock.Set(until)
	if got := pollWait(t, d); got != 1 {
		t.Fatalf("held delivery not sent at the end of the hold")
	}
}

func TestWorkersBoundLeasing(t *testing.T) {
	st, _ := newStore(t)
	addEvents(t, st, 5)
	release := make(chan struct{})
	ts := &testSink{fn: func(context.Context, sink.Request) error { <-release; return nil }}
	d := newDispatcher(t, st, registry(t, ts), Options{Clock: &fakeClock{now: t0}, Workers: 2})
	if got := d.poll(t.Context()); got != 2 {
		t.Errorf("first poll started %d, want 2", got)
	}
	if got := d.poll(t.Context()); got != 0 {
		t.Errorf("poll with no free worker started %d", got)
	}
	close(release)
	d.wg.Wait()
	if got := pollWait(t, d); got != 2 {
		t.Errorf("poll after the workers freed started %d", got)
	}
}

// finishCounter counts the outcomes recorded through any wrapper sharing count
// and closes done, once, when want of them have been recorded.
type finishCounter struct {
	Store
	count *atomic.Int32
	want  int32
	done  chan struct{}
}

func (f *finishCounter) FinishDelivery(ctx context.Context, fin store.DeliveryFinish, holder string) (bool, error) {
	ok, err := f.Store.FinishDelivery(ctx, fin, holder)
	if err == nil && ok && f.count.Add(1) == f.want {
		close(f.done)
	}
	return ok, err
}

// failingStore fails every call.
type failingStore struct{ calls atomic.Int32 }

func (f *failingStore) LeaseDueDeliveries(context.Context, string, time.Time, time.Time, int) ([]store.LeasedDelivery, error) {
	f.calls.Add(1)
	return nil, errors.New("disk I/O error")
}

func (f *failingStore) HasDueDeliveries(context.Context, time.Time) (bool, error) {
	f.calls.Add(1)
	return true, nil
}

func (f *failingStore) FinishDelivery(context.Context, store.DeliveryFinish, string) (bool, error) {
	return false, errors.New("disk I/O error")
}

func TestStoreFailuresDoNotStopPolling(t *testing.T) {
	fs := &failingStore{}
	clock := &fakeClock{now: t0}
	d := newDispatcher(t, fs, sink.NewRegistry(), Options{Clock: clock})
	for i := range 3 {
		clock.Set(t0.Add(time.Duration(i) * 30 * time.Second))
		if got := d.poll(t.Context()); got != 0 {
			t.Errorf("poll started %d", got)
		}
	}
	if d.failingSince != t0 || d.lastErrLog != t0.Add(time.Minute) {
		t.Errorf("failingSince %v lastErrLog %v", d.failingSince, d.lastErrLog)
	}
	d.storeOK(t.Context())
	if !d.failingSince.IsZero() {
		t.Error("storeOK did not reset")
	}
	// A failed finish leaves the delivery to lease expiry; it must not panic.
	d.attempt(t.Context(), store.LeasedDelivery{Delivery: store.Delivery{ID: "x"}})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if d.poll(ctx) != 0 || fs.calls.Load() != 6 {
		t.Errorf("poll after cancel touched the store: %d calls", fs.calls.Load())
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	st, _ := newStore(t)
	d := newDispatcher(t, st, registry(t, &testSink{}), Options{PollInterval: time.Millisecond})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() { done <- d.Run(ctx) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run = %v", err)
	}
	if d.Name() != "dispatcher" {
		t.Errorf("Name = %q", d.Name())
	}
}

func TestNewOptions(t *testing.T) {
	st, _ := newStore(t)
	sinks := sink.NewRegistry()
	if _, err := New(nil, sinks, Options{}); err == nil {
		t.Error("nil store accepted")
	}
	if _, err := New(st, nil, Options{}); err == nil {
		t.Error("nil sinks accepted")
	}
	for _, o := range []Options{
		{Workers: -1},
		{MaxAttempts: -1},
		{RetryBase: -1},
		{LeaseTTL: -1},
		{DeliverTimeout: time.Minute, LeaseTTL: time.Minute},
		{DeliverTimeout: 91 * time.Second},
		{DeliverTimeout: 91 * time.Minute, LeaseTTL: 2 * time.Hour},
		{LeaseTTL: 20 * time.Second},
	} {
		if _, err := New(st, sinks, o); err == nil {
			t.Errorf("New(%+v) accepted", o)
		}
	}
	d, err := New(st, sinks, Options{})
	must(t, err)
	o := d.opts
	if o.Workers != DefaultWorkers || o.MaxAttempts != DefaultMaxAttempts || o.RetryBase != DefaultRetryBase ||
		o.RetryMax != DefaultRetryMax || o.LeaseTTL != DefaultLeaseTTL || o.DeliverTimeout != DefaultDeliverTimeout ||
		o.PollInterval != DefaultPollInterval || o.Holder == "" {
		t.Errorf("defaults = %+v", o)
	}
	if d, _ := New(st, sinks, Options{LeaseTTL: 40 * time.Second}); d.opts.DeliverTimeout != 10*time.Second {
		t.Errorf("deliver timeout under a short lease = %v", d.opts.DeliverTimeout)
	}
	if _, err := New(st, sinks, Options{DeliverTimeout: 90 * time.Second}); err != nil {
		t.Errorf("deliver timeout at the bound: %v", err)
	}
	if DefaultDeliverTimeout+writeTimeout >= supervisor.DefaultDrainTimeout {
		t.Errorf("deliver %v + write %v not below the drain timeout %v", DefaultDeliverTimeout, writeTimeout, supervisor.DefaultDrainTimeout)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short"); got != "short" {
		t.Errorf("truncate(short) = %q", got)
	}
	long := strings.Repeat("a", maxErrorLen-1) + "é" + "tail"
	got := truncate(long)
	if got != strings.Repeat("a", maxErrorLen-1)+"…" {
		t.Errorf("truncate cut inside a rune: %q", got[len(got)-8:])
	}
}
