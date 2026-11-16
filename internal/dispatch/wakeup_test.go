package dispatch

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/sink"
	"github.com/angelia-dev/angelia/internal/store"
)

// Story #258 (as #224 for the scheduler): an attempt that finishes while the
// pool is not full wakes the poll loop, so a delivery that became due in the
// meantime is started without waiting for the poll timer.
func TestFinishedAttemptWakesPollLoop(t *testing.T) {
	st, _ := newStore(t)
	addEvents(t, st, 1) // E0000
	release := make(chan struct{})
	sent := make(chan string, 4)
	ts := &testSink{fn: func(_ context.Context, req sink.Request) error {
		sent <- req.DeliveryID
		if req.DeliveryID == "DE0000" {
			<-release
		}
		return nil
	}}
	// The fake clock's timer never fires.
	d := newDispatcher(t, st, registry(t, ts), Options{Clock: &fakeClock{now: t0}, Workers: 2})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	if id := <-sent; id != "DE0000" { // the first poll leased one of two slots
		t.Fatalf("sent %s", id)
	}
	addEvent(t, st, "E0001") // due now, while DE0000 is being sent
	close(release)           // DE0000 finishes with the pool not full
	select {
	case id := <-sent:
		if id != "DE0001" {
			t.Fatalf("sent %s, want DE0001", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DE0001 was not sent: the finished attempt did not wake the poll loop")
	}
	cancel()
	must(t, <-done)
}

// Every attempt wakes the loop when it ends, whatever its outcome, and also
// when the pool was not full.
func TestEveryAttemptOutcomeWakes(t *testing.T) {
	fail := errors.New("boom")
	for _, tc := range []struct {
		name   string
		send   error
		finish func(Store) Store
	}{
		{name: "delivered"},
		{name: "retry", send: fail},
		{name: "dead", send: sink.Permanent(fail)},
		{name: "lost lease", finish: func(s Store) Store { return &finishOverride{Store: s, held: false} }},
		{name: "failed write", finish: func(s Store) Store { return &finishOverride{Store: s, err: fail} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, _ := newStore(t)
			addEvents(t, base, 1)
			var st Store = base
			if tc.finish != nil {
				st = tc.finish(base)
			}
			ts := &testSink{fn: func(context.Context, sink.Request) error { return tc.send }}
			d := newDispatcher(t, st, registry(t, ts), Options{Clock: &fakeClock{now: t0}, Workers: 2})
			if n := pollWait(t, d); n != 1 {
				t.Fatalf("poll started %d attempts, want 1", n)
			}
			if len(d.freed) != 1 {
				t.Fatal("the finished attempt left no wake-up")
			}
		})
	}
}

// Story #258: a poll that leases nothing starts no attempt, so nothing wakes
// the loop again before the poll timer; the loop does not spin.
func TestEmptyPollDoesNotWakeTheLoop(t *testing.T) {
	base, _ := newStore(t)
	addEvents(t, base, 1)
	st := &pollSpy{Store: base, polled: make(chan struct{}, 16)}
	// The fake clock's timer never fires.
	d := newDispatcher(t, st, registry(t, &testSink{}), Options{Clock: &fakeClock{now: t0}})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	for _, what := range []string{"the first poll", "a poll woken by the finished attempt"} {
		select {
		case <-st.polled:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s did not happen", what)
		}
	}
	select {
	case <-st.polled:
		t.Fatal("the loop polled again without a finished attempt or the timer")
	case <-time.After(200 * time.Millisecond):
	}
	if len(d.freed) != 0 {
		t.Fatal("a wake-up is pending after a poll that leased nothing")
	}
	cancel()
	must(t, <-done)
	if del := delivery(t, base, "E0000"); del.Status != store.DeliveryDelivered {
		t.Fatalf("DE0000: %s", del.Status)
	}

	// The same for a poll whose store fails: no attempt, no wake-up.
	fs := &failingStore{}
	fd := newDispatcher(t, fs, registry(t, &testSink{}), Options{Clock: &fakeClock{now: t0}})
	if n := fd.poll(t.Context()); n != 0 {
		t.Fatalf("failing poll started %d attempts", n)
	}
	if len(fd.freed) != 0 {
		t.Fatal("a failing poll left a wake-up")
	}
}

// Story #258: when the context is cancelled while a wake-up is ready, select
// may pick either; Run returns at once instead of polling again.
func TestRunReturnsWhenCancelledWithWakeUpReady(t *testing.T) {
	base, _ := newStore(t)
	// select picks at random, so a single run would pass half the time on
	// a Run without the check.
	for range 50 {
		st := &pollSpy{Store: base}
		clock := &timerCounter{fakeClock: fakeClock{now: t0}}
		d := newDispatcher(t, st, registry(t, &testSink{}), Options{Clock: clock})
		d.freed <- struct{}{}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		must(t, d.Run(ctx))
		if n := st.calls.Load(); n != 0 {
			t.Fatalf("%d HasDueDeliveries calls after cancel, want 0", n)
		}
		if n := clock.timers.Load(); n != 1 {
			t.Fatalf("Run went round its loop %d times after cancel, want 1", n)
		}
	}
}

// addEvent writes one event with one delivery "D"+id for s1, due at t0.
func addEvent(t *testing.T, st store.Store, id string) {
	t.Helper()
	must(t, st.AppendEvent(t.Context(), store.Event{ID: id, WatchID: "w1", Type: "dev.angelia.release.published.v1", Payload: payload(id), CreatedAt: t0},
		[]store.Delivery{{ID: "D" + id, SubscriptionID: "s1", Status: store.DeliveryPending, NextAttemptAt: t0}}))
}

// pollSpy counts HasDueDeliveries calls (one per poll with a free worker)
// and signals each on polled, if set.
type pollSpy struct {
	Store
	calls  atomic.Int32
	polled chan struct{}
}

func (s *pollSpy) HasDueDeliveries(ctx context.Context, now time.Time) (bool, error) {
	s.calls.Add(1)
	ok, err := s.Store.HasDueDeliveries(ctx, now)
	if s.polled != nil {
		s.polled <- struct{}{}
	}
	return ok, err
}

// finishOverride records nothing and answers FinishDelivery with held and err.
type finishOverride struct {
	Store
	held bool
	err  error
}

func (f *finishOverride) FinishDelivery(context.Context, store.DeliveryFinish, string) (bool, error) {
	return f.held, f.err
}

// timerCounter counts the poll timers Run starts: one per loop.
type timerCounter struct {
	fakeClock
	timers atomic.Int32
}

func (c *timerCounter) Timer(d time.Duration) (<-chan time.Time, func()) {
	c.timers.Add(1)
	return c.fakeClock.Timer(d)
}
