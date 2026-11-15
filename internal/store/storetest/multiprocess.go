package storetest

import (
	"cmp"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// Cases of story #86: two store values on one database stand in for two
// processes, on every engine.

// testLeasesUnderLoadAcrossStoreInstances: two store instances, each with 8
// goroutines leasing in a loop, over 1,000 due targets and 5,000 due
// deliveries. No lease expires during the test, so every id is returned to
// exactly one holder, and together the holders lease every due row.
func testLeasesUnderLoadAcrossStoreInstances(t *testing.T, e Engine) {
	if !e.ScaleOut {
		t.Skip("the engine runs in one process")
	}
	first := e.Open(t)
	second := e.OpenSecond(t, first)
	seed(t, first) // its target t1 is due too
	ctx := t.Context()
	const targets, deliveries, fanout, perStore, limit = 1000, 5000, 100, 8, 16
	for i := range targets {
		must(t, first.CreateTarget(ctx, store.Target{
			ID: fmt.Sprintf("tl%04d", i), Kind: "pypi", Coordinates: map[string]string{"name": fmt.Sprint("load", i)}, NextCheckAt: t0,
		}))
	}
	for ev := range deliveries / fanout {
		ds := make([]store.Delivery, fanout)
		for i := range ds {
			ds[i] = store.Delivery{ID: fmt.Sprintf("dl%02d-%03d", ev, i), SubscriptionID: fmt.Sprintf("s%03d", i), Status: store.DeliveryPending, NextAttemptAt: t0}
		}
		must(t, first.AppendEvent(ctx, event(fmt.Sprintf("el%02d", ev)), ds))
	}

	var mu sync.Mutex
	gotTargets, gotDeliveries := map[string]string{}, map[string]string{}
	claim := func(into map[string]string, id, holder string) {
		mu.Lock()
		defer mu.Unlock()
		if prev, ok := into[id]; ok {
			t.Errorf("%s leased by %s and by %s", id, prev, holder)
		}
		into[id] = holder
	}
	until := t0.Add(time.Hour)
	var wg sync.WaitGroup
	for i, s := range []store.Store{first, second} {
		for g := range perStore {
			holder := fmt.Sprintf("store%d-g%d", i, g)
			wg.Go(func() {
				for {
					ts, err := s.LeaseDueTargets(ctx, holder, t0, until, limit)
					if err != nil {
						t.Error(err)
						return
					}
					ds, err := s.LeaseDueDeliveries(ctx, holder, t0, until, limit)
					if err != nil {
						t.Error(err)
						return
					}
					for _, x := range ts {
						claim(gotTargets, x.ID, holder)
					}
					for _, x := range ds {
						claim(gotDeliveries, x.Delivery.ID, holder)
					}
					if len(ts)+len(ds) == 0 {
						return
					}
				}
			})
		}
	}
	wg.Wait()
	if len(gotTargets) != targets+1 || len(gotDeliveries) != deliveries {
		t.Fatalf("leased %d targets and %d deliveries, want %d and %d", len(gotTargets), len(gotDeliveries), targets+1, deliveries)
	}
	if hasT, err := first.HasDueTargets(ctx, t0); err != nil || hasT {
		t.Errorf("due targets left: %v %v", hasT, err)
	}
	if hasD, err := first.HasDueDeliveries(ctx, t0); err != nil || hasD {
		t.Errorf("due deliveries left: %v %v", hasD, err)
	}
}

// testLeaseTakeoverAcrossStoreInstances: a holder whose lease expired (a
// crashed process) loses the target and the delivery to a holder on another
// instance; its fenced writes then write nothing, and the new holder's do.
func testLeaseTakeoverAcrossStoreInstances(t *testing.T, e Engine) {
	old := e.Open(t)
	other := e.OpenSecond(t, old)
	seed(t, old)
	ctx := t.Context()
	must(t, old.AppendEvent(ctx, event("e1"), []store.Delivery{{ID: "d1", SubscriptionID: "s1", Status: store.DeliveryPending}}))
	until := t0.Add(time.Minute)
	ts, err := old.LeaseDueTargets(ctx, "old", t0, until, 10)
	must(t, err)
	ds, err := old.LeaseDueDeliveries(ctx, "old", t0, until, 10)
	must(t, err)
	if len(ts) != 1 || len(ds) != 1 {
		t.Fatalf("first lease: %d targets, %d deliveries", len(ts), len(ds))
	}
	later := until.Add(time.Second)
	nts, err := other.LeaseDueTargets(ctx, "new", later, later.Add(time.Minute), 10)
	must(t, err)
	nds, err := other.LeaseDueDeliveries(ctx, "new", later, later.Add(time.Minute), 10)
	must(t, err)
	if len(nts) != 1 || nts[0].ID != "t1" || len(nds) != 1 || nds[0].Delivery.ID != "d1" || nds[0].Delivery.Attempts != 2 {
		t.Fatalf("takeover: %+v, %+v", nts, nds)
	}

	// The old holder's clock still says its lease is live.
	tg := ts[0]
	tg.NextCheckAt = t0.Add(time.Hour)
	if ok, err := old.UpdateTargetLeased(ctx, tg, store.TargetLeaseName("t1"), "old", until.Add(-time.Second)); err != nil || ok {
		t.Fatalf("UpdateTargetLeased by the old holder = %v, %v; want false", ok, err)
	}
	if ok, err := old.FinishDelivery(ctx, store.DeliveryFinish{ID: "d1", Attempt: 1, Status: store.DeliveryDelivered}, "old"); err != nil || ok {
		t.Fatalf("FinishDelivery by the old holder = %v, %v; want false", ok, err)
	}
	if got, err := other.GetTarget(ctx, "t1"); err != nil || !got.NextCheckAt.Equal(t0) {
		t.Fatalf("target after the fenced write: %+v, %v", got, err)
	}
	if d, err := other.GetDelivery(ctx, "d1"); err != nil || d.Status != store.DeliveryInFlight || d.Attempts != 2 {
		t.Fatalf("delivery after the fenced finish: %+v, %v", d, err)
	}

	tg.NextCheckAt = later.Add(time.Hour)
	if ok, err := other.UpdateTargetLeased(ctx, tg, store.TargetLeaseName("t1"), "new", later); err != nil || !ok {
		t.Fatalf("UpdateTargetLeased by the new holder = %v, %v", ok, err)
	}
	if ok, err := other.FinishDelivery(ctx, store.DeliveryFinish{ID: "d1", Attempt: 2, Status: store.DeliveryDelivered}, "new"); err != nil || !ok {
		t.Fatalf("FinishDelivery by the new holder = %v, %v", ok, err)
	}
}

// testDeleteExpiredTargetLeasesConcurrent: instances deleting expired target
// leases at the same time do no harm. Each expired lease is deleted once, the
// live ones and the other leases stay.
func testDeleteExpiredTargetLeasesConcurrent(t *testing.T, e Engine) {
	first := e.Open(t)
	second := e.OpenSecond(t, first)
	ctx := t.Context()
	const expired, live = 300, 50
	take := func(name string, until time.Time) {
		t.Helper()
		if ok, err := first.AcquireLease(ctx, name, "h", t0, until); err != nil || !ok {
			t.Fatalf("acquire %s: %v %v", name, ok, err)
		}
	}
	for i := range expired {
		take(store.TargetLeaseName(fmt.Sprint("old", i)), t0.Add(time.Minute))
	}
	for i := range live {
		take(store.TargetLeaseName(fmt.Sprint("live", i)), t0.Add(time.Hour))
	}
	take("compactor", t0.Add(time.Minute))

	var deleted atomic.Int64
	var wg sync.WaitGroup
	for _, s := range []store.Store{first, second, first, second} {
		wg.Go(func() {
			n, err := s.DeleteExpiredTargetLeases(ctx, t0.Add(2*time.Minute))
			if err != nil {
				t.Error(err)
			}
			deleted.Add(n)
		})
	}
	wg.Wait()
	if n := deleted.Load(); n != expired {
		t.Fatalf("deleted %d leases in all, want %d", n, expired)
	}
	for _, name := range []string{store.TargetLeaseName("live0"), store.TargetLeaseName(fmt.Sprint("live", live-1)), "compactor"} {
		if ok, err := second.AcquireLease(ctx, name, "other", t0, t0.Add(time.Second)); err != nil || ok {
			t.Errorf("%s was deleted: %v %v", name, ok, err)
		}
	}
}

// testSubscriptionGenerationAcrossStoreInstances: a subscription written
// through one instance moves the generation another instance reads, so its
// matcher cache (event.MatcherCache) reloads.
func testSubscriptionGenerationAcrossStoreInstances(t *testing.T, e Engine) {
	a := e.Open(t)
	b := e.OpenSecond(t, a)
	seed(t, a)
	ctx := t.Context()
	gen := func() int64 {
		t.Helper()
		g, err := b.SubscriptionGeneration(ctx)
		must(t, err)
		return g
	}
	g0 := gen()
	must(t, a.CreateSubscription(ctx, store.Subscription{ID: "s9", SinkID: "k1"}))
	g1 := gen()
	if g1 == g0 {
		t.Fatal("a subscription created on the other instance did not move the generation")
	}
	subs, err := b.ListSubscriptions(ctx)
	must(t, err)
	if !slices.ContainsFunc(subs, func(s store.Subscription) bool { return s.ID == "s9" }) {
		t.Fatal("the other instance does not list the new subscription")
	}
	must(t, a.InConfigTx(ctx, func(tx store.ConfigTx) error { return tx.DeleteSubscription(ctx, "s9") }))
	if g2 := gen(); g2 == g1 {
		t.Fatal("a deletion on the other instance did not move the generation")
	}
}

// testLeaseDueDeliveriesKeepsTheOrder is the order test of story #235
// (sqlite's TestListLeasableDueDeliveriesKeepsTheOrder) through the Store
// interface, so that it runs on every engine: on a mix of statuses, NULL and
// tied next_attempt_at values and live, expired and missing leases, each
// LeaseDueDeliveries call returns the due rows a reference model picks, in its
// order: next_attempt_at NULLS FIRST, then id.
func testLeaseDueDeliveriesKeepsTheOrder(t *testing.T, open opener) {
	s := open(t)
	seed(t, s)
	ctx := t.Context()
	r := rand.New(rand.NewPCG(235, 1)) //nolint:gosec // reproducible test data, not crypto
	statuses := []string{store.DeliveryPending, store.DeliveryPending, store.DeliveryInFlight, store.DeliveryDelivered, store.DeliveryDead}
	type row struct {
		id, status string
		next       time.Time // zero: NULL
		leaseUntil time.Time // zero: no lease
	}
	const n = 400
	model := make([]*row, 0, n)
	ds := make([]store.Delivery, 0, n)
	for i := range n {
		w := &row{id: fmt.Sprintf("d%03d", r.IntN(1000)*1000+i), status: statuses[r.IntN(len(statuses))]}
		if r.IntN(4) > 0 { // NULL in one row out of four, else one of 11 shared minutes
			w.next = t0.Add(time.Duration(r.IntN(11)-5) * time.Minute)
		}
		if k := r.IntN(3); k > 0 { // a lease ending 4 minutes before or after t0
			w.leaseUntil = t0.Add(time.Duration(8*k-12) * time.Minute)
		}
		model = append(model, w)
		ds = append(ds, store.Delivery{ID: w.id, SubscriptionID: fmt.Sprintf("s%03d", i), Status: w.status, NextAttemptAt: w.next})
	}
	must(t, s.AppendEvent(ctx, event("e1"), ds))
	for _, w := range model {
		if !w.leaseUntil.IsZero() {
			ok, err := s.AcquireLease(ctx, store.DeliveryLeaseName(w.id), "x", t0.Add(-time.Hour), w.leaseUntil)
			if err != nil || !ok {
				t.Fatalf("lease of %s: %v %v", w.id, ok, err)
			}
		}
	}
	// due is the lease query of the model: what ListLeasableDueDeliveries
	// returned before story #232, one ORDER BY over every due row.
	due := func(now time.Time, limit int) []*row {
		var out []*row
		for _, w := range model {
			pendingDue := w.status == store.DeliveryPending && (w.next.IsZero() || !w.next.After(now))
			if (pendingDue || w.status == store.DeliveryInFlight) && !w.leaseUntil.After(now) {
				out = append(out, w)
			}
		}
		slices.SortFunc(out, func(a, b *row) int {
			switch {
			case a.next.IsZero() && !b.next.IsZero():
				return -1
			case !a.next.IsZero() && b.next.IsZero():
				return 1
			}
			return cmp.Or(a.next.Compare(b.next), cmp.Compare(a.id, b.id))
		})
		return out[:min(limit, len(out))]
	}
	nonEmpty := 0
	for _, now := range []time.Time{t0.Add(-10 * time.Minute), t0.Add(-4 * time.Minute), t0, t0.Add(3 * time.Minute), t0.Add(10 * time.Minute)} {
		for _, limit := range []int{1, 2, 7, 16, 64, n} {
			want := due(now, limit)
			wantIDs := make([]string, len(want))
			for i, w := range want {
				wantIDs[i] = w.id
			}
			// A lease of 1 ms: live for the other limits at this now, expired
			// at the next now.
			until := now.Add(time.Millisecond)
			leased, err := s.LeaseDueDeliveries(ctx, "h", now, until, limit)
			must(t, err)
			got := make([]string, len(leased))
			for i, ld := range leased {
				got[i] = ld.Delivery.ID
			}
			if !slices.Equal(got, wantIDs) {
				t.Fatalf("now %v, limit %d:\n got  %v\n want %v", now.Sub(t0), limit, got, wantIDs)
			}
			for _, w := range want {
				w.status, w.leaseUntil = store.DeliveryInFlight, until
			}
			if len(want) > 0 {
				nonEmpty++
			}
		}
	}
	if nonEmpty < 20 {
		t.Fatalf("only %d of 30 calls had due rows; the data does not exercise the query", nonEmpty)
	}
}
