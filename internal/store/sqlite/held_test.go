package sqlite

import (
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

func heldDelivery(id, sub string, until time.Time) store.Delivery {
	return store.Delivery{
		ID: id, SubscriptionID: sub, Status: store.DeliveryPending,
		Hold: &store.Hold{Until: until, StartedAt: t0, WatchCooldown: time.Hour, Prerelease: true, WatchID: "w1", Raw: "1.2.0"},
	}
}

// AC: a cooldown held by a subscription survives a restart (ADR 0014): the
// delivery is a row, still held after reopening, and due when its time comes.
func TestHeldDeliverySurvivesRestart(t *testing.T) {
	s, path := openTest(t)
	seed(t, s)
	ctx := t.Context()
	until := t0.Add(72 * time.Hour)
	must(t, s.AppendEvent(ctx, event("e1"), []store.Delivery{heldDelivery("d1", "s1", until), pending("s2")}))
	must(t, s.Close())

	s2, err := Open(ctx, path, discard())
	must(t, err)
	t.Cleanup(func() { _ = s2.Close() })
	ds, err := s2.ListDeliveries(ctx, "e1")
	must(t, err)
	byID := map[string]store.Delivery{}
	for _, d := range ds {
		byID[d.ID] = d
	}
	h := byID["d1"]
	if h.Status != store.DeliveryPending || !h.NextAttemptAt.Equal(until) || h.Hold == nil ||
		!h.Hold.Until.Equal(until) || !h.Hold.StartedAt.Equal(t0) || h.Hold.WatchCooldown != time.Hour ||
		!h.Hold.Prerelease || h.Hold.WatchID != "w1" || h.Hold.Raw != "1.2.0" {
		t.Fatalf("held delivery after restart = %+v (hold %+v)", h, h.Hold)
	}
	if byID["d-s2"].Hold != nil {
		t.Fatalf("a plain delivery must not be held: %+v", byID["d-s2"])
	}

	// Held at t0+1h; no longer held (so due) once its time has passed.
	held, err := listHeld(t, s2, "s1", t0.Add(time.Hour))
	must(t, err)
	if len(held) != 1 || held[0].ID != "d1" {
		t.Fatalf("held before the end = %+v", held)
	}
	held, err = listHeld(t, s2, "s1", until)
	must(t, err)
	if len(held) != 0 {
		t.Fatalf("held at the end = %+v", held)
	}
}

func TestHeldDeliveryValidation(t *testing.T) {
	s, _ := openTest(t)
	seed(t, s)
	d := heldDelivery("d1", "s1", t0)
	d.Hold.Until = time.Time{}
	wantErr(t, s.AppendEvent(t.Context(), event("e1"), []store.Delivery{d}), store.ErrInvalid, "hold without end")
	d = heldDelivery("d1", "s1", t0)
	d.Status = store.DeliveryDelivered
	wantErr(t, s.AppendEvent(t.Context(), event("e2"), []store.Delivery{d}), store.ErrInvalid, "hold on a delivered row")
}

func TestHeldDeliveryOperations(t *testing.T) {
	s, _ := openTest(t)
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
		must(t, tx.RescheduleHeldDelivery(ctx, "d2", t0.Add(time.Hour)))
		must(t, tx.FailHeldDelivery(ctx, "d3", "no longer wanted"))
		// Not held any more, or not held at all: ErrNotFound.
		wantErr(t, tx.ReleaseHeldDelivery(ctx, "d1", t0), store.ErrNotFound, "release twice")
		wantErr(t, tx.RescheduleHeldDelivery(ctx, "d-s2", t0), store.ErrNotFound, "reschedule a plain delivery")
		wantErr(t, tx.FailHeldDelivery(ctx, "d3", "x"), store.ErrNotFound, "fail a dead delivery")
		return nil
	}))
	if d := get("e1", "d1"); d.Hold != nil || !d.NextAttemptAt.Equal(t0) || d.Status != store.DeliveryPending {
		t.Fatalf("released = %+v", d)
	}
	if d := get("e2", "d2"); d.Hold == nil || !d.Hold.Until.Equal(t0.Add(time.Hour)) || !d.NextAttemptAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("rescheduled = %+v", d)
	}
	if d := get("e2", "d3"); d.Status != store.DeliveryDead || d.LastError != "no longer wanted" || !d.NextAttemptAt.IsZero() {
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

func TestSubscriptionPolicyRoundTrip(t *testing.T) {
	s, _ := openTest(t)
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

func listHeld(t *testing.T, s *Store, sub string, now time.Time) ([]store.Delivery, error) {
	t.Helper()
	var out []store.Delivery
	err := s.InConfigTx(t.Context(), func(tx store.ConfigTx) error {
		var err error
		out, err = tx.ListHeldDeliveries(t.Context(), sub, now)
		return err
	})
	return out, err
}
