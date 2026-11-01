package watchfile

import (
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// AC: editing a subscription decides held deliveries again with the new policy
// and leaves queued ones alone (ADR 0014 section 3).
func TestEditingSubscriptionReevaluatesHeldDeliveries(t *testing.T) {
	st := openStore(t)
	ctx := t.Context()
	mustApply(t, st, subDoc("{cooldown: 72h}"))
	var subID string
	if err := st.InConfigTx(ctx, func(tx store.ConfigTx) error {
		subs, err := tx.ListManagedSubscriptions(ctx, store.ManagedByFile)
		if len(subs) == 1 {
			subID = subs[0].ID
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	start := clock.Add(-24 * time.Hour) // the versions were released a day ago
	held := func(id, sub string, pre bool, until time.Time) (store.Event, []store.Delivery) {
		e, ds := ev(id, "w1", sub)
		ds[0].ID = "d-" + id
		st := start
		if id == "released" {
			st = clock.Add(-48 * time.Hour) // the new 48h cooldown ends now
		}
		ds[0].Hold = &store.Hold{Until: until, StartedAt: st, Prerelease: pre, WatchID: "w1", Raw: id}
		return e, ds
	}
	plan := []struct {
		id    string
		sub   string
		pre   bool
		until time.Time
	}{
		{"released", subID, false, start.Add(72 * time.Hour)},    // 24h cooldown has passed
		{"moved", subID, false, start.Add(72 * time.Hour)},       // 48h cooldown is still ahead
		{"dropped", subID, true, start.Add(72 * time.Hour)},      // a pre-release
		{"elapsed", subID, false, clock.Add(-time.Hour)},         // hold already over: queued
		{"other", "other-sub", false, start.Add(72 * time.Hour)}, // another subscription's
	}
	for _, p := range plan {
		e, ds := held(p.id, p.sub, p.pre, p.until)
		if err := st.AppendEvent(ctx, e, ds); err != nil {
			t.Fatal(err)
		}
	}
	// A queued delivery that was never held.
	e, ds := ev("queued", "w1", subID)
	if err := st.AppendEvent(ctx, e, ds); err != nil {
		t.Fatal(err)
	}

	p := mustApply(t, st, subDoc("{prerelease: none, cooldown: 48h}"))
	var detail string
	for _, c := range p.Changes {
		if c.Kind == KindSubscription {
			detail = c.Detail
		}
	}
	if detail != "1 held released, 1 held rescheduled, 1 held dead" {
		t.Fatalf("plan detail = %q", detail)
	}

	get := func(id string) store.Delivery {
		t.Helper()
		ds, err := st.ListDeliveries(ctx, id)
		if err != nil || len(ds) != 1 {
			t.Fatalf("deliveries of %s: %v %v", id, ds, err)
		}
		return ds[0]
	}
	if d := get("released"); d.Status != store.DeliveryPending || d.Hold != nil || !d.NextAttemptAt.Equal(clock) {
		t.Errorf("released = %+v", d)
	}
	if d := get("moved"); d.Hold == nil || !d.Hold.Until.Equal(start.Add(48*time.Hour)) || !d.NextAttemptAt.Equal(start.Add(48*time.Hour)) {
		t.Errorf("moved = %+v", d)
	}
	if d := get("dropped"); d.Status != store.DeliveryDead || d.LastError != DeadOnPolicy {
		t.Errorf("dropped = %+v", d)
	}
	if d := get("elapsed"); d.Status != store.DeliveryPending || d.Hold == nil || !d.Hold.Until.Equal(clock.Add(-time.Hour)) {
		t.Errorf("a hold that is over is queued and must not change: %+v", d)
	}
	if d := get("queued"); d.Status != store.DeliveryPending || d.Hold != nil || !d.NextAttemptAt.IsZero() {
		t.Errorf("queued must not change: %+v", d)
	}
	if d := get("other"); d.Status != store.DeliveryPending || d.Hold == nil || !d.Hold.Until.Equal(start.Add(72*time.Hour)) {
		t.Errorf("another subscription's delivery must not change: %+v", d)
	}

	// An edit that leaves the policy alone does not look at held deliveries,
	// and a lengthened cooldown moves the hold later.
	mustApply(t, st, subDoc("{prerelease: none, cooldown: 100h}"))
	if d := get("moved"); !d.Hold.Until.Equal(start.Add(100 * time.Hour)) {
		t.Errorf("lengthened = %+v", d)
	}
}
