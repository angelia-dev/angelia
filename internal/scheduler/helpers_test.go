package scheduler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
	"github.com/angelia-dev/angelia/internal/supervisor"
)

var (
	_ supervisor.Role = (*Scheduler)(nil)
	_ Store           = (*sqlite.Store)(nil)
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// fakeClock only moves when Advance is called.
type fakeClock struct {
	mu     sync.Mutex
	cond   *sync.Cond
	now    time.Time
	timers map[int]fakeTimer
	nextID int
}

type fakeTimer struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock(now time.Time) *fakeClock {
	c := &fakeClock{now: now, timers: map[int]fakeTimer{}}
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Timer(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch, func() {}
	}
	id := c.nextID
	c.nextID++
	c.timers[id] = fakeTimer{at: c.now.Add(d), ch: ch}
	c.cond.Broadcast()
	return ch, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.timers, id)
	}
}

// Advance moves the clock and fires the timers that are due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for id, t := range c.timers {
		if !t.at.After(c.now) {
			t.ch <- c.now
			delete(c.timers, id)
		}
	}
}

// waitTimers blocks until at least n timers are pending.
func (c *fakeClock) waitTimers(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.timers) < n {
		c.cond.Wait()
	}
}

func openStore(t *testing.T) *sqlite.Store {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "angelia.db"), logger)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func addTarget(t *testing.T, st store.Store, id, name string, due time.Time) {
	t.Helper()
	must(t, st.CreateTarget(t.Context(), store.Target{
		ID: id, Kind: "pypi", Coordinates: map[string]string{"name": name},
		FetchState: json.RawMessage(`{"etag":"e0"}`), NextCheckAt: due,
	}))
}

func getTarget(t *testing.T, st store.Store, id string) store.Target {
	t.Helper()
	got, err := st.GetTarget(t.Context(), id)
	must(t, err)
	return got
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// newTestScheduler builds a scheduler with a quiet logger, no jitter and
// the given clock; mod adjusts the options.
func newTestScheduler(t *testing.T, st Store, c Checker, clk Clock, mod func(*Options)) *Scheduler {
	t.Helper()
	o := Options{
		Clock: clk, Jitter: -1, Holder: "test",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mod != nil {
		mod(&o)
	}
	s, err := New(st, c, o)
	must(t, err)
	return s
}

// pollAndWait runs one poll and waits for the checks it started.
func pollAndWait(ctx context.Context, s *Scheduler) int {
	n := s.poll(ctx)
	s.wg.Wait()
	return n
}

// counter is a Checker that counts checks per target and records the
// highest number of simultaneous checks of any single target.
type counter struct {
	mu        sync.Mutex
	counts    map[string]int
	active    map[string]int
	maxActive int
	hold      func() // runs while the check is active
}

func newCounter() *counter {
	return &counter{counts: map[string]int{}, active: map[string]int{}}
}

func (c *counter) Check(_ context.Context, t store.Target) (Outcome, error) {
	c.mu.Lock()
	c.counts[t.ID]++
	c.active[t.ID]++
	c.maxActive = max(c.maxActive, c.active[t.ID])
	c.mu.Unlock()
	if c.hold != nil {
		c.hold()
	}
	c.mu.Lock()
	c.active[t.ID]--
	c.mu.Unlock()
	return Outcome{}, nil
}

func (c *counter) count(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[id]
}

func (c *counter) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.counts {
		n += v
	}
	return n
}

// lockedRand is a deterministic, goroutine-safe random source.
func lockedRand(vals ...float64) func() float64 {
	var mu sync.Mutex
	i := 0
	return func() float64 {
		mu.Lock()
		defer mu.Unlock()
		v := vals[i%len(vals)]
		i++
		return v
	}
}
