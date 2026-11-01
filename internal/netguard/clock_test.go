package netguard

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manual clock for tests. In auto mode, Sleep advances the
// clock by the requested duration and returns at once, which suits
// sequential code. In manual mode, Sleep blocks until Advance moves the clock
// past the sleeper's deadline.
type fakeClock struct {
	mu       sync.Mutex
	now      time.Time
	auto     bool
	sleepers []*sleeper
	slept    []time.Duration
}

type sleeper struct {
	until time.Time
	ch    chan struct{}
}

// fakeEpoch has whole seconds, so it survives the HTTP date format.
var fakeEpoch = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func newFakeClock(auto bool) *fakeClock {
	return &fakeClock{now: fakeEpoch, auto: auto}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	f.mu.Lock()
	f.slept = append(f.slept, d)
	if f.auto {
		f.now = f.now.Add(d)
		f.mu.Unlock()
		return ctx.Err()
	}
	s := &sleeper{until: f.now.Add(d), ch: make(chan struct{})}
	f.sleepers = append(f.sleepers, s)
	f.mu.Unlock()
	select {
	case <-s.ch:
		return nil
	case <-ctx.Done():
		f.mu.Lock()
		for i, o := range f.sleepers {
			if o == s {
				f.sleepers = append(f.sleepers[:i], f.sleepers[i+1:]...)
				break
			}
		}
		f.mu.Unlock()
		return ctx.Err()
	}
}

// Advance moves the clock forward and wakes the sleepers that are due.
func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	kept := f.sleepers[:0]
	for _, s := range f.sleepers {
		if !s.until.After(f.now) {
			close(s.ch)
			continue
		}
		kept = append(kept, s)
	}
	f.sleepers = kept
}

func (f *fakeClock) numSleepers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sleepers)
}

func (f *fakeClock) sleeps() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.slept...)
}

func TestRealClockSleep(t *testing.T) {
	c := realClock{}
	if c.Now().IsZero() {
		t.Fatal("Now returned the zero time")
	}
	if err := c.Sleep(t.Context(), time.Millisecond); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if err := c.Sleep(t.Context(), 0); err != nil {
		t.Fatalf("Sleep(0): %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.Sleep(ctx, time.Hour); err == nil {
		t.Fatal("Sleep on a cancelled context returned nil")
	}
}
