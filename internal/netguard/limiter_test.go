package netguard

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// AC: rate limiter tested with a fake clock: bursts above the budget wait,
// never exceed.
func TestLimiterBurstAboveBudgetWaitsNeverExceeds(t *testing.T) {
	const (
		n     = 40
		burst = 5
		rate  = 2.0 // tokens per second
		step  = 50 * time.Millisecond
	)
	clock := newFakeClock(false)
	l := newLimiter(clock, HostLimit{
		Authenticated: Limit{Rate: rate, Burst: burst},
		Anonymous:     Limit{Rate: rate, Burst: burst},
	}, nil)

	var done atomic.Int64
	var mu sync.Mutex
	var grants []time.Duration
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			if err := l.wait(t.Context(), "registry.example", false); err != nil {
				t.Errorf("Wait: %v", err)
				return
			}
			mu.Lock()
			grants = append(grants, clock.Now().Sub(fakeEpoch))
			mu.Unlock()
			done.Add(1)
		})
	}

	// settle waits until every goroutine has either finished or is asleep
	// on the fake clock, so the clock only moves when nothing can run.
	settle := func() {
		deadline := time.Now().Add(5 * time.Second)
		for int(done.Load())+clock.numSleepers() != n {
			if time.Now().After(deadline) {
				t.Fatalf("goroutines did not settle: done=%d sleepers=%d", done.Load(), clock.numSleepers())
			}
			time.Sleep(time.Millisecond)
		}
	}

	settle()
	if got := done.Load(); got != burst {
		t.Fatalf("granted at t=0: got %d, want the burst %d", got, burst)
	}
	for done.Load() < n {
		clock.Advance(step)
		settle()
		elapsed := clock.Now().Sub(fakeEpoch).Seconds()
		maxAllowed := burst + int64(math.Floor(rate*elapsed+1e-9))
		if got := done.Load(); got > maxAllowed {
			t.Fatalf("at t=%.2fs: %d grants, budget allows %d", elapsed, got, maxAllowed)
		}
	}
	wg.Wait()

	// Requests above the burst waited: the last one is granted no earlier
	// than (n-burst)/rate seconds.
	want := time.Duration(float64(n-burst) / rate * float64(time.Second))
	var last time.Duration
	for _, g := range grants {
		last = max(last, g)
	}
	if last < want {
		t.Fatalf("last grant at %v, want >= %v", last, want)
	}
	if last > want+step {
		t.Fatalf("last grant at %v, want <= %v (limiter too slow)", last, want+step)
	}
}

func TestLimiterRefillsAfterIdle(t *testing.T) {
	clock := newFakeClock(true)
	l := newLimiter(clock, HostLimit{Anonymous: Limit{Rate: 1, Burst: 2}, Authenticated: Limit{Rate: 1, Burst: 2}}, nil)
	for range 2 {
		if err := l.wait(t.Context(), "h", false); err != nil {
			t.Fatal(err)
		}
	}
	if s := clock.sleeps(); len(s) != 0 {
		t.Fatalf("burst should not sleep, slept %v", s)
	}
	clock.Advance(10 * time.Second) // refill, capped at burst
	for range 2 {
		if err := l.wait(t.Context(), "h", false); err != nil {
			t.Fatal(err)
		}
	}
	if s := clock.sleeps(); len(s) != 0 {
		t.Fatalf("refilled burst should not sleep, slept %v", s)
	}
	if err := l.wait(t.Context(), "h", false); err != nil {
		t.Fatal(err)
	}
	if s := clock.sleeps(); len(s) != 1 || s[0] != time.Second {
		t.Fatalf("third request should wait 1s, slept %v", s)
	}
}

func TestLimiterPerHostAndAuth(t *testing.T) {
	clock := newFakeClock(true)
	l := newLimiter(clock,
		HostLimit{Anonymous: Limit{Rate: 1, Burst: 1}, Authenticated: Limit{Rate: 1, Burst: 1}},
		map[string]HostLimit{
			"api.example": {Authenticated: Limit{Rate: 100, Burst: 3}, Anonymous: Limit{Rate: 1, Burst: 1}},
		})
	ctx := t.Context()
	// Different hosts have separate buckets.
	_ = l.wait(ctx, "a.example", false)
	_ = l.wait(ctx, "b.example", false)
	// Authenticated requests get their own, larger bucket.
	for range 3 {
		_ = l.wait(ctx, "API.example", true)
	}
	_ = l.wait(ctx, "api.example", false)
	if s := clock.sleeps(); len(s) != 0 {
		t.Fatalf("no request should wait, slept %v", s)
	}
}

func TestLimiterPauseDelaysUntilReset(t *testing.T) {
	clock := newFakeClock(true)
	l := newLimiter(clock, HostLimit{Anonymous: Limit{Rate: 10, Burst: 10}, Authenticated: Limit{Rate: 10, Burst: 10}}, nil)
	l.pause("h", false, fakeEpoch.Add(30*time.Second))
	if err := l.wait(t.Context(), "h", false); err != nil {
		t.Fatal(err)
	}
	if got := clock.Now().Sub(fakeEpoch); got < 30*time.Second {
		t.Fatalf("request granted at %v, want >= 30s", got)
	}
	// A pause in the past or beyond maxPause is ignored or capped.
	l.pause("h", false, fakeEpoch)
	l.pause("other", false, clock.Now().Add(100*time.Hour))
	start := clock.Now()
	_ = l.wait(t.Context(), "other", false)
	if got := clock.Now().Sub(start); got > defaultMaxPause+time.Second {
		t.Fatalf("pause not capped: waited %v", got)
	}
}

func TestLimiterWaitCancelled(t *testing.T) {
	clock := newFakeClock(false)
	l := newLimiter(clock, HostLimit{Anonymous: Limit{Rate: 1, Burst: 1}, Authenticated: Limit{Rate: 1, Burst: 1}}, nil)
	_ = l.wait(t.Context(), "h", false)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := l.wait(ctx, "h", false); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait: got %v, want context.Canceled", err)
	}
}

func TestBudget(t *testing.T) {
	l, err := Budget(5000, time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	// burst + rate*per must not exceed the budget.
	if total := float64(l.Burst) + l.Rate*3600; total > 5000+1e-6 {
		t.Fatalf("Budget allows %.2f requests per hour, want <= 5000", total)
	}
	for _, tc := range []struct {
		n, burst int
		per      time.Duration
	}{{10, 10, time.Hour}, {10, 0, time.Hour}, {10, 1, 0}} {
		if _, err := Budget(tc.n, tc.per, tc.burst); err == nil {
			t.Errorf("Budget(%d, %v, %d): want error", tc.n, tc.per, tc.burst)
		}
	}
}

func TestDefaultHostLimitsGitHub(t *testing.T) {
	gh, ok := DefaultHostLimits()["api.github.com"]
	if !ok {
		t.Fatal("api.github.com has no default limit")
	}
	if got := float64(gh.Authenticated.Burst) + gh.Authenticated.Rate*3600; got > 5000+1e-6 {
		t.Errorf("authenticated budget %.1f/h, want <= 5000", got)
	}
	if got := float64(gh.Anonymous.Burst) + gh.Anonymous.Rate*3600; got > 60+1e-6 {
		t.Errorf("anonymous budget %.1f/h, want <= 60", got)
	}
}

func TestDefaultHostLimitsCrates(t *testing.T) {
	l, ok := DefaultHostLimits()["crates.io"]
	if !ok {
		t.Fatal("crates.io has no default limit")
	}
	for name, lim := range map[string]Limit{"authenticated": l.Authenticated, "anonymous": l.Anonymous} {
		if lim.Rate != 1 || lim.Burst != 1 {
			t.Errorf("%s limit = %+v, want 1 request per second, burst 1 (crates.io crawler policy)", name, lim)
		}
	}
}

// waitUntil polls cond in real time; the fake clock does not move meanwhile.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// B1: waits that are cancelled, or whose deadline comes before their slot,
// give their token back, so they leave no debt behind.
func TestLimiterCancelledWaitsAreRefunded(t *testing.T) {
	const n = 100
	clock := newFakeClock(false)
	lim := Limit{Rate: 1.0 / 60, Burst: 1} // one per minute
	l := newLimiter(clock, HostLimit{Authenticated: lim, Anonymous: lim}, nil)
	ctx := t.Context()
	if err := l.wait(ctx, "h", false); err != nil { // uses the burst
		t.Fatal(err)
	}

	// Already-cancelled contexts never reserve.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for range n {
		if err := l.wait(cancelled, "h", false); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled wait: got %v", err)
		}
	}

	// A deadline before the slot fails at once, without sleeping.
	short, cancelShort := context.WithTimeout(ctx, 10*time.Second)
	defer cancelShort()
	for range n {
		if err := l.wait(short, "h", false); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("short-deadline wait: got %v", err)
		}
	}
	if s := clock.sleeps(); len(s) != 0 {
		t.Fatalf("short-deadline waits slept: %v", s)
	}

	// Waits cancelled while asleep refund their tokens.
	sleepCtx, cancelSleep := context.WithCancel(ctx)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() { errs <- l.wait(sleepCtx, "h", false) })
	}
	waitUntil(t, "sleepers", func() bool { return clock.numSleepers() == n })
	cancelSleep()
	wg.Wait()
	close(errs)
	for err := range errs {
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("sleeping wait: got %v", err)
		}
	}

	// The next real wait is ready within 1/rate.
	done := make(chan error, 1)
	go func() { done <- l.wait(ctx, "h", false) }()
	waitUntil(t, "real waiter", func() bool { return clock.numSleepers() == 1 })
	s := clock.sleeps()
	if got := s[len(s)-1]; got > time.Minute {
		t.Fatalf("next wait sleeps %v, want <= 1m", got)
	}
	clock.Advance(time.Minute)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// S2: a pause also delays callers already asleep in wait.
func TestLimiterPauseDelaysSleepingWaiters(t *testing.T) {
	clock := newFakeClock(false)
	lim := Limit{Rate: 1, Burst: 1}
	l := newLimiter(clock, HostLimit{Authenticated: lim, Anonymous: lim}, nil)
	if err := l.wait(t.Context(), "h", false); err != nil {
		t.Fatal(err)
	}
	var granted atomic.Int64 // fake-clock offset in ns, or -1 while waiting
	granted.Store(-1)
	go func() {
		if err := l.wait(t.Context(), "h", false); err != nil {
			t.Error(err)
		}
		granted.Store(int64(clock.Now().Sub(fakeEpoch)))
	}()
	waitUntil(t, "waiter asleep", func() bool { return clock.numSleepers() == 1 })

	l.pause("h", false, fakeEpoch.Add(30*time.Second))
	clock.Advance(time.Second) // the original slot
	waitUntil(t, "waiter re-sleeping or done", func() bool {
		return clock.numSleepers() == 1 || granted.Load() >= 0
	})
	if g := granted.Load(); g >= 0 {
		t.Fatalf("waiter granted at %v despite the pause", time.Duration(g))
	}
	clock.Advance(29 * time.Second)
	clock.Advance(time.Second)
	waitUntil(t, "grant", func() bool { return granted.Load() >= 0 })
	if g := time.Duration(granted.Load()); g < 31*time.Second {
		t.Fatalf("granted at %v, want >= 31s (pause end + its slot)", g)
	}
}

func TestLimiterHostKeysNormalised(t *testing.T) {
	clock := newFakeClock(true)
	one := Limit{Rate: 1, Burst: 1}
	l := newLimiter(clock, HostLimit{Authenticated: one, Anonymous: one}, map[string]HostLimit{
		"API.Example.": {Authenticated: one, Anonymous: Limit{Rate: 1, Burst: 3}},
	})
	for _, h := range []string{"api.example", "API.EXAMPLE", "api.example."} {
		if err := l.wait(t.Context(), h, false); err != nil {
			t.Fatal(err)
		}
	}
	if s := clock.sleeps(); len(s) != 0 {
		t.Fatalf("spellings of one host should share its burst of 3, slept %v", s)
	}
	l.pause("Api.Example.", false, clock.Now().Add(time.Minute))
	start := clock.Now()
	_ = l.wait(t.Context(), "api.example", false)
	if got := clock.Now().Sub(start); got < time.Minute {
		t.Fatalf("pause on another spelling not applied: waited %v", got)
	}
}
