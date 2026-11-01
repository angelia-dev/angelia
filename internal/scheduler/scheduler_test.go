package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// AC: a target is never fetched by two workers at the same time, with
// several schedulers (holders) of several workers each racing on one store.
func TestNoTargetCheckedTwiceConcurrently(t *testing.T) {
	const schedulers, workers, targets, rounds = 4, 3, 40, 3
	st := openStore(t)
	clk := newFakeClock(t0)
	for i := range targets {
		addTarget(t, st, fmt.Sprintf("t%02d", i), fmt.Sprint("pkg", i), t0)
	}
	c := newCounter()
	c.hold = runtime.Gosched // widen the window for overlap
	var ss []*Scheduler
	for i := range schedulers {
		ss = append(ss, newTestScheduler(t, st, c, clk, func(o *Options) {
			o.Workers = workers
			o.Holder = fmt.Sprint("holder-", i)
			o.Jitter = 0.1
			o.Rand = lockedRand(0, 0.3, 0.7, 0.99)
		}))
	}
	ctx := t.Context()
	for round := 1; round <= rounds; round++ {
		var wg sync.WaitGroup
		for _, s := range ss {
			wg.Go(func() {
				for c.total() < round*targets {
					s.poll(ctx)
					runtime.Gosched()
				}
				s.wg.Wait()
			})
		}
		wg.Wait()
		for i := range targets {
			if n := c.count(fmt.Sprintf("t%02d", i)); n != round {
				t.Fatalf("round %d: t%02d checked %d times, want %d", round, i, n, round)
			}
		}
		clk.Advance(2 * time.Hour) // past interval + max jitter
	}
	if c.maxActive != 1 {
		t.Fatalf("a target was checked by %d workers at once", c.maxActive)
	}
}

// AC: while one holder's check runs, no other holder fetches the target.
func TestLeasedTargetIsSkipped(t *testing.T) {
	st := openStore(t)
	clk := newFakeClock(t0)
	for i := range 5 {
		addTarget(t, st, fmt.Sprint("t", i), fmt.Sprint("pkg", i), t0)
	}
	gate := make(chan struct{})
	c := newCounter()
	c.hold = func() { <-gate }
	a := newTestScheduler(t, st, c, clk, func(o *Options) { o.Holder, o.Workers = "a", 10 })
	b := newTestScheduler(t, st, c, clk, func(o *Options) { o.Holder, o.Workers = "b", 10 })
	ctx := t.Context()

	if n := a.poll(ctx); n != 5 {
		t.Fatalf("a started %d checks, want 5", n)
	}
	if n := b.poll(ctx); n != 0 {
		t.Fatalf("b started %d checks while a holds the leases", n)
	}
	close(gate)
	a.wg.Wait()
	if n := pollAndWait(ctx, b); n != 0 {
		t.Fatalf("b started %d checks of targets a just checked", n)
	}
	if c.total() != 5 {
		t.Fatalf("total checks = %d, want 5", c.total())
	}
}

// AC: the pool never runs more than Workers checks, and a freed slot
// wakes the poll loop when the pool was full.
func TestWorkerPoolBound(t *testing.T) {
	st := openStore(t)
	clk := newFakeClock(t0)
	for i := range 5 {
		addTarget(t, st, fmt.Sprint("t", i), fmt.Sprint("pkg", i), t0)
	}
	gate := make(chan struct{})
	started := make(chan struct{}, 5)
	var mu sync.Mutex
	running, peak := 0, 0
	c := CheckerFunc(func(context.Context, store.Target) (Outcome, error) {
		mu.Lock()
		running++
		peak = max(peak, running)
		mu.Unlock()
		started <- struct{}{}
		<-gate
		mu.Lock()
		running--
		mu.Unlock()
		return Outcome{}, nil
	})
	s := newTestScheduler(t, st, c, clk, func(o *Options) { o.Workers = 2 })
	ctx := t.Context()
	if n := s.poll(ctx); n != 2 {
		t.Fatalf("first poll started %d, want 2", n)
	}
	<-started
	<-started
	if n := s.poll(ctx); n != 0 {
		t.Fatalf("poll with a full pool started %d", n)
	}
	gate <- struct{}{}
	<-s.freed // the pool was full, so the finished check signals
	if n := s.poll(ctx); n != 1 {
		t.Fatalf("poll after one finished started %d, want 1", n)
	}
	close(gate)
	s.wg.Wait()
	total := 3
	for n := pollAndWait(ctx, s); n > 0; n = pollAndWait(ctx, s) {
		total += n
	}
	if total != 5 || peak != 2 {
		t.Fatalf("checks = %d, peak concurrency = %d; want 5 and 2", total, peak)
	}
}

// AC: an expired lease (crashed worker) is picked up after the lease TTL.
func TestExpiredLeaseIsTakenOver(t *testing.T) {
	st := openStore(t)
	clk := newFakeClock(t0)
	addTarget(t, st, "t1", "requests", t0)
	const ttl = 5 * time.Minute
	ctx := t.Context()
	// A worker leases the target and crashes without releasing it.
	got, err := st.LeaseDueTargets(ctx, "crashed", t0, t0.Add(ttl), 10)
	must(t, err)
	if len(got) != 1 {
		t.Fatalf("crashed holder leased %d targets", len(got))
	}
	c := newCounter()
	s := newTestScheduler(t, st, c, clk, func(o *Options) { o.LeaseTTL = ttl })

	if n := pollAndWait(ctx, s); n != 0 {
		t.Fatalf("leased target checked before expiry")
	}
	clk.Advance(ttl - time.Millisecond)
	if n := pollAndWait(ctx, s); n != 0 {
		t.Fatalf("leased target checked 1ms before expiry")
	}
	clk.Advance(time.Millisecond)
	if n := pollAndWait(ctx, s); n != 1 {
		t.Fatalf("expired lease not taken over")
	}
	if c.count("t1") != 1 {
		t.Fatalf("checks = %d", c.count("t1"))
	}
	tg := getTarget(t, st, "t1")
	if want := clk.Now().Add(DefaultInterval); !tg.NextCheckAt.Equal(want) {
		t.Errorf("next check = %v, want %v", tg.NextCheckAt, want)
	}
	// The scheduler released its lease after the check.
	if ok, err := st.AcquireLease(ctx, store.TargetLeaseName("t1"), "other", clk.Now(), clk.Now().Add(ttl)); err != nil || !ok {
		t.Fatalf("lease not released: %v %v", ok, err)
	}
}

// AC: two watches on pypi:requests share one target and cause one fetch
// per interval.
func TestWatchesOnOneTargetShareOneFetch(t *testing.T) {
	st := openStore(t)
	clk := newFakeClock(t0)
	ctx := t.Context()
	addTarget(t, st, "t-requests", "requests", t0)
	must(t, st.CreateWatch(ctx, store.Watch{ID: "w1", TargetID: "t-requests"}))
	must(t, st.CreateWatch(ctx, store.Watch{ID: "w2", TargetID: "t-requests", Labels: map[string]string{"team": "b"}}))
	// The store keeps one target per upstream object.
	err := st.CreateTarget(ctx, store.Target{
		ID: "t-dup", Kind: "pypi", Coordinates: map[string]string{"name": "requests"}, NextCheckAt: t0,
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second target for pypi:requests: %v, want ErrConflict", err)
	}

	c := newCounter()
	s := newTestScheduler(t, st, c, clk, func(o *Options) { o.Workers = 8 })
	// Three hours, polling every minute.
	for range 180 {
		pollAndWait(ctx, s)
		clk.Advance(time.Minute)
	}
	if n := c.count("t-requests"); n != 3 {
		t.Fatalf("fetches over 3 intervals = %d, want 3", n)
	}
	if c.total() != 3 {
		t.Fatalf("checks of other targets: %v", c.counts)
	}
}

// AC: the backoff sequence (exponential, capped at 24h) and its reset on
// success, with a fake clock.
func TestBackoffSequenceAndReset(t *testing.T) {
	st := openStore(t)
	clk := newFakeClock(t0)
	addTarget(t, st, "t1", "requests", t0)
	fail := 13
	c := CheckerFunc(func(context.Context, store.Target) (Outcome, error) {
		if fail > 0 {
			fail--
			return Outcome{FetchState: json.RawMessage(`{"etag":"ignored"}`)}, errors.New("upstream 503")
		}
		return Outcome{FetchState: json.RawMessage(`{"etag":"e1"}`)}, nil
	})
	s := newTestScheduler(t, st, c, clk, nil)
	ctx := t.Context()
	wantDelays := []time.Duration{
		time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute,
		32 * time.Minute, 64 * time.Minute, 128 * time.Minute, 256 * time.Minute, 512 * time.Minute,
		1024 * time.Minute, 24 * time.Hour, 24 * time.Hour,
	}
	for i, want := range wantDelays {
		if n := pollAndWait(ctx, s); n != 1 {
			t.Fatalf("attempt %d not run", i+1)
		}
		tg := getTarget(t, st, "t1")
		if tg.ErrorCount != int64(i+1) {
			t.Fatalf("attempt %d: error count %d", i+1, tg.ErrorCount)
		}
		if got := tg.NextCheckAt.Sub(clk.Now()); got != want {
			t.Fatalf("attempt %d: delay %v, want %v", i+1, got, want)
		}
		if string(tg.FetchState) != `{"etag":"e0"}` || !tg.LastCheckedAt.IsZero() {
			t.Fatalf("attempt %d: failure changed fetch state or last check: %+v", i+1, tg)
		}
		// Not due again before the delay has passed.
		clk.Advance(want - time.Millisecond)
		if n := pollAndWait(ctx, s); n != 0 {
			t.Fatalf("attempt %d: rechecked before backoff elapsed", i+1)
		}
		clk.Advance(time.Millisecond)
	}
	// Success resets the error count and returns to the normal interval.
	pollAndWait(ctx, s)
	tg := getTarget(t, st, "t1")
	if tg.ErrorCount != 0 || !tg.LastCheckedAt.Equal(clk.Now()) || !tg.NextCheckAt.Equal(clk.Now().Add(time.Hour)) {
		t.Fatalf("after success: %+v", tg)
	}
	if string(tg.FetchState) != `{"etag":"e1"}` {
		t.Fatalf("fetch state = %s", tg.FetchState)
	}
	// A later failure starts the sequence again from the base delay.
	fail = 1
	clk.Advance(time.Hour)
	pollAndWait(ctx, s)
	tg = getTarget(t, st, "t1")
	if tg.ErrorCount != 1 || tg.NextCheckAt.Sub(clk.Now()) != time.Minute {
		t.Fatalf("after reset and new failure: %+v", tg)
	}
}

// Jitter spreads both the interval and the backoff by ±10% by default,
// and the backoff never exceeds its cap.
func TestJitterApplied(t *testing.T) {
	tests := []struct {
		name  string
		r     float64
		fails int64 // error count already stored
		err   error
		want  time.Duration
	}{
		{"interval low", 0, 0, nil, 54 * time.Minute},
		{"interval high", 0.99999, 0, nil, 66 * time.Minute},
		{"backoff low", 0, 0, errors.New("x"), 54 * time.Second},
		{"backoff at cap stays capped", 0.99999, 20, errors.New("x"), 24 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			clk := newFakeClock(t0)
			ctx := t.Context()
			must(t, st.CreateTarget(ctx, store.Target{
				ID: "t1", Kind: "pypi", Coordinates: map[string]string{"name": "x"}, NextCheckAt: t0, ErrorCount: tc.fails,
			}))
			c := CheckerFunc(func(context.Context, store.Target) (Outcome, error) { return Outcome{}, tc.err })
			s := newTestScheduler(t, st, c, clk, func(o *Options) { o.Jitter = 0; o.Rand = func() float64 { return tc.r } })
			pollAndWait(ctx, s)
			got := getTarget(t, st, "t1").NextCheckAt.Sub(t0)
			if got.Round(time.Second) != tc.want {
				t.Fatalf("delay = %v, want %v", got, tc.want)
			}
		})
	}
}

// Outcome.Interval overrides the default interval; a nil FetchState keeps
// the stored one.
func TestOutcomeInterval(t *testing.T) {
	st := openStore(t)
	clk := newFakeClock(t0)
	addTarget(t, st, "t1", "requests", t0)
	c := CheckerFunc(func(context.Context, store.Target) (Outcome, error) {
		return Outcome{Interval: 10 * time.Minute}, nil
	})
	s := newTestScheduler(t, st, c, clk, nil)
	pollAndWait(t.Context(), s)
	tg := getTarget(t, st, "t1")
	if tg.NextCheckAt.Sub(t0) != 10*time.Minute || string(tg.FetchState) != `{"etag":"e0"}` {
		t.Fatalf("target = %+v", tg)
	}
}

type fakeMetrics struct {
	mu      sync.Mutex
	backlog []int64
	lag     []time.Duration
}

func (m *fakeMetrics) DueBacklog(n int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.backlog = append(m.backlog, n)
}

func (m *fakeMetrics) CheckLag(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lag = append(m.lag, d)
}

// AC: metrics hooks for due backlog and check lag.
func TestMetricsHooks(t *testing.T) {
	st := openStore(t)
	clk := newFakeClock(t0)
	addTarget(t, st, "t1", "a", t0)
	addTarget(t, st, "t2", "b", t0.Add(10*time.Second))
	addTarget(t, st, "t3", "c", t0.Add(time.Hour))
	m := &fakeMetrics{}
	s := newTestScheduler(t, st, newCounter(), clk, func(o *Options) { o.Metrics = m; o.Workers = 1 })
	clk.Advance(30 * time.Second)
	pollAndWait(t.Context(), s)
	pollAndWait(t.Context(), s)
	pollAndWait(t.Context(), s)
	want := []int64{2, 1, 0}
	if fmt.Sprint(m.backlog) != fmt.Sprint(want) {
		t.Errorf("backlog = %v, want %v", m.backlog, want)
	}
	if wantLag := []time.Duration{30 * time.Second, 20 * time.Second}; fmt.Sprint(m.lag) != fmt.Sprint(wantLag) {
		t.Errorf("lag = %v, want %v", m.lag, wantLag)
	}
}

// Run polls on its interval, rechecks a target once it is due again, and
// returns nil on cancellation.
func TestRun(t *testing.T) {
	st := openStore(t)
	clk := newFakeClock(t0)
	addTarget(t, st, "t1", "requests", t0)
	checked := make(chan string, 4)
	c := CheckerFunc(func(_ context.Context, tg store.Target) (Outcome, error) {
		checked <- tg.ID
		return Outcome{}, nil
	})
	s := newTestScheduler(t, st, c, clk, func(o *Options) { o.PollInterval = 10 * time.Second })
	idle, resume := make(chan struct{}), make(chan struct{})
	s.testHookIdle = func() { idle <- struct{}{}; <-resume }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	<-idle // first poll done
	if id := <-checked; id != "t1" {
		t.Fatalf("checked %s", id)
	}
	s.wg.Wait() // Run is parked in the hook, so no poll adds work
	clk.Advance(time.Hour)
	resume <- struct{}{}
	<-idle // the poll timer fired and the target was due again
	if id := <-checked; id != "t1" {
		t.Fatalf("checked %s", id)
	}
	cancel()
	resume <- struct{}{}
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
}

// On shutdown a running check is cancelled, its lease released and the
// target left due, without counting an error.
func TestShutdownReleasesLease(t *testing.T) {
	st := openStore(t)
	clk := newFakeClock(t0)
	addTarget(t, st, "t1", "requests", t0)
	started := make(chan struct{})
	c := CheckerFunc(func(ctx context.Context, _ store.Target) (Outcome, error) {
		close(started)
		<-ctx.Done()
		return Outcome{}, fmt.Errorf("fetch: %w", ctx.Err())
	})
	s := newTestScheduler(t, st, c, clk, nil)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	<-started
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
	tg := getTarget(t, st, "t1")
	if !tg.NextCheckAt.Equal(t0) || tg.ErrorCount != 0 {
		t.Fatalf("target changed by an abandoned check: %+v", tg)
	}
	got, err := st.LeaseDueTargets(t.Context(), "next", t0, t0.Add(time.Minute), 10)
	must(t, err)
	if len(got) != 1 {
		t.Fatalf("lease not released on shutdown")
	}
}

// A check that completes while shutdown starts is still recorded.
func TestShutdownAfterSuccessfulCheckRecordsIt(t *testing.T) {
	st := openStore(t)
	clk := newFakeClock(t0)
	addTarget(t, st, "t1", "requests", t0)
	ctx, cancel := context.WithCancel(t.Context())
	c := CheckerFunc(func(context.Context, store.Target) (Outcome, error) {
		cancel()
		return Outcome{FetchState: json.RawMessage(`{"etag":"e1"}`)}, nil
	})
	s := newTestScheduler(t, st, c, clk, nil)
	pollAndWait(ctx, s)
	tg := getTarget(t, st, "t1")
	if string(tg.FetchState) != `{"etag":"e1"}` || !tg.NextCheckAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("completed check not recorded: %+v", tg)
	}
}

// leaseStore wraps a store and controls lease renewals.
type leaseStore struct {
	Store
	renewOK  bool
	renewals atomic.Int32
}

func (l *leaseStore) AcquireLease(ctx context.Context, name, holder string, now, until time.Time) (bool, error) {
	l.renewals.Add(1)
	if !l.renewOK {
		return false, nil
	}
	return l.Store.AcquireLease(ctx, name, holder, now, until)
}

// A long check renews its lease every TTL/3; if the renewal finds the lease
// taken, the check is cancelled and the target left untouched.
func TestLeaseRenewal(t *testing.T) {
	for _, renewOK := range []bool{true, false} {
		t.Run(fmt.Sprint("renew ok=", renewOK), func(t *testing.T) {
			base := openStore(t)
			clk := newFakeClock(t0)
			addTarget(t, base, "t1", "requests", t0)
			st := &leaseStore{Store: base, renewOK: renewOK}
			finish := make(chan struct{})
			started := make(chan struct{})
			c := CheckerFunc(func(ctx context.Context, _ store.Target) (Outcome, error) {
				close(started)
				select {
				case <-finish:
					return Outcome{}, nil
				case <-ctx.Done():
					return Outcome{}, ctx.Err()
				}
			})
			s := newTestScheduler(t, st, c, clk, func(o *Options) { o.LeaseTTL = 3 * time.Minute })
			s.poll(t.Context())
			<-started
			clk.waitTimers(1) // the renewal timer
			clk.Advance(time.Minute)
			if renewOK {
				clk.waitTimers(1) // renewed, next renewal scheduled
				close(finish)
			}
			s.wg.Wait()
			if st.renewals.Load() < 1 {
				t.Fatal("lease not renewed")
			}
			tg := getTarget(t, base, "t1")
			if renewOK != tg.NextCheckAt.After(t0) {
				t.Fatalf("renewOK=%v but target = %+v", renewOK, tg)
			}
			if !renewOK && tg.ErrorCount != 0 {
				t.Fatalf("lost lease counted as error: %+v", tg)
			}
		})
	}
}

func TestNew(t *testing.T) {
	st := openStore(t)
	c := newCounter()
	if _, err := New(nil, c, Options{}); err == nil {
		t.Error("nil store accepted")
	}
	if _, err := New(st, nil, Options{}); err == nil {
		t.Error("nil checker accepted")
	}
	for _, o := range []Options{{Workers: -1}, {Interval: -1}, {LeaseTTL: -1}, {Jitter: 1}} {
		if _, err := New(st, c, o); err == nil {
			t.Errorf("%+v accepted", o)
		}
	}
	s, err := New(st, c, Options{})
	must(t, err)
	if s.Name() != "scheduler" {
		t.Errorf("Name = %q", s.Name())
	}
	o := s.opts
	if o.Workers != DefaultWorkers || o.Interval != time.Hour || o.Jitter != 0.1 || o.BackoffBase != time.Minute ||
		o.BackoffMax != 24*time.Hour || o.LeaseTTL != DefaultLeaseTTL || o.PollInterval != DefaultPollInterval ||
		o.Holder == "" || o.Clock == nil || o.Rand == nil {
		t.Errorf("defaults not applied: %+v", o)
	}
	if r := o.Rand(); r < 0 || r >= 1 {
		t.Errorf("default rand = %v", r)
	}
	s2, err := New(st, c, Options{})
	must(t, err)
	if s2.opts.Holder == o.Holder {
		t.Error("two schedulers got the same holder id")
	}
	if s3, _ := New(st, c, Options{Jitter: -1}); s3.opts.Jitter != 0 {
		t.Error("negative jitter does not disable it")
	}
}

func TestRealClock(t *testing.T) {
	var c Clock = realClock{}
	if d := time.Since(c.Now()); d < 0 || d > time.Minute {
		t.Fatalf("Now off by %v", d)
	}
	ch, stop := c.Timer(time.Nanosecond)
	<-ch
	stop()
}
