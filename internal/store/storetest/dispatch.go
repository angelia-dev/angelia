package storetest

import (
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// seedDeliveries writes event e1 with a due delivery for s1 and one for s2
// due a minute later.
func seedDeliveries(t *testing.T, s store.APIStore) {
	t.Helper()
	seed(t, s)
	d1 := pending("s1")
	d1.NextAttemptAt = t0
	d2 := pending("s2")
	d2.NextAttemptAt = t0.Add(time.Minute)
	must(t, s.AppendEvent(t.Context(), event("e1"), []store.Delivery{d1, d2}))
}

func leaseIDs(t *testing.T, s store.APIStore, holder string, now time.Time, limit int) []string {
	t.Helper()
	got, err := s.LeaseDueDeliveries(t.Context(), holder, now, now.Add(time.Minute), limit)
	if err != nil {
		t.Fatalf("LeaseDueDeliveries(%s, %v): %v", holder, now, err)
	}
	ids := make([]string, len(got))
	for i, ld := range got {
		ids[i] = ld.Delivery.ID
	}
	return ids
}

func testLeaseDueDeliveries(t *testing.T, open opener) {
	s := open(t)
	seedDeliveries(t, s)
	ctx := t.Context()

	if ok, err := s.HasDueDeliveries(ctx, t0.Add(-time.Second)); err != nil || ok {
		t.Fatalf("HasDueDeliveries before due = %v, %v", ok, err)
	}
	if ok, err := s.HasDueDeliveries(ctx, t0); err != nil || !ok {
		t.Fatalf("HasDueDeliveries at due = %v, %v", ok, err)
	}
	got, err := s.LeaseDueDeliveries(ctx, "a", t0, t0.Add(time.Minute), 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("lease at t0 = %+v, %v; want d-s1 only", got, err)
	}
	ld := got[0]
	if ld.Delivery.ID != "d-s1" || ld.Delivery.Status != store.DeliveryInFlight || ld.Delivery.Attempts != 1 {
		t.Errorf("leased delivery = %+v", ld.Delivery)
	}
	if ld.Event.ID != "e1" || string(ld.Event.Payload) != `{"v":1}` {
		t.Errorf("leased event = %+v", ld.Event)
	}
	if ld.Subscription == nil || ld.Subscription.ID != "s1" || ld.Sink == nil || ld.Sink.Kind != "webhook" {
		t.Errorf("leased subscription/sink = %+v / %+v", ld.Subscription, ld.Sink)
	}
	// Under a live lease the delivery is not due for anyone.
	if ids := leaseIDs(t, s, "b", t0.Add(59*time.Second), 10); len(ids) != 0 {
		t.Errorf("lease under a live lease = %v, want none", ids)
	}
	if ok, err := s.HasDueDeliveries(ctx, t0.Add(59*time.Second)); err != nil || ok {
		t.Errorf("HasDueDeliveries under a live lease = %v, %v", ok, err)
	}
	// Limit and order: oldest due first.
	if ids := leaseIDs(t, s, "b", t0.Add(2*time.Minute), 1); len(ids) != 1 || ids[0] != "d-s1" {
		t.Errorf("lease after expiry with limit 1 = %v, want [d-s1] (expired in-flight, oldest)", ids)
	}
	if ids, _ := s.LeaseDueDeliveries(ctx, "b", t0, t0.Add(time.Minute), 0); ids != nil {
		t.Errorf("limit 0 = %v", ids)
	}
	if _, err := s.LeaseDueDeliveries(ctx, "", t0, t0.Add(time.Minute), 1); err == nil {
		t.Error("empty holder accepted")
	}
	if _, err := s.LeaseDueDeliveries(ctx, "b", t0, t0, 1); err == nil {
		t.Error("until == now accepted")
	}
}

// A holder that crashed mid-delivery leaves the row in flight; once its
// lease expires another holder leases it again, and the crashed holder's
// late write is fenced off.
func testLeaseDueDeliveriesTakesOverExpired(t *testing.T, open opener) {
	s := open(t)
	seedDeliveries(t, s)
	ctx := t.Context()
	if ids := leaseIDs(t, s, "a", t0, 1); len(ids) != 1 {
		t.Fatalf("first lease = %v", ids)
	}
	later := t0.Add(time.Minute)
	got, err := s.LeaseDueDeliveries(ctx, "b", later, later.Add(time.Minute), 1)
	if err != nil || len(got) != 1 || got[0].Delivery.ID != "d-s1" || got[0].Delivery.Attempts != 2 {
		t.Fatalf("takeover = %+v, %v; want d-s1 at attempt 2", got, err)
	}
	ok, err := s.FinishDelivery(ctx, store.DeliveryFinish{ID: "d-s1", Attempt: 1, Status: store.DeliveryDelivered}, "a")
	if err != nil || ok {
		t.Fatalf("late finish by the old holder = %v, %v; want false", ok, err)
	}
	ok, err = s.FinishDelivery(ctx, store.DeliveryFinish{ID: "d-s1", Attempt: 2, Status: store.DeliveryDelivered}, "b")
	if err != nil || !ok {
		t.Fatalf("finish by the new holder = %v, %v", ok, err)
	}
	if ok, _ := s.AcquireLease(ctx, store.DeliveryLeaseName("d-s1"), "c", later, later.Add(time.Minute)); !ok {
		t.Error("finish did not release the lease")
	}
}

func testFinishDelivery(t *testing.T, open opener) {
	s := open(t)
	seedDeliveries(t, s)
	ctx := t.Context()
	leaseIDs(t, s, "a", t0.Add(time.Minute), 10)
	retry := t0.Add(time.Hour)
	for _, f := range []store.DeliveryFinish{
		{ID: "d-s1", Attempt: 1, Status: store.DeliveryPending, NextAttemptAt: retry, LastError: "boom"},
		{ID: "d-s2", Attempt: 1, Status: store.DeliveryDead, LastError: "gone"},
	} {
		if ok, err := s.FinishDelivery(ctx, f, "a"); err != nil || !ok {
			t.Fatalf("FinishDelivery(%+v) = %v, %v", f, ok, err)
		}
	}
	ds, err := s.ListDeliveries(ctx, "e1")
	must(t, err)
	if d := ds[0]; d.Status != store.DeliveryPending || !d.NextAttemptAt.Equal(retry) || d.LastError != "boom" || d.Attempts != 1 {
		t.Errorf("retried delivery = %+v", d)
	}
	if d := ds[1]; d.Status != store.DeliveryDead || !d.NextAttemptAt.IsZero() || d.LastError != "gone" {
		t.Errorf("dead delivery = %+v", d)
	}
	// Not in flight any more: a second finish writes nothing.
	if ok, err := s.FinishDelivery(ctx, store.DeliveryFinish{ID: "d-s2", Attempt: 1, Status: store.DeliveryDelivered}, "a"); err != nil || ok {
		t.Errorf("finish of a dead delivery = %v, %v", ok, err)
	}
	// Retried: due again at its next attempt, and success clears the error.
	if ids := leaseIDs(t, s, "a", retry, 10); len(ids) != 1 || ids[0] != "d-s1" {
		t.Fatalf("lease at retry time = %v", ids)
	}
	must(t, okErr(s.FinishDelivery(ctx, store.DeliveryFinish{ID: "d-s1", Attempt: 2, Status: store.DeliveryDelivered}, "a")))
	ds, _ = s.ListDeliveries(ctx, "e1")
	if d := ds[0]; d.Status != store.DeliveryDelivered || d.LastError != "" || d.Attempts != 2 {
		t.Errorf("delivered = %+v", d)
	}
	for _, bad := range []store.DeliveryFinish{
		{ID: "d-s1", Status: store.DeliveryPending},
		{ID: "d-s1", Status: store.DeliveryInFlight},
	} {
		if _, err := s.FinishDelivery(ctx, bad, "a"); err == nil {
			t.Errorf("FinishDelivery(%+v) accepted", bad)
		}
	}
}

func testLeaseDueDeliveriesDeletedSubscription(t *testing.T, open opener) {
	s := open(t)
	seedDeliveries(t, s)
	ctx := t.Context()
	must(t, s.DeleteSubscription(ctx, "s1"))
	got, err := s.LeaseDueDeliveries(ctx, "a", t0, t0.Add(time.Minute), 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("lease = %+v, %v", got, err)
	}
	if got[0].Subscription != nil || got[0].Sink != nil {
		t.Errorf("deleted subscription leased with %+v / %+v", got[0].Subscription, got[0].Sink)
	}
}

// The same holder's stale attempt (its lease expired and it leased the
// delivery again) cannot finish the new attempt.
func testFinishDeliveryFencesStaleAttemptOfSameHolder(t *testing.T, open opener) {
	s := open(t)
	seedDeliveries(t, s)
	ctx := t.Context()
	leaseIDs(t, s, "a", t0, 1)
	later := t0.Add(time.Minute)
	if ids := leaseIDs(t, s, "a", later, 1); len(ids) != 1 {
		t.Fatalf("re-lease by the same holder = %v", ids)
	}
	ok, err := s.FinishDelivery(ctx, store.DeliveryFinish{ID: "d-s1", Attempt: 1, Status: store.DeliveryDelivered}, "a")
	if err != nil || ok {
		t.Fatalf("stale attempt 1 finish = %v, %v; want false", ok, err)
	}
	ok, err = s.FinishDelivery(ctx, store.DeliveryFinish{ID: "d-s1", Attempt: 2, Status: store.DeliveryDelivered}, "a")
	if err != nil || !ok {
		t.Fatalf("attempt 2 finish = %v, %v", ok, err)
	}
}

// deliverAll leases every due delivery and finishes each as delivered, the
// way the dispatcher ends a successful attempt.
func deliverAll(t *testing.T, s store.APIStore) {
	t.Helper()
	ctx := t.Context()
	got, err := s.LeaseDueDeliveries(ctx, "deliverAll", t0, t0.Add(time.Minute), 1000)
	must(t, err)
	for _, ld := range got {
		ok, err := s.FinishDelivery(ctx, store.DeliveryFinish{ID: ld.Delivery.ID, Attempt: ld.Delivery.Attempts, Status: store.DeliveryDelivered}, "deliverAll")
		must(t, err)
		if !ok {
			t.Fatalf("FinishDelivery(%s) was fenced out", ld.Delivery.ID)
		}
	}
}

func okErr(ok bool, err error) error {
	if err == nil && !ok {
		return store.ErrNotFound
	}
	return err
}
