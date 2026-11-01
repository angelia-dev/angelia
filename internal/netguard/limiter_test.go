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
	l := NewLimiter(clock, HostLimit{
		Authenticated: Limit{Rate: rate, Burst: burst},
		Anonymous:     Limit{Rate: rate, Burst: burst},
	}, nil)

	var done atomic.Int64
	var mu sync.Mutex
	var grants []time.Duration
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			if err := l.Wait(t.Context(), "registry.example", false); err != nil {
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
	l := NewLimiter(clock, HostLimit{Anonymous: Limit{Rate: 1, Burst: 2}, Authenticated: Limit{Rate: 1, Burst: 2}}, nil)
	for range 2 {
		if err := l.Wait(t.Context(), "h", false); err != nil {
			t.Fatal(err)
		}
	}
	if s := clock.sleeps(); len(s) != 0 {
		t.Fatalf("burst should not sleep, slept %v", s)
	}
	clock.Advance(10 * time.Second) // refill, capped at burst
	for range 2 {
		if err := l.Wait(t.Context(), "h", false); err != nil {
			t.Fatal(err)
		}
	}
	if s := clock.sleeps(); len(s) != 0 {
		t.Fatalf("refilled burst should not sleep, slept %v", s)
	}
	if err := l.Wait(t.Context(), "h", false); err != nil {
		t.Fatal(err)
	}
	if s := clock.sleeps(); len(s) != 1 || s[0] != time.Second {
		t.Fatalf("third request should wait 1s, slept %v", s)
	}
}

func TestLimiterPerHostAndAuth(t *testing.T) {
	clock := newFakeClock(true)
	l := NewLimiter(clock,
		HostLimit{Anonymous: Limit{Rate: 1, Burst: 1}, Authenticated: Limit{Rate: 1, Burst: 1}},
		map[string]HostLimit{
			"api.example": {Authenticated: Limit{Rate: 100, Burst: 3}, Anonymous: Limit{Rate: 1, Burst: 1}},
		})
	ctx := t.Context()
	// Different hosts have separate buckets.
	_ = l.Wait(ctx, "a.example", false)
	_ = l.Wait(ctx, "b.example", false)
	// Authenticated requests get their own, larger bucket.
	for range 3 {
		_ = l.Wait(ctx, "API.example", true)
	}
	_ = l.Wait(ctx, "api.example", false)
	if s := clock.sleeps(); len(s) != 0 {
		t.Fatalf("no request should wait, slept %v", s)
	}
}

func TestLimiterPauseDelaysUntilReset(t *testing.T) {
	clock := newFakeClock(true)
	l := NewLimiter(clock, HostLimit{Anonymous: Limit{Rate: 10, Burst: 10}, Authenticated: Limit{Rate: 10, Burst: 10}}, nil)
	l.Pause("h", false, fakeEpoch.Add(30*time.Second))
	if err := l.Wait(t.Context(), "h", false); err != nil {
		t.Fatal(err)
	}
	if got := clock.Now().Sub(fakeEpoch); got < 30*time.Second {
		t.Fatalf("request granted at %v, want >= 30s", got)
	}
	// A pause in the past or beyond maxPause is ignored or capped.
	l.Pause("h", false, fakeEpoch)
	l.Pause("other", false, clock.Now().Add(100*time.Hour))
	start := clock.Now()
	_ = l.Wait(t.Context(), "other", false)
	if got := clock.Now().Sub(start); got > defaultMaxPause+time.Second {
		t.Fatalf("pause not capped: waited %v", got)
	}
}

func TestLimiterWaitCancelled(t *testing.T) {
	clock := newFakeClock(false)
	l := NewLimiter(clock, HostLimit{Anonymous: Limit{Rate: 1, Burst: 1}, Authenticated: Limit{Rate: 1, Burst: 1}}, nil)
	_ = l.Wait(t.Context(), "h", false)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := l.Wait(ctx, "h", false); !errors.Is(err, context.Canceled) {
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
