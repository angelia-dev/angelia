package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
	"github.com/angelia-dev/angelia/internal/supervisor"
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

// Jitter spreads both the interval and the backoff by ±10% by default.
// The cap applies before jitter, so targets at the cap are spread around
// it instead of all landing on exactly BackoffMax.
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
		{"backoff at cap, low", 0, 20, errors.New("x"), 21*time.Hour + 36*time.Minute},
		{"backoff at cap, middle", 0.5, 20, errors.New("x"), 24 * time.Hour},
		{"backoff at cap, high", 0.99999, 20, errors.New("x"), 26*time.Hour + 24*time.Minute},
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

// A target's own interval (set by a watch file) replaces Options.Interval;
// Outcome.Interval still wins over it.
func TestTargetInterval(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  time.Duration
		want time.Duration
	}{
		{"target interval", 0, 15 * time.Minute},
		{"outcome wins", 5 * time.Minute, 5 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			clk := newFakeClock(t0)
			addTarget(t, st, "t1", "requests", t0)
			tg := getTarget(t, st, "t1")
			if err := st.InConfigTx(t.Context(), func(tx store.ConfigTx) error {
				return tx.SetTargetInterval(t.Context(), "t1", 15*time.Minute, tg.NextCheckAt)
			}); err != nil {
				t.Fatal(err)
			}
			c := CheckerFunc(func(_ context.Context, got store.Target) (Outcome, error) {
				if got.Interval != 15*time.Minute {
					t.Errorf("leased target interval = %v", got.Interval)
				}
				return Outcome{Interval: tc.out}, nil
			})
			s := newTestScheduler(t, st, c, clk, nil)
			pollAndWait(t.Context(), s)
			if d := getTarget(t, st, "t1").NextCheckAt.Sub(t0); d != tc.want {
				t.Fatalf("delay = %v, want %v", d, tc.want)
			}
		})
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
	for _, o := range []Options{
		{Workers: -1},
		{Interval: -1},
		{LeaseTTL: -1},
		{Jitter: 1},
		{CheckTimeout: -1},
		{CheckTimeout: DefaultLeaseTTL},
		{LeaseTTL: time.Minute, CheckTimeout: 2 * time.Minute},
	} {
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
		o.CheckTimeout != DefaultCheckTimeout ||
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
	// A short lease TTL lowers the default check timeout below it.
	if s4, _ := New(st, c, Options{LeaseTTL: time.Minute}); s4.opts.CheckTimeout != 30*time.Second {
		t.Errorf("check timeout with a 1m TTL = %v", s4.opts.CheckTimeout)
	}
	// Huge durations are clamped so jitter cannot overflow.
	s5, err := New(st, c, Options{Interval: math.MaxInt64, BackoffMax: math.MaxInt64})
	must(t, err)
	if s5.opts.Interval != MaxInterval || s5.opts.BackoffMax != MaxInterval {
		t.Errorf("not clamped: interval %v, backoff max %v", s5.opts.Interval, s5.opts.BackoffMax)
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

// spyStore wraps a store, counts calls and can inject failures.
type spyStore struct {
	Store
	mu          sync.Mutex
	leaseCalls  int
	renewErr    error // returned by AcquireLease (renewals) when set
	leaseErr    error // returned by HasDueTargets and LeaseDueTargets when set
	pruned      []int64
	pruneCalls  int
	fencedWrite []bool
}

func (s *spyStore) LeaseDueTargets(ctx context.Context, holder string, now, until time.Time, limit int) ([]store.Target, error) {
	s.mu.Lock()
	s.leaseCalls++
	err := s.leaseErr
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.Store.LeaseDueTargets(ctx, holder, now, until, limit)
}

func (s *spyStore) HasDueTargets(ctx context.Context, now time.Time) (bool, error) {
	s.mu.Lock()
	err := s.leaseErr
	s.mu.Unlock()
	if err != nil {
		return false, err
	}
	return s.Store.HasDueTargets(ctx, now)
}

func (s *spyStore) AcquireLease(ctx context.Context, name, holder string, now, until time.Time) (bool, error) {
	s.mu.Lock()
	err := s.renewErr
	s.mu.Unlock()
	if err != nil {
		return false, err
	}
	return s.Store.AcquireLease(ctx, name, holder, now, until)
}

func (s *spyStore) UpdateTargetLeased(ctx context.Context, t store.Target, lease, holder string, now time.Time) (bool, error) {
	ok, err := s.Store.UpdateTargetLeased(ctx, t, lease, holder, now)
	s.mu.Lock()
	s.fencedWrite = append(s.fencedWrite, ok)
	s.mu.Unlock()
	return ok, err
}

func (s *spyStore) DeleteExpiredTargetLeases(ctx context.Context, now time.Time) (int64, error) {
	n, err := s.Store.DeleteExpiredTargetLeases(ctx, now)
	s.mu.Lock()
	s.pruneCalls++
	s.pruned = append(s.pruned, n)
	s.mu.Unlock()
	return n, err
}

// Renewals that keep failing do not let a check run past its lease: once
// the last renewed expiry has passed, the check is abandoned.
func TestRenewalErrorPastExpiryCancelsCheck(t *testing.T) {
	base := openStore(t)
	clk := newFakeClock(t0)
	addTarget(t, base, "t1", "requests", t0)
	st := &spyStore{Store: base, renewErr: errors.New("database is locked")}
	started := make(chan struct{})
	cause := make(chan error, 1)
	c := CheckerFunc(func(ctx context.Context, _ store.Target) (Outcome, error) {
		close(started)
		<-ctx.Done()
		cause <- context.Cause(ctx)
		return Outcome{}, ctx.Err()
	})
	const ttl = 3 * time.Minute
	s := newTestScheduler(t, st, c, clk, func(o *Options) { o.LeaseTTL = ttl })
	s.poll(t.Context())
	<-started
	// Renewals at +1m and +2m fail while the lease is still valid: the
	// check goes on.
	for range 2 {
		clk.waitTimers(1)
		clk.Advance(time.Minute)
	}
	clk.waitTimers(1)
	select {
	case err := <-cause:
		t.Fatalf("check cancelled before the lease expired: %v", err)
	default:
	}
	// The renewal at +3m fails at the lease's expiry: abandon.
	clk.Advance(time.Minute)
	if err := <-cause; !errors.Is(err, errLeaseLost) {
		t.Fatalf("cause = %v, want errLeaseLost", err)
	}
	s.wg.Wait()
	if tg := getTarget(t, base, "t1"); !tg.NextCheckAt.Equal(t0) || tg.ErrorCount != 0 {
		t.Fatalf("abandoned check changed the target: %+v", tg)
	}
	if len(st.fencedWrite) != 0 {
		t.Fatalf("abandoned check wrote the target")
	}
}

// A second holder that takes the target over (here: its clock runs past
// the first holder's lease) fences out the first holder's late write.
func TestTakeoverMakesFencedWriteNoop(t *testing.T) {
	base := openStore(t)
	clk := newFakeClock(t0)
	addTarget(t, base, "t1", "requests", t0)
	st := &spyStore{Store: base}
	const ttl = 3 * time.Minute
	skewed := t0.Add(10 * time.Minute) // holder b's clock
	c := CheckerFunc(func(ctx context.Context, _ store.Target) (Outcome, error) {
		got, err := base.LeaseDueTargets(ctx, "b", skewed, skewed.Add(ttl), 10)
		if err != nil || len(got) != 1 {
			t.Errorf("b did not take over: %v %v", got, err)
		}
		return Outcome{FetchState: json.RawMessage(`{"etag":"stale"}`)}, nil
	})
	s := newTestScheduler(t, st, c, clk, func(o *Options) { o.LeaseTTL = ttl; o.Holder = "a" })
	pollAndWait(t.Context(), s)
	if fmt.Sprint(st.fencedWrite) != "[false]" {
		t.Fatalf("fenced writes = %v, want [false]", st.fencedWrite)
	}
	tg := getTarget(t, base, "t1")
	if string(tg.FetchState) != `{"etag":"e0"}` || !tg.NextCheckAt.Equal(t0) {
		t.Fatalf("a overwrote b's target: %+v", tg)
	}
	// a's release did not drop b's lease.
	if ok, err := base.AcquireLease(t.Context(), store.TargetLeaseName("t1"), "c", skewed, skewed.Add(ttl)); err != nil || ok {
		t.Fatalf("b's lease gone: %v %v", ok, err)
	}
}

// A check that runs past CheckTimeout is cancelled and counts as an error.
func TestCheckTimeout(t *testing.T) {
	st := openStore(t)
	clk := newFakeClock(t0)
	addTarget(t, st, "t1", "requests", t0)
	cause := make(chan error, 1)
	c := CheckerFunc(func(ctx context.Context, _ store.Target) (Outcome, error) {
		<-ctx.Done()
		cause <- context.Cause(ctx)
		return Outcome{}, fmt.Errorf("fetch: %w", ctx.Err())
	})
	s := newTestScheduler(t, st, c, clk, func(o *Options) { o.CheckTimeout = 20 * time.Millisecond })
	pollAndWait(t.Context(), s)
	if err := <-cause; !errors.Is(err, errCheckTimeout) {
		t.Fatalf("cause = %v", err)
	}
	tg := getTarget(t, st, "t1")
	if tg.ErrorCount != 1 || !tg.NextCheckAt.Equal(t0.Add(DefaultBackoffBase)) {
		t.Fatalf("timeout not counted as an error: %+v", tg)
	}
}

// Idle polls only read: the leasing write transaction runs only when a
// target is due.
func TestIdlePollDoesNotLease(t *testing.T) {
	base := openStore(t)
	clk := newFakeClock(t0)
	addTarget(t, base, "t1", "requests", t0.Add(time.Hour))
	for _, withMetrics := range []bool{false, true} {
		st := &spyStore{Store: base}
		s := newTestScheduler(t, st, newCounter(), clk, func(o *Options) {
			if withMetrics {
				o.Metrics = &fakeMetrics{}
			}
		})
		for range 5 {
			pollAndWait(t.Context(), s)
		}
		if st.leaseCalls != 0 {
			t.Fatalf("metrics=%v: %d leasing transactions while idle", withMetrics, st.leaseCalls)
		}
	}
	clk.Advance(time.Hour)
	st := &spyStore{Store: base}
	s := newTestScheduler(t, st, newCounter(), clk, nil)
	if n := pollAndWait(t.Context(), s); n != 1 || st.leaseCalls != 1 {
		t.Fatalf("due target: started %d with %d leasing transactions", n, st.leaseCalls)
	}
}

// Huge intervals from a checker are clamped to MaxInterval instead of
// overflowing through jitter.
func TestIntervalClamped(t *testing.T) {
	st := openStore(t)
	clk := newFakeClock(t0)
	addTarget(t, st, "t1", "requests", t0)
	c := CheckerFunc(func(context.Context, store.Target) (Outcome, error) {
		return Outcome{Interval: math.MaxInt64}, nil
	})
	s := newTestScheduler(t, st, c, clk, func(o *Options) { o.Jitter = 0.1; o.Rand = func() float64 { return 0.99999 } })
	pollAndWait(t.Context(), s)
	got := getTarget(t, st, "t1").NextCheckAt.Sub(t0)
	if got < MaxInterval || got > MaxInterval*11/10 {
		t.Fatalf("delay = %v, want MaxInterval + jitter", got)
	}
}

// Expired target leases of crashed holders are deleted once per
// leasePruneEvery.
func TestExpiredLeasesPruned(t *testing.T) {
	base := openStore(t)
	clk := newFakeClock(t0)
	ctx := t.Context()
	// A crashed holder's lease on a target that is not due.
	ok, err := base.AcquireLease(ctx, store.TargetLeaseName("gone"), "crashed", t0, t0.Add(time.Minute))
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	st := &spyStore{Store: base}
	s := newTestScheduler(t, st, newCounter(), clk, nil)
	pollAndWait(ctx, s) // first poll prunes; the lease is still live
	clk.Advance(30 * time.Minute)
	pollAndWait(ctx, s) // too soon to prune again
	clk.Advance(30 * time.Minute)
	pollAndWait(ctx, s)
	if fmt.Sprint(st.pruned) != "[0 1]" {
		t.Fatalf("pruned = %v, want [0 1]", st.pruned)
	}
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) count(sub string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.b.String(), sub)
}

// A failing store is logged once per minute, not on every poll.
func TestStoreErrorLogRateLimited(t *testing.T) {
	base := openStore(t)
	clk := newFakeClock(t0)
	addTarget(t, base, "t1", "requests", t0.Add(time.Hour))
	st := &spyStore{Store: base, leaseErr: errors.New("disk I/O error")}
	logs := &syncBuffer{}
	s := newTestScheduler(t, st, newCounter(), clk, func(o *Options) {
		o.Logger = slog.New(slog.NewTextHandler(logs, nil))
	})
	ctx := t.Context()
	for range 60 { // one minute of 1s polls
		pollAndWait(ctx, s)
		clk.Advance(time.Second)
	}
	if n := logs.count("disk I/O error"); n != 1 {
		t.Fatalf("logged %d times in the first minute, want 1", n)
	}
	pollAndWait(ctx, s)
	if n := logs.count("disk I/O error"); n != 2 {
		t.Fatalf("logged %d times after a minute, want 2", n)
	}
	st.mu.Lock()
	st.leaseErr = nil
	st.mu.Unlock()
	pollAndWait(ctx, s)
	if logs.count("store reachable again") != 1 {
		t.Fatal("recovery not logged")
	}
}

// Acceptance criteria (#177): writeTimeout is above SQLite's busy timeout, so
// contention delays the final write instead of dropping the check result; the
// lease invariants hold; and the shutdown path fits in the drain timeout.
func TestWriteTimeoutAboveBusyTimeout(t *testing.T) {
	if writeTimeout <= sqlite.BusyTimeout {
		t.Errorf("writeTimeout %v not above the SQLite busy timeout %v", writeTimeout, sqlite.BusyTimeout)
	}
	// Lease: a check that runs to its timeout still records its result
	// (one write) while the renewed lease is held.
	if DefaultCheckTimeout >= DefaultLeaseTTL {
		t.Errorf("check timeout %v not below the lease TTL %v", DefaultCheckTimeout, DefaultLeaseTTL)
	}
	if DefaultCheckTimeout+writeTimeout >= DefaultLeaseTTL {
		t.Errorf("check %v + write %v not below the lease TTL %v", DefaultCheckTimeout, writeTimeout, DefaultLeaseTTL)
	}
	// Shutdown: Run waits for running checks. A check cut short only
	// releases its lease (one write); one that completed first records its
	// result and then releases (two writes in a row).
	if 2*writeTimeout >= supervisor.DefaultDrainTimeout {
		t.Errorf("2 x write %v not below the drain timeout %v", writeTimeout, supervisor.DefaultDrainTimeout)
	}
}
