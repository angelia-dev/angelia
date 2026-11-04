package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/sink"
	"github.com/angelia-dev/angelia/internal/store"
)

// replay replays a delivery as POST /v1/deliveries/{id}/replay does.
func replay(t *testing.T, st interface {
	InAPITx(context.Context, func(store.APITx) error) error
}, id string, now time.Time,
) {
	t.Helper()
	must(t, st.InAPITx(t.Context(), func(tx store.APITx) error { return tx.ReplayDelivery(t.Context(), id, now) }))
}

// A replay starts a new retry budget (spec docs/api.md decision 28): a
// delivery dead after its last attempt is sent again, retried on failure,
// and dead again only once the new budget is spent. Attempts keeps counting.
func TestReplayRestartsRetryBudget(t *testing.T) {
	st, _ := newStore(t)
	addEvents(t, st, 1)
	clock := &fakeClock{now: t0}
	ts := &testSink{fn: func(context.Context, sink.Request) error { return errors.New("503 from upstream") }}
	d := newDispatcher(t, st, registry(t, ts), Options{Clock: clock, MaxAttempts: 2, RetryBase: time.Second, RetryMax: time.Second, Rand: func() float64 { return 0 }})

	run := func(n int) {
		t.Helper()
		for range n {
			if got := pollWait(t, d); got != 1 {
				t.Fatalf("poll started %d", got)
			}
			clock.Set(clock.Now().Add(time.Second))
		}
	}
	run(2)
	del := delivery(t, st, "E0000")
	if del.Status != store.DeliveryDead || del.Attempts != 2 {
		t.Fatalf("before the replay: %+v", del)
	}
	replay(t, st, del.ID, clock.Now())
	if del = delivery(t, st, "E0000"); del.ReplayBaseAttempts != 2 || del.Attempts != 2 || del.Status != store.DeliveryPending {
		t.Fatalf("after the replay: %+v", del)
	}
	run(1)
	if del = delivery(t, st, "E0000"); del.Status != store.DeliveryPending || del.Attempts != 3 {
		t.Fatalf("first attempt of the replay is retried: %+v", del)
	}
	run(1)
	if del = delivery(t, st, "E0000"); del.Status != store.DeliveryDead || del.Attempts != 4 || del.LastError != "giving up after 2 attempts: 503 from upstream" {
		t.Fatalf("the new budget is spent: %+v", del)
	}
	if n := len(ts.requests()); n != 4 {
		t.Errorf("%d sends, want 4", n)
	}
}

// The poison guard counts from the replay too: an attempt of the replay that
// never finished still ends the delivery once the new budget is spent, and
// not before.
func TestReplayPoisonGuard(t *testing.T) {
	st, path := newStore(t)
	addEvents(t, st, 1)
	clock := &fakeClock{now: t0}
	ts := &testSink{}
	d := newDispatcher(t, st, registry(t, ts), Options{Clock: clock, MaxAttempts: 1})
	ts.fn = func(context.Context, sink.Request) error { return errors.New("down") }
	pollWait(t, d)
	replay(t, st, "DE0000", clock.Now())

	// The first attempt of the replay crashes: its holder never records it.
	crashed := openStore(t, path)
	ts.fn = func(context.Context, sink.Request) error { _ = crashed.Close(); return nil }
	pollWait(t, newDispatcher(t, crashed, registry(t, ts), Options{Clock: clock, MaxAttempts: 1, Holder: "crashed"}))
	clock.Set(clock.Now().Add(DefaultLeaseTTL))
	ts.fn = nil
	pollWait(t, d)
	del := delivery(t, st, "E0000")
	if del.Status != store.DeliveryDead || del.Attempts != 3 || del.LastError != "giving up after 1 attempts (previous attempt did not finish)" {
		t.Fatalf("%+v", del)
	}
	if n := len(ts.requests()); n != 2 {
		t.Errorf("%d sends, want 2", n)
	}
}
