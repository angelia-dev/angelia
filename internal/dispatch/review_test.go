package dispatch

import (
	"context"
	"errors"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/sink"
	"github.com/angelia-dev/angelia/internal/store"
)

// An unregistered sink kind is retried, so that a rolling upgrade (an old
// binary leasing a delivery for a new kind) loses nothing.
func TestUnknownSinkKindIsRetried(t *testing.T) {
	st, _ := newStore(t)
	addEvents(t, st, 1)
	d := newDispatcher(t, st, sink.NewRegistry(), Options{Clock: &fakeClock{now: t0}})
	pollWait(t, d)
	del := delivery(t, st, "E0000")
	if del.Status != store.DeliveryPending || del.LastError != `dispatch: get sink "test": sink: unknown kind` {
		t.Errorf("unknown kind: %+v", del)
	}
}

// A sink that panics does not take the process down: the delivery is dead
// with the panic as its error.
func TestSinkPanicIsPermanent(t *testing.T) {
	st, _ := newStore(t)
	addEvents(t, st, 1)
	ts := &testSink{fn: func(context.Context, sink.Request) error { panic("boom") }}
	d := newDispatcher(t, st, registry(t, ts), Options{Clock: &fakeClock{now: t0}})
	pollWait(t, d)
	if del := delivery(t, st, "E0000"); del.Status != store.DeliveryDead || del.LastError != "sink panicked: boom" {
		t.Errorf("after a panic: %+v", del)
	}
}

// A delivery whose last attempt never finished (it crashed its dispatcher)
// is dead at the next lease, without calling the sink again.
func TestUnfinishedLastAttemptIsDeadWithoutSending(t *testing.T) {
	st, _ := newStore(t)
	addEvents(t, st, 1)
	ctx := t.Context()
	at := t0
	for range 2 { // two attempts leased by holders that crashed
		if _, err := st.LeaseDueDeliveries(ctx, "crashed", at, at.Add(DefaultLeaseTTL), 1); err != nil {
			t.Fatal(err)
		}
		at = at.Add(DefaultLeaseTTL)
	}
	ts := &testSink{}
	d := newDispatcher(t, st, registry(t, ts), Options{Clock: &fakeClock{now: at}, MaxAttempts: 2})
	if got := pollWait(t, d); got != 1 {
		t.Fatalf("poll started %d", got)
	}
	del := delivery(t, st, "E0000")
	if del.Status != store.DeliveryDead || del.Attempts != 3 ||
		del.LastError != "giving up after 2 attempts (previous attempt did not finish)" {
		t.Errorf("delivery = %+v", del)
	}
	if n := len(ts.requests()); n != 0 {
		t.Errorf("sink called %d times", n)
	}
}

// The same dispatcher's sink outlives the lease. The delivery is leased
// again by the same holder but not started twice, and the stale attempt
// cannot record its result over the newer lease.
func TestSameHolderStaleAttempt(t *testing.T) {
	st, _ := newStore(t)
	addEvents(t, st, 1)
	clock := &fakeClock{now: t0}
	entered, release := make(chan struct{}, 4), make(chan struct{})
	var calls atomic.Int32
	ts := &testSink{fn: func(context.Context, sink.Request) error {
		entered <- struct{}{}
		if calls.Add(1) == 1 {
			<-release // ignores ctx: outlives DeliverTimeout and the lease
		}
		return nil
	}}
	d := newDispatcher(t, st, registry(t, ts), Options{Clock: clock, Holder: "a"})
	if d.poll(t.Context()) != 1 {
		t.Fatal("first poll started nothing")
	}
	<-entered
	clock.Set(t0.Add(DefaultLeaseTTL))
	if got := d.poll(t.Context()); got != 0 {
		t.Fatalf("delivery started twice by one holder: %d", got)
	}
	close(release)
	d.wg.Wait()
	// Attempt 1's result was fenced off: the row is still at attempt 2.
	if del := delivery(t, st, "E0000"); del.Status != store.DeliveryInFlight || del.Attempts != 2 {
		t.Fatalf("after the stale attempt: %+v", del)
	}
	clock.Set(t0.Add(2 * DefaultLeaseTTL))
	if got := pollWait(t, d); got != 1 {
		t.Fatalf("redelivery after the second lease expired: %d", got)
	}
	if del := delivery(t, st, "E0000"); del.Status != store.DeliveryDelivered || del.Attempts != 3 {
		t.Errorf("final: %+v", del)
	}
	if got := ts.requests(); len(got) != 2 {
		t.Errorf("sink called %d times, want 2", len(got))
	}
}

// Shutdown drains: an attempt in progress when Run is cancelled finishes,
// with an uncancelled context, and is recorded before Run returns.
func TestRunDrainsAttemptsOnCancel(t *testing.T) {
	st, _ := newStore(t)
	addEvents(t, st, 1)
	entered, release := make(chan struct{}), make(chan struct{})
	ts := &testSink{fn: func(ctx context.Context, _ sink.Request) error {
		close(entered)
		<-release
		return ctx.Err()
	}}
	d := newDispatcher(t, st, registry(t, ts), Options{Clock: &fakeClock{now: t0}})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() { done <- d.Run(ctx) }()
	<-entered
	cancel()
	select {
	case err := <-done:
		t.Fatalf("Run returned (%v) before the attempt finished", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	must(t, <-done)
	if del := delivery(t, st, "E0000"); del.Status != store.DeliveryDelivered {
		t.Errorf("after drain: %+v", del)
	}
}

// URLs in sink errors lose their path and query before they are stored:
// chat webhook URLs carry their secret there.
func TestLastErrorRedactsURLs(t *testing.T) {
	st, _ := newStore(t)
	addEvents(t, st, 1)
	const hook = "https://hooks.chat.example/services/team/channel/not-a-real-token" //nolint:gosec // G101: synthetic placeholder URL
	ts := &testSink{fn: func(context.Context, sink.Request) error {
		return &url.Error{Op: "Post", URL: hook, Err: errors.New("connection refused")}
	}}
	d := newDispatcher(t, st, registry(t, ts), Options{Clock: &fakeClock{now: t0}})
	pollWait(t, d)
	if del := delivery(t, st, "E0000"); del.LastError != `Post "https://hooks.chat.example/[redacted]": connection refused` {
		t.Errorf("last_error = %q", del.LastError)
	}
}

func TestDefaultRendererClonesBody(t *testing.T) {
	in := []byte(`{"id":"x"}`)
	r, err := DefaultRenderer{}.Render(t.Context(), "", in)
	must(t, err)
	r.Body[0] = 'X'
	if in[0] != '{' || r.Templated || r.ContentType != sink.CloudEventsContentType {
		t.Errorf("render shares the envelope or claims a template: %q %+v", in, r)
	}
}
