package dispatch

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/sink"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
	"github.com/angelia-dev/angelia/internal/supervisor"
)

var (
	_ supervisor.Role = (*Dispatcher)(nil)
	_ Store           = (*sqlite.Store)(nil)
	_ Sinks           = (*sink.Registry)(nil)
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

const sinkKind = "test"

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeClock only moves when Set or Advance is called. Its timers never
// fire: tests drive poll directly.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func (c *fakeClock) Timer(time.Duration) (<-chan time.Time, func()) {
	return make(chan time.Time), func() {}
}

// testSink calls fn for every attempt and records the requests. It also
// checks that no idempotency key is ever sent by two callers at once.
type testSink struct {
	fn func(ctx context.Context, req sink.Request) error

	mu       sync.Mutex
	reqs     []sink.Request
	active   map[string]bool
	parallel []string // keys seen while already being sent
}

func (s *testSink) Kind() string               { return sinkKind }
func (s *testSink) Validate(sink.Config) error { return nil }

func (s *testSink) Deliver(ctx context.Context, req sink.Request) (sink.Result, error) {
	s.mu.Lock()
	if s.active == nil {
		s.active = map[string]bool{}
	}
	if s.active[req.IdempotencyKey] {
		s.parallel = append(s.parallel, req.IdempotencyKey)
	}
	s.active[req.IdempotencyKey] = true
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.active, req.IdempotencyKey)
		s.mu.Unlock()
	}()
	if s.fn == nil {
		return sink.Result{StatusCode: 204}, nil
	}
	return sink.Result{StatusCode: 200}, s.fn(ctx, req)
}

func (s *testSink) requests() []sink.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sink.Request(nil), s.reqs...)
}

func registry(t *testing.T, s sink.Sink) *sink.Registry {
	t.Helper()
	r := sink.NewRegistry()
	if err := r.Register(s); err != nil {
		t.Fatal(err)
	}
	return r
}

func openStore(t *testing.T, path string) *sqlite.Store {
	t.Helper()
	st, err := sqlite.Open(t.Context(), path, discard())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newStore(t *testing.T) (*sqlite.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "angelia.db")
	st := openStore(t, path)
	must(t, st.CreateSink(t.Context(), store.Sink{ID: "k1", Kind: sinkKind, ConfigEncrypted: []byte(`{"url":"https://example.com/hook"}`)}))
	must(t, st.CreateSubscription(t.Context(), store.Subscription{ID: "s1", SinkID: "k1"}))
	return st, path
}

func payload(id string) []byte {
	return fmt.Appendf(nil, `{"specversion":"1.0","id":%q,"type":"dev.angelia.release.published.v1","subject":"pypi:requests@2.33.0"}`, id)
}

// addEvents writes n events with one delivery each for s1, due at t0.
func addEvents(t *testing.T, st *sqlite.Store, n int) {
	t.Helper()
	for i := range n {
		id := fmt.Sprintf("E%04d", i)
		must(t, st.AppendEvent(t.Context(), store.Event{ID: id, WatchID: "w1", Type: "dev.angelia.release.published.v1", Payload: payload(id), CreatedAt: t0},
			[]store.Delivery{{ID: "D" + id, SubscriptionID: "s1", Status: store.DeliveryPending, NextAttemptAt: t0}}))
	}
}

func delivery(t *testing.T, st *sqlite.Store, eventID string) store.Delivery {
	t.Helper()
	ds, err := st.ListDeliveries(t.Context(), eventID)
	must(t, err)
	if len(ds) != 1 {
		t.Fatalf("event %s has %d deliveries", eventID, len(ds))
	}
	return ds[0]
}

func newDispatcher(t *testing.T, st Store, sinks Sinks, opts Options) *Dispatcher {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = discard()
	}
	d, err := New(st, sinks, opts)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// pollWait runs one poll and waits for the attempts it started.
func pollWait(t *testing.T, d *Dispatcher) int {
	t.Helper()
	n := d.poll(t.Context())
	d.wg.Wait()
	return n
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
