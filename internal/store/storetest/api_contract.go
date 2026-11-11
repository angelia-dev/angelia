package storetest

import (
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// Store changes behind the OpenAPI v1 contract (story #185, spec docs/api.md
// decisions 2, 10 and 12).

// Decision 2: a name is unique per resource type whoever owns the row.
func testNamesUniqueAcrossOwners(t *testing.T, open opener) {
	s := open(t)
	ctx := t.Context()
	must(t, s.CreateTarget(ctx, store.Target{ID: "t1", Kind: "pypi", Coordinates: map[string]string{"name": "a"}, NextCheckAt: t0}))
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w1", TargetID: "t1", Name: "a", ManagedBy: store.ManagedByFile}))
	wantErr(t, s.CreateWatch(ctx, store.Watch{ID: "w2", TargetID: "t1", Name: "a", ManagedBy: store.ManagedByAPI}), store.ErrConflict, "watch name across owners")
	must(t, s.CreateSink(ctx, store.Sink{ID: "k1", Kind: "webhook", Name: "k", ManagedBy: store.ManagedByAPI}))
	wantErr(t, s.CreateSink(ctx, store.Sink{ID: "k2", Kind: "webhook", Name: "k", ManagedBy: store.ManagedByFile}), store.ErrConflict, "sink name across owners")
	must(t, s.CreateSubscription(ctx, store.Subscription{ID: "s1", SinkID: "k1", Name: "s", ManagedBy: store.ManagedByFile}))
	wantErr(t, s.CreateSubscription(ctx, store.Subscription{ID: "s2", SinkID: "k1", Name: "s"}), store.ErrConflict, "subscription name across owners")
	// The same name in two resource types is fine, and unnamed rows never collide.
	must(t, s.CreateSink(ctx, store.Sink{ID: "k3", Kind: "webhook", Name: "a"}))
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w3", TargetID: "t1"}))
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w4", TargetID: "t1"}))
}

// Decision 10: the interval a watch asked for is stored on the watch and read
// back by every read path. Zero stays unset.
func testWatchIntervalRoundTrip(t *testing.T, open opener) {
	s := open(t)
	ctx := t.Context()
	must(t, s.CreateTarget(ctx, store.Target{ID: "t1", Kind: "pypi", Coordinates: map[string]string{"name": "a"}, NextCheckAt: t0, Interval: 5 * time.Minute}))
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w1", TargetID: "t1", Name: "a", ManagedBy: store.ManagedByFile, Interval: time.Hour}))
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w2", TargetID: "t1"}))

	w, err := s.GetWatch(ctx, "w1")
	must(t, err)
	if w.Interval != time.Hour {
		t.Fatalf("GetWatch interval = %s, want 1h", w.Interval)
	}
	if w, _ = s.GetWatch(ctx, "w2"); w.Interval != 0 {
		t.Fatalf("unset interval = %s", w.Interval)
	}
	ws, err := s.ListWatches(ctx, "t1")
	must(t, err)
	if len(ws) != 2 || ws[0].Interval != time.Hour || ws[1].Interval != 0 {
		t.Fatalf("ListWatches: %+v", ws)
	}
	must(t, s.InConfigTx(ctx, func(tx store.ConfigTx) error {
		ws, err := tx.ListManagedWatches(ctx, store.ManagedByFile)
		if err != nil || len(ws) != 1 || ws[0].Interval != time.Hour {
			t.Fatalf("ListManagedWatches: %+v %v", ws, err)
		}
		return tx.UpdateWatch(ctx, store.Watch{ID: "w1", TargetID: "t1", Interval: 10 * time.Minute})
	}))
	if w, _ = s.GetWatch(ctx, "w1"); w.Interval != 10*time.Minute {
		t.Fatalf("updated interval = %s", w.Interval)
	}
	// The target keeps its own (effective) interval.
	tg, err := s.GetTarget(ctx, "t1")
	must(t, err)
	if tg.Interval != 5*time.Minute {
		t.Fatalf("target interval = %s", tg.Interval)
	}
}

// Decision 12: the highest pruned seq starts at 0, follows every prune, and
// never goes down.
func testHighestPrunedSeq(t *testing.T, open opener) {
	s := open(t)
	seed(t, s)
	ctx := t.Context()
	got := func() int64 {
		t.Helper()
		n, err := s.HighestPrunedSeq(ctx)
		must(t, err)
		return n
	}
	if n := got(); n != 0 {
		t.Fatalf("fresh store = %d, want 0", n)
	}
	must(t, s.AppendEvent(ctx, eventAt("e1", pruneOld), nil))
	must(t, s.AppendEvent(ctx, eventAt("e2", pruneOld), []store.Delivery{deliveryWith("d2", "e2", "s1", store.DeliveryPending)}))
	must(t, s.AppendEvent(ctx, eventAt("e3", pruneOld), nil))
	must(t, s.AppendEvent(ctx, eventAt("e4", t0), nil))
	evs, err := s.ListEventsAfter(ctx, 0, 10)
	must(t, err)
	seq := map[string]int64{}
	for _, e := range evs {
		seq[e.ID] = e.Seq
	}

	_, err = s.PruneHistory(ctx, pruneBefore, 1)
	must(t, err)
	// e1 and e3 are pruned; e2 waits for its pending delivery.
	if n := got(); n != seq["e3"] {
		t.Fatalf("after prune = %d, want seq of e3 (%d)", n, seq["e3"])
	}
	// Pruning the older e2 later does not lower it.
	deliverAll(t, s)
	res, err := s.PruneHistory(ctx, pruneBefore, 0)
	must(t, err)
	if res.Events != 1 {
		t.Fatalf("second prune = %+v, want e2", res)
	}
	if n := got(); n != seq["e3"] {
		t.Fatalf("after pruning older e2 = %d, want %d", n, seq["e3"])
	}
	// A prune that deletes nothing leaves it alone.
	_, err = s.PruneHistory(ctx, pruneBefore, 0)
	must(t, err)
	if n := got(); n != seq["e3"] {
		t.Fatalf("after empty prune = %d", n)
	}
}

// RecomputeTargetInterval sets the target to the smallest interval among its
// watches, ignoring watches without one, and pulls the next check forward
// when the interval goes down.
func testRecomputeTargetInterval(t *testing.T, open opener) {
	s := open(t)
	ctx := t.Context()
	next := t0.Add(2 * time.Hour)
	must(t, s.CreateTarget(ctx, store.Target{ID: "t1", Kind: "pypi", Coordinates: map[string]string{"name": "a"}, NextCheckAt: next, Interval: 2 * time.Hour}))
	recompute := func() (time.Duration, bool) {
		t.Helper()
		var d time.Duration
		var changed bool
		must(t, s.InConfigTx(ctx, func(tx store.ConfigTx) (err error) {
			d, changed, err = tx.RecomputeTargetInterval(ctx, "t1", t0)
			return err
		}))
		return d, changed
	}
	// No watch has an interval: the target keeps its own.
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w0", TargetID: "t1"}))
	if d, changed := recompute(); d != 2*time.Hour || changed {
		t.Fatalf("no intervals: %s %v", d, changed)
	}
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w1", TargetID: "t1", Interval: time.Hour}))
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w2", TargetID: "t1", Name: "f", ManagedBy: store.ManagedByFile, Interval: 30 * time.Minute}))
	if d, changed := recompute(); d != 30*time.Minute || !changed {
		t.Fatalf("lowered: %s %v", d, changed)
	}
	tg, err := s.GetTarget(ctx, "t1")
	must(t, err)
	if tg.Interval != 30*time.Minute || !tg.NextCheckAt.Equal(t0.Add(30*time.Minute)) {
		t.Fatalf("target: %+v", tg)
	}
	if d, changed := recompute(); d != 30*time.Minute || changed {
		t.Fatalf("again: %s %v", d, changed)
	}
	// Raising it leaves the next check alone.
	must(t, s.DeleteWatch(ctx, "w2"))
	if d, changed := recompute(); d != time.Hour || !changed {
		t.Fatalf("raised: %s %v", d, changed)
	}
	if tg, _ = s.GetTarget(ctx, "t1"); !tg.NextCheckAt.Equal(t0.Add(30 * time.Minute)) {
		t.Fatalf("next check moved: %s", tg.NextCheckAt)
	}
	wantErr(t, s.InConfigTx(ctx, func(tx store.ConfigTx) error {
		_, _, err := tx.RecomputeTargetInterval(ctx, "nope", t0)
		return err
	}), store.ErrNotFound, "missing target")
}
