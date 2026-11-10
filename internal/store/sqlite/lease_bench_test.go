package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// seedBacklog writes a delivery backlog shaped like a dispatcher under load
// (story #232): pending deliveries already due, written by events that fan
// out to 100 subscriptions each, so 100 rows share one next_attempt_at;
// leaseLimit of the oldest ones in flight under a live lease (the workers);
// and a tenth of the backlog held until after now.
func seedBacklog(tb testing.TB, s *Store, pendingN, leaseLimit int, now time.Time) {
	tb.Helper()
	ctx := context.Background()
	if err := s.CreateTarget(ctx, store.Target{ID: "t1", Kind: "pypi", Coordinates: map[string]string{"name": "x"}, NextCheckAt: now}); err != nil {
		tb.Fatal(err)
	}
	if err := s.CreateWatch(ctx, store.Watch{ID: "w1", TargetID: "t1"}); err != nil {
		tb.Fatal(err)
	}
	if err := s.CreateSink(ctx, store.Sink{ID: "k1", Kind: "webhook", ConfigEncrypted: []byte{1}}); err != nil {
		tb.Fatal(err)
	}
	const fanout = 100
	held := pendingN / 10
	total := pendingN + held
	for ev := 0; ev*fanout < total; ev++ {
		e := store.Event{ID: fmt.Sprintf("e%06d", ev), WatchID: "w1", Type: "release.published", Payload: []byte(`{}`), CreatedAt: now}
		due := now.Add(-time.Hour).Add(time.Duration(ev) * time.Millisecond)
		ds := make([]store.Delivery, 0, fanout)
		for i := 0; i < fanout && ev*fanout+i < total; i++ {
			n := ev*fanout + i
			d := store.Delivery{ID: fmt.Sprintf("d%07d", n), SubscriptionID: fmt.Sprintf("s%03d", i), Status: store.DeliveryPending, NextAttemptAt: due}
			if n >= pendingN {
				d.NextAttemptAt = now.Add(time.Hour)
			}
			ds = append(ds, d)
		}
		if err := s.AppendEvent(ctx, e, ds); err != nil {
			tb.Fatal(err)
		}
	}
	if _, err := s.LeaseDueDeliveries(ctx, "worker", now, now.Add(time.Minute), leaseLimit); err != nil {
		tb.Fatal(err)
	}
}

// BenchmarkListLeasableDueDeliveries measures the dispatcher's lease query
// (one poll for 16 free workers) against the size of the due backlog. Its
// cost must not grow with the backlog (story #232).
func BenchmarkListLeasableDueDeliveries(b *testing.B) {
	const limit = 16
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, n := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("pending=%d", n), func(b *testing.B) {
			ctx := context.Background()
			s, err := Open(ctx, filepath.Join(b.TempDir(), "angelia.db"), discard())
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			seedBacklog(b, s, n, limit, now)
			p := ListLeasableDueDeliveriesParams{Now: millis(now), MaxRows: limit}
			for b.Loop() {
				ids, err := s.q.ListLeasableDueDeliveries(ctx, p)
				if err != nil || len(ids) != limit {
					b.Fatalf("list = %d rows, %v", len(ids), err)
				}
			}
		})
	}
}
