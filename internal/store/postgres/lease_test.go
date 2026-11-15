package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/pgtest"
)

// twoStores opens two Store values on one fresh schema, the stand-in for two
// processes (story #86).
func twoStores(t *testing.T) (a, b *Store) {
	t.Helper()
	dsn := pgtest.DSN(t)
	return openStoreDSN(t, dsn), openStoreDSN(t, dsn)
}

func addDueTargets(t *testing.T, s *Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		must(t, s.CreateTarget(t.Context(), store.Target{ID: id, Kind: "pypi", Coordinates: map[string]string{"name": id}, NextCheckAt: t0}))
	}
}

// addDueDeliveries writes one event with a pending delivery due at t0 for
// each subscription s1..sN (the seed's s1 included) and returns their ids.
func addDueDeliveries(t *testing.T, s *Store, n int) []string {
	t.Helper()
	ctx := t.Context()
	ds := make([]store.Delivery, n)
	ids := make([]string, n)
	for i := range n {
		sub := fmt.Sprintf("s%d", i+1)
		if i > 0 {
			must(t, s.CreateSubscription(ctx, store.Subscription{ID: sub, SinkID: "k1"}))
		}
		ids[i] = "d-" + sub
		ds[i] = store.Delivery{ID: ids[i], SubscriptionID: sub, Status: store.DeliveryPending, NextAttemptAt: t0}
	}
	must(t, s.AppendEvent(ctx, storeEvent("e1", `{}`), ds))
	return ids
}

// AC 86-A: a lease row that another holder inserts between the select of due
// targets and the lease upsert skips that target. The call returns fewer
// targets and no ErrConflict, and the other holder keeps its lease.
func TestLeaseDueTargetsSkipsALeaseTakenMeanwhile(t *testing.T) {
	a, b := twoStores(t)
	ctx := t.Context()
	addDueTargets(t, a, "t1", "t2")
	a.afterLeaseSelect = func() {
		ok, err := b.AcquireLease(ctx, store.TargetLeaseName("t1"), "other", t0, t0.Add(time.Minute))
		if err != nil || !ok {
			t.Errorf("the other holder's lease: %v, %v", ok, err)
		}
	}
	got, err := a.LeaseDueTargets(ctx, "h", t0, t0.Add(time.Minute), 10)
	must(t, err)
	if len(got) != 1 || got[0].ID != "t2" {
		t.Fatalf("leased %+v, want t2 only", got)
	}
	if n := count(t, a.db, `SELECT count(*) FROM lease WHERE name = 'target:t1' AND holder = 'other'`); n != 1 {
		t.Fatal("the other holder lost its lease")
	}
}

// AC 86-A: the same for deliveries. The skipped delivery is not set in flight
// and gets no attempt.
func TestLeaseDueDeliveriesSkipsALeaseTakenMeanwhile(t *testing.T) {
	a, b := twoStores(t)
	seedStore(t, a)
	ctx := t.Context()
	addDueDeliveries(t, a, 2)
	a.afterLeaseSelect = func() {
		ok, err := b.AcquireLease(ctx, store.DeliveryLeaseName("d-s1"), "other", t0, t0.Add(time.Minute))
		if err != nil || !ok {
			t.Errorf("the other holder's lease: %v, %v", ok, err)
		}
	}
	got, err := a.LeaseDueDeliveries(ctx, "h", t0, t0.Add(time.Minute), 10)
	must(t, err)
	if len(got) != 1 || got[0].Delivery.ID != "d-s2" {
		t.Fatalf("leased %+v, want d-s2 only", got)
	}
	d, err := a.GetDelivery(ctx, "d-s1")
	must(t, err)
	if d.Status != store.DeliveryPending || d.Attempts != 0 {
		t.Fatalf("skipped delivery = %+v, want pending with no attempt", d)
	}
}

// advisoryLocks counts the advisory locks held or awaited in s's schema key.
func advisoryLocks(t *testing.T, s *Store) int64 {
	t.Helper()
	return count(t, s.db, `SELECT count(*) FROM pg_locks
WHERE locktype = 'advisory' AND objsubid = 2 AND classid = (($1::bigint + 4294967296) % 4294967296)::oid`, s.key)
}

// AC 86-A: no lease transaction takes an advisory lock. One lease transaction
// is held open after its select; a second one, from another store, returns
// within 1 s with a disjoint set.
func TestLeaseTransactionsRunConcurrently(t *testing.T) {
	type leaser func(s *Store, ctx context.Context, holder string) ([]string, error)
	targets := func(s *Store, ctx context.Context, holder string) ([]string, error) {
		ts, err := s.LeaseDueTargets(ctx, holder, t0, t0.Add(time.Minute), 4)
		ids := make([]string, len(ts))
		for i, x := range ts {
			ids[i] = x.ID
		}
		return ids, err
	}
	deliveries := func(s *Store, ctx context.Context, holder string) ([]string, error) {
		ds, err := s.LeaseDueDeliveries(ctx, holder, t0, t0.Add(time.Minute), 4)
		ids := make([]string, len(ds))
		for i, x := range ds {
			ids[i] = x.Delivery.ID
		}
		return ids, err
	}
	for name, c := range map[string]struct {
		seed  func(t *testing.T, s *Store)
		lease leaser
	}{
		"targets": {func(t *testing.T, s *Store) {
			addDueTargets(t, s, "t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8")
		}, targets},
		"deliveries": {func(t *testing.T, s *Store) { seedStore(t, s); addDueDeliveries(t, s, 8) }, deliveries},
	} {
		t.Run(name, func(t *testing.T) {
			a, b := twoStores(t)
			c.seed(t, a)
			entered, release := make(chan struct{}), make(chan struct{})
			a.afterLeaseSelect = func() { close(entered); <-release }
			type result struct {
				ids []string
				err error
			}
			first := make(chan result, 1)
			go func() {
				ids, err := c.lease(a, t.Context(), "a")
				first <- result{ids, err}
			}()
			<-entered
			if n := advisoryLocks(t, b); n != 0 {
				t.Errorf("%d advisory locks held while a lease transaction is open", n)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			start := time.Now()
			second, err := c.lease(b, ctx, "b")
			cancel()
			close(release)
			if err != nil {
				t.Fatalf("second lease transaction after %v: %v", time.Since(start), err)
			}
			r := <-first
			must(t, r.err)
			if len(r.ids) != 4 || len(second) != 4 {
				t.Fatalf("leased %v and %v, want 4 each", r.ids, second)
			}
			for _, id := range second {
				if slices.Contains(r.ids, id) {
					t.Fatalf("%s leased by both: %v and %v", id, r.ids, second)
				}
			}
		})
	}
}

// A writer that takes no lease lock waits for the row locks of an open lease
// transaction instead of changing a row under it: the target deletion and the
// failing of pending deliveries apply after the lease commits, and the batch
// is whole.
func TestWritersWaitForALeaseTransaction(t *testing.T) {
	a, b := twoStores(t)
	seedStore(t, a)
	ctx := t.Context()
	addDueTargets(t, a, "t2")
	addDueDeliveries(t, a, 2)
	wait := func(name string, lease func() int, write func() error) {
		t.Helper()
		done := make(chan error, 1)
		a.afterLeaseSelect = func() {
			go func() { done <- write() }()
			select {
			case err := <-done:
				t.Errorf("%s: the write did not wait for the lease transaction: %v", name, err)
			case <-time.After(200 * time.Millisecond):
			}
		}
		if n := lease(); n != 2 {
			t.Errorf("%s: leased %d rows, want 2", name, n)
		}
		a.afterLeaseSelect = nil
		select {
		case err := <-done:
			must(t, err)
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: the write did not finish after the lease committed", name)
		}
	}
	wait("targets", func() int {
		ts, err := a.LeaseDueTargets(ctx, "h", t0, t0.Add(time.Minute), 10)
		must(t, err)
		return len(ts)
	}, func() error {
		return b.InConfigTx(ctx, func(tx store.ConfigTx) error {
			ok, err := tx.DeleteTargetIfUnused(ctx, "t2")
			if err == nil && !ok {
				err = errors.New("t2 not deleted")
			}
			return err
		})
	})
	wait("deliveries", func() int {
		ds, err := a.LeaseDueDeliveries(ctx, "h", t0, t0.Add(time.Minute), 10)
		must(t, err)
		return len(ds)
	}, func() error {
		return b.InConfigTx(ctx, func(tx store.ConfigTx) error {
			_, err := tx.FailPendingDeliveries(ctx, "s1", "subscription removed")
			return err
		})
	})
	// The failing applied to pending rows only: the leased one stays in flight.
	d, err := a.GetDelivery(ctx, "d-s1")
	must(t, err)
	if d.Status != store.DeliveryInFlight || d.Attempts != 1 {
		t.Fatalf("leased delivery after the failing = %+v", d)
	}
}

// The fenced writes lock their row before the lease row, as lease
// transactions do. A fenced write by a holder that lost its lease to a
// takeover in progress waits for it, then writes nothing.
func TestFencedWritesWaitForATakeover(t *testing.T) {
	a, b := twoStores(t)
	seedStore(t, a)
	ctx := t.Context()
	addDueDeliveries(t, a, 1)
	until := t0.Add(time.Minute)
	ts, err := a.LeaseDueTargets(ctx, "old", t0, until, 1)
	must(t, err)
	ds, err := a.LeaseDueDeliveries(ctx, "old", t0, until, 1)
	must(t, err)
	if len(ts) != 1 || len(ds) != 1 {
		t.Fatalf("leased %d targets and %d deliveries", len(ts), len(ds))
	}
	later := until.Add(time.Minute)
	type write struct {
		ok  bool
		err error
	}
	for name, c := range map[string]struct {
		takeover func() int
		fenced   func() (bool, error)
	}{
		"target": {
			func() int {
				got, err := b.LeaseDueTargets(ctx, "new", later, later.Add(time.Minute), 1)
				must(t, err)
				return len(got)
			},
			func() (bool, error) {
				tg := ts[0]
				tg.NextCheckAt = later.Add(time.Hour)
				return a.UpdateTargetLeased(ctx, tg, store.TargetLeaseName(tg.ID), "old", until.Add(-time.Second))
			},
		},
		"delivery": {
			func() int {
				got, err := b.LeaseDueDeliveries(ctx, "new", later, later.Add(time.Minute), 1)
				must(t, err)
				return len(got)
			},
			func() (bool, error) {
				return a.FinishDelivery(ctx, store.DeliveryFinish{ID: ds[0].Delivery.ID, Attempt: ds[0].Delivery.Attempts, Status: store.DeliveryDelivered}, "old")
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			done := make(chan write, 1)
			b.afterLeaseSelect = func() {
				go func() {
					ok, err := c.fenced()
					done <- write{ok, err}
				}()
				select {
				case w := <-done:
					t.Errorf("the fenced write did not wait for the takeover: %+v", w)
				case <-time.After(200 * time.Millisecond):
				}
			}
			defer func() { b.afterLeaseSelect = nil }()
			if n := c.takeover(); n != 1 {
				t.Fatalf("took over %d rows, want 1", n)
			}
			w := <-done
			if w.err != nil || w.ok {
				t.Fatalf("fenced write after the takeover = %v, %v; want false, nil", w.ok, w.err)
			}
		})
	}
	tg, err := a.GetTarget(ctx, ts[0].ID)
	must(t, err)
	if !tg.NextCheckAt.Equal(t0) {
		t.Errorf("the stale holder's target write applied: next check %v", tg.NextCheckAt)
	}
	d, err := a.GetDelivery(ctx, ds[0].Delivery.ID)
	must(t, err)
	if d.Status != store.DeliveryInFlight || d.Attempts != 2 {
		t.Errorf("delivery after the takeover = %+v, want in flight at attempt 2", d)
	}
}

// Two instances deleting expired target leases at once each skip the rows the
// other holds: neither waits, and together they delete each row once.
func TestDeleteExpiredTargetLeasesSkipsHeldRows(t *testing.T) {
	a, b := twoStores(t)
	ctx := t.Context()
	for i := range 20 {
		ok, err := a.AcquireLease(ctx, store.TargetLeaseName(fmt.Sprint("t", i)), "h", t0, t0.Add(time.Minute))
		must(t, err)
		if !ok {
			t.Fatal("lease refused")
		}
	}
	tx, err := a.db.BeginTx(ctx, nil)
	must(t, err)
	defer func() { _ = tx.Rollback() }()
	// Hold five rows, as a renewal in progress would.
	_, err = tx.ExecContext(ctx, `SELECT 1 FROM lease WHERE name IN ('target:t0','target:t1','target:t2','target:t3','target:t4') FOR UPDATE`)
	must(t, err)
	wctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	n, err := b.DeleteExpiredTargetLeases(wctx, t0.Add(2*time.Minute))
	must(t, err)
	if n != 15 {
		t.Fatalf("deleted %d leases while 5 are held, want 15", n)
	}
	must(t, tx.Rollback())
	n, err = a.DeleteExpiredTargetLeases(ctx, t0.Add(2*time.Minute))
	must(t, err)
	if n != 5 {
		t.Fatalf("deleted %d leases after the release, want 5", n)
	}
}
