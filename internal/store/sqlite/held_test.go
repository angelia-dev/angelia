package sqlite

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
		!h.Hold.Prerelease || h.Hold.WatchID != "w1" || h.Hold.Raw != "1.2.0" || string(h.Hold.Input) != `{"bump":"patch"}` {
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
