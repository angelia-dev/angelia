package sqlite

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/angelia-dev/angelia/internal/store"
)

var errRollback = errors.New("rollback")

func TestSubscriptionGenerationCountsEverySubscriptionWrite(t *testing.T) {
	s, _ := openTest(t)
	ctx := t.Context()
	gen := func() int64 {
		t.Helper()
		g, err := s.SubscriptionGeneration(ctx)
		must(t, err)
		return g
	}
	g0 := gen()
	seed(t, s) // creates a target, a watch, a sink and two subscriptions
	g1 := gen()
	if g1 != g0+2 {
		t.Fatalf("after two inserts: generation %d, want %d", g1, g0+2)
	}
	if again := gen(); again != g1 {
		t.Fatalf("a read changed the generation: %d -> %d", g1, again)
	}
	must(t, s.InConfigTx(ctx, func(tx store.ConfigTx) error {
		return tx.UpdateSubscription(ctx, store.Subscription{ID: "s1", SinkID: "k1", Filter: json.RawMessage(`{"bump":["major"]}`)})
	}))
	g2 := gen()
	if g2 <= g1 {
		t.Fatalf("update did not advance the generation: %d -> %d", g1, g2)
	}
	must(t, s.DeleteSubscription(ctx, "s2"))
	g3 := gen()
	if g3 <= g2 {
		t.Fatalf("delete did not advance the generation: %d -> %d", g2, g3)
	}
	// A rolled-back write leaves the counter alone.
	err := s.InConfigTx(ctx, func(tx store.ConfigTx) error {
		if err := tx.CreateSubscription(ctx, store.Subscription{ID: "s9", SinkID: "k1"}); err != nil {
			return err
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatalf("InConfigTx: %v", err)
	}
	if g := gen(); g != g3 {
		t.Fatalf("rolled-back insert advanced the generation: %d -> %d", g3, g)
	}
	// Other tables do not count.
	must(t, s.CreateSink(ctx, store.Sink{ID: "k2", Kind: "webhook", ConfigEncrypted: []byte{1}}))
	if g := gen(); g != g3 {
		t.Fatalf("a sink insert advanced the generation: %d -> %d", g3, g)
	}
}
