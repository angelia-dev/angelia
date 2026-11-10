package storetest

import (
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

func heldDelivery(id, sub string, until time.Time) store.Delivery {
	return store.Delivery{
		ID: id, SubscriptionID: sub, Status: store.DeliveryPending,
		Hold: &store.Hold{Until: until, StartedAt: t0, WatchCooldown: time.Hour, Prerelease: true, WatchID: "w1", Raw: "1.2.0", Input: []byte(`{"bump":"patch"}`)},
	}
}

func testHeldDeliveryValidation(t *testing.T, open opener) {
	s := open(t)
	seed(t, s)
	d := heldDelivery("d1", "s1", t0)
	d.Hold.Until = time.Time{}
	wantErr(t, s.AppendEvent(t.Context(), event("e1"), []store.Delivery{d}), store.ErrInvalid, "hold without end")
	d = heldDelivery("d1", "s1", t0)
	d.Status = store.DeliveryDelivered
	wantErr(t, s.AppendEvent(t.Context(), event("e2"), []store.Delivery{d}), store.ErrInvalid, "hold on a delivered row")
}

func testHeldDeliveryOperations(t *testing.T, open opener) {
	s := open(t)
	seed(t, s)
	ctx := t.Context()
	until := t0.Add(72 * time.Hour)
	must(t, s.AppendEvent(ctx, event("e1"), []store.Delivery{heldDelivery("d1", "s1", until), pending("s2")}))
	must(t, s.AppendEvent(ctx, event("e2"), []store.Delivery{heldDelivery("d2", "s1", until), heldDelivery("d3", "s2", until)}))
	get := func(ev, id string) store.Delivery {
		t.Helper()
		ds, err := s.ListDeliveries(ctx, ev)
		must(t, err)
		for _, d := range ds {
			if d.ID == id {
				return d
			}
		}
		t.Fatalf("no delivery %s", id)
		return store.Delivery{}
	}
	must(t, s.InConfigTx(ctx, func(tx store.ConfigTx) error {
		must(t, tx.ReleaseHeldDelivery(ctx, "d1", t0))
		must(t, tx.RescheduleHeldDelivery(ctx, "d2", t0.Add(time.Hour), t0))
		must(t, tx.FailHeldDelivery(ctx, "d3", "no longer wanted", t0))
		// Not held any more, or not held at all: ErrNotFound.
		wantErr(t, tx.ReleaseHeldDelivery(ctx, "d1", t0), store.ErrNotFound, "release twice")
		wantErr(t, tx.RescheduleHeldDelivery(ctx, "d-s2", t0, t0), store.ErrNotFound, "reschedule a plain delivery")
		wantErr(t, tx.FailHeldDelivery(ctx, "d3", "x", t0), store.ErrNotFound, "fail a dead delivery")
		// A hold that has ended by the clock is not touched.
		wantErr(t, tx.ReleaseHeldDelivery(ctx, "d2", until), store.ErrNotFound, "release at the end of the hold")
		wantErr(t, tx.RescheduleHeldDelivery(ctx, "d2", until, until.Add(time.Hour)), store.ErrNotFound, "reschedule after the end")
		wantErr(t, tx.FailHeldDelivery(ctx, "d2", "x", until.Add(time.Hour)), store.ErrNotFound, "fail after the end")
		return nil
	}))
	if d := get("e1", "d1"); d.Hold == nil || !d.Hold.Until.IsZero() || !d.NextAttemptAt.Equal(t0) || d.Status != store.DeliveryPending {
		t.Fatalf("released = %+v", d)
	}
	if d := get("e2", "d2"); d.Hold == nil || !d.Hold.Until.Equal(t0.Add(time.Hour)) || !d.NextAttemptAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("rescheduled = %+v", d)
	}
	if d := get("e2", "d3"); d.Status != store.DeliveryDead || d.LastError != "no longer wanted" || !d.NextAttemptAt.IsZero() || d.Hold == nil || !d.Hold.Until.IsZero() {
		t.Fatalf("failed = %+v", d)
	}

	// FailPendingDeliveries: every pending delivery of one subscription, held or not.
	var n int64
	must(t, s.InConfigTx(ctx, func(tx store.ConfigTx) error {
		var err error
		n, err = tx.FailPendingDeliveries(ctx, "s1", "gone")
		return err
	}))
	if n != 2 { // d1 (released, still pending) and d2
		t.Fatalf("FailPendingDeliveries = %d, want 2", n)
	}
	if d := get("e1", "d-s2"); d.Status != store.DeliveryPending {
		t.Fatalf("another subscription's delivery changed: %+v", d)
	}
}

func testSubscriptionPolicyRoundTrip(t *testing.T, open opener) {
	s := open(t)
	seed(t, s)
	ctx := t.Context()
	must(t, s.InConfigTx(ctx, func(tx store.ConfigTx) error {
		must(t, tx.CreateSubscription(ctx, store.Subscription{
			ID: "s3", SinkID: "k1", Name: "n", ManagedBy: store.ManagedByFile, Policy: []byte(`{"cooldown":"72h"}`),
		}))
		must(t, tx.UpdateSubscription(ctx, store.Subscription{ID: "s3", SinkID: "k1", Policy: []byte(`{"cooldown":"1h"}`)}))
		got, err := tx.ListManagedSubscriptions(ctx, store.ManagedByFile)
		must(t, err)
		if len(got) != 1 || string(got[0].Policy) != `{"cooldown":"1h"}` {
			t.Fatalf("policy = %+v", got)
		}
		return nil
	}))
}
