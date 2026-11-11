package storetest

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// Acceptance criterion of #106: events are never updated after insert. The
// store API has no update and a second append of the same id is refused
// without touching the stored row. (The schema's own refusal of an UPDATE
// from any path is checked by each engine.)
func testEventImmutable(t *testing.T, open opener) {
	s := open(t)
	seed(t, s)
	ctx := t.Context()
	must(t, s.AppendEvent(ctx, event("e1"), []store.Delivery{pending("s1")}))

	again := event("e1")
	again.Payload = json.RawMessage(`{"v":2}`)
	again.Type = "release.prerelease"
	if err := s.AppendEvent(ctx, again, nil); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second AppendEvent of e1 = %v, want ErrConflict", err)
	}
	evs, err := s.ListEventsAfter(ctx, 0, 0)
	must(t, err)
	if len(evs) != 1 || evs[0].ID != "e1" || string(evs[0].Payload) != `{"v":1}` ||
		evs[0].Type != "release.published" || !evs[0].CreatedAt.Equal(t0) {
		t.Fatalf("events after refused writes = %+v, want e1 unchanged", evs)
	}
	ds, err := s.ListDeliveries(ctx, "e1")
	must(t, err)
	if len(ds) != 1 {
		t.Fatalf("deliveries = %+v, want the first append's one", ds)
	}

	// Retention still deletes events.
	res, err := s.PruneHistory(ctx, t0.Add(time.Second), 0)
	must(t, err)
	if res.Events != 0 {
		t.Fatalf("pruned %d events with a pending delivery", res.Events)
	}
	deliverAll(t, s)
	if res, err = s.PruneHistory(ctx, t0.Add(time.Second), 0); err != nil || res.Events != 1 {
		t.Fatalf("PruneHistory = %+v, %v; want 1 event deleted", res, err)
	}
}

func testListSubscriptions(t *testing.T, open opener) {
	s := open(t)
	seed(t, s)
	ctx := t.Context()
	must(t, s.CreateSubscription(ctx, store.Subscription{
		ID: "s0", SinkID: "k1", Policy: json.RawMessage(`{"prerelease":"none","cooldown":"1h0m0s"}`),
		Name: "first", ManagedBy: store.ManagedByFile,
	}))
	subs, err := s.ListSubscriptions(ctx)
	must(t, err)
	var ids []string
	for _, x := range subs {
		ids = append(ids, x.ID)
	}
	if len(ids) != 3 || ids[0] != "s0" || ids[1] != "s1" || ids[2] != "s2" {
		t.Fatalf("ids = %v, want [s0 s1 s2]", ids)
	}
	if subs[0].Name != "first" || subs[0].ManagedBy != store.ManagedByFile ||
		string(subs[0].Policy) != `{"prerelease":"none","cooldown":"1h0m0s"}` || string(subs[2].Filter) != `{"bump":"major"}` {
		t.Fatalf("rows = %+v", subs)
	}
	err = s.InTx(ctx, func(tx store.Tx) error {
		in, err := tx.ListSubscriptions(ctx)
		if err == nil && len(in) != 3 {
			t.Errorf("in tx: %d subscriptions", len(in))
		}
		return err
	})
	must(t, err)
}
