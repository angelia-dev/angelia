package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/pgtest"
)

// seedBacklog writes a delivery backlog shaped like a dispatcher under load,
// as the SQLite benchmark of story #232 does: pendingN deliveries already due,
// written by events that fan out to 100 subscriptions each, so 100 rows share
// one next_attempt_at; inFlight of the oldest ones in flight under a live
// lease (the busy workers); and a tenth of the backlog held until after now.
func seedBacklog(tb testing.TB, s *Store, pendingN, inFlight int, now time.Time) {
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
	total := pendingN + pendingN/10
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
	if got, err := s.LeaseDueDeliveries(ctx, "busy", now, now.Add(time.Hour), inFlight); err != nil || len(got) != inFlight {
		tb.Fatalf("in-flight seed: %d, %v", len(got), err)
	}
	if _, err := s.db.ExecContext(ctx, `ANALYZE`); err != nil {
		tb.Fatal(err)
	}
}

// BenchmarkLeaseDueDeliveries measures the dispatcher's lease call on
// Postgres (one poll for 16 free workers) with 20,000 pending deliveries due
// and 1,000 in flight under live leases (story #86: under 20 ms on the CI
// runner). The leased rows are put back as pending, due after now, outside
// the timer, so the in-flight count stays at 1,000.
func BenchmarkLeaseDueDeliveries(b *testing.B) {
	const pending, inFlight, limit = 20_000, 1_000, 16
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	s, err := Open(context.Background(), pgtest.DSN(b), discardLogger())
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	seedBacklog(b, s, pending, inFlight, now)
	ctx := context.Background()
	b.Run("call=LeaseDueDeliveries", func(b *testing.B) {
		for b.Loop() {
			got, err := s.LeaseDueDeliveries(ctx, "bench", now, now.Add(time.Minute), limit)
			if err != nil || len(got) != limit {
				b.Fatalf("lease = %d, %v", len(got), err)
			}
			b.StopTimer()
			for _, ld := range got {
				f := store.DeliveryFinish{ID: ld.Delivery.ID, Attempt: ld.Delivery.Attempts, Status: store.DeliveryPending, NextAttemptAt: now.Add(2 * time.Hour)}
				if ok, err := s.FinishDelivery(ctx, f, "bench"); err != nil || !ok {
					b.Fatalf("finish = %v, %v", ok, err)
				}
			}
			b.StartTimer()
		}
	})
	b.Run("query=ListLeasableDueDeliveries", func(b *testing.B) {
		p := ListLeasableDueDeliveriesParams{Now: millis(now), MaxRows: limit}
		for b.Loop() {
			ids, err := s.q.ListLeasableDueDeliveries(ctx, p)
			if err != nil || len(ids) != limit {
				b.Fatalf("list = %d rows, %v", len(ids), err)
			}
		}
	})
}
