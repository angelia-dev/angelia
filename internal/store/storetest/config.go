package storetest

import (
	"errors"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

func testConfigTxOwnershipAndUniqueNames(t *testing.T, open opener) {
	s := open(t)
	ctx := t.Context()
	must(t, s.CreateTarget(ctx, store.Target{ID: "t1", Kind: "pypi", Coordinates: map[string]string{"name": "a"}, NextCheckAt: t0, Interval: time.Hour}))
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w1", TargetID: "t1", Name: "a", ManagedBy: store.ManagedByFile}))
	// Names are unique across owners (00011): an api row cannot take a file row's name.
	wantErr(t, s.CreateWatch(ctx, store.Watch{ID: "w2", TargetID: "t1", Name: "a"}), store.ErrConflict, "api row, same name")
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w3", TargetID: "t1"})) // unnamed
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w4", TargetID: "t1"})) // unnamed rows never collide
	wantErr(t, s.CreateWatch(ctx, store.Watch{ID: "w5", TargetID: "t1", Name: "a", ManagedBy: store.ManagedByFile}), store.ErrConflict, "duplicate file name")

	must(t, s.InConfigTx(ctx, func(tx store.ConfigTx) error {
		ws, err := tx.ListManagedWatches(ctx, store.ManagedByFile)
		if err != nil || len(ws) != 1 || ws[0].ID != "w1" {
			t.Fatalf("file watches: %v %v", ws, err)
		}
		tg, err := tx.FindTarget(ctx, "pypi", map[string]string{"name": "a"})
		if err != nil || tg.ID != "t1" || tg.Interval != time.Hour {
			t.Fatalf("find target: %+v %v", tg, err)
		}
		if _, err := tx.FindTarget(ctx, "pypi", map[string]string{"name": "zz"}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("find missing: %v", err)
		}
		// Not deleted while a watch uses it.
		if ok, err := tx.DeleteTargetIfUnused(ctx, "t1"); err != nil || ok {
			t.Fatalf("delete used target: %v %v", ok, err)
		}
		return nil
	}))
	if err := s.InConfigTx(ctx, func(tx store.ConfigTx) error {
		return tx.SetTargetInterval(ctx, "nope", time.Hour, t0)
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("set interval on missing target: %v", err)
	}
	if err := s.InConfigTx(ctx, func(tx store.ConfigTx) error {
		return tx.SetTargetInterval(ctx, "t1", 0, t0)
	}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("zero interval: %v", err)
	}
}

// UpdateWatch leaves baseline_done to the worker; ResetWatchBaseline clears it.
func testUpdateWatchKeepsBaseline(t *testing.T, open opener) {
	s := open(t)
	ctx := t.Context()
	must(t, s.CreateTarget(ctx, store.Target{ID: "t1", Kind: "pypi", Coordinates: map[string]string{"name": "a"}, NextCheckAt: t0}))
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w1", TargetID: "t1", Name: "a", ManagedBy: store.ManagedByFile}))
	must(t, s.MarkBaselineDone(ctx, "w1"))
	must(t, s.InConfigTx(ctx, func(tx store.ConfigTx) error {
		return tx.UpdateWatch(ctx, store.Watch{ID: "w1", TargetID: "t1", Labels: map[string]string{"k": "v"}})
	}))
	w, err := s.GetWatch(ctx, "w1")
	must(t, err)
	if !w.BaselineDone || w.Labels["k"] != "v" {
		t.Fatalf("watch: %+v", w)
	}
	must(t, s.InConfigTx(ctx, func(tx store.ConfigTx) error { return tx.ResetWatchBaseline(ctx, "w1") }))
	if w, _ = s.GetWatch(ctx, "w1"); w.BaselineDone {
		t.Fatal("baseline not reset")
	}
	wantErr(t, s.InConfigTx(ctx, func(tx store.ConfigTx) error { return tx.ResetWatchBaseline(ctx, "nope") }), store.ErrNotFound, "reset missing")
}

// testAdoption: AdoptRow moves a file row to the API in place (id, baseline
// kept) and records the name; a second adoption, an API row or an unknown id
// is ErrNotFound; records are listed by kind and name and deleted by name.
func testAdoption(t *testing.T, open opener) {
	s := open(t)
	ctx := t.Context()
	must(t, s.CreateTarget(ctx, store.Target{ID: "t1", Kind: "pypi", Coordinates: map[string]string{"name": "a"}, NextCheckAt: t0}))
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w1", TargetID: "t1", Name: "a", ManagedBy: store.ManagedByFile}))
	must(t, s.MarkBaselineDone(ctx, "w1"))
	must(t, s.CreateSink(ctx, store.Sink{ID: "k1", Kind: "webhook", ConfigEncrypted: []byte(`{}`), Name: "hook", ManagedBy: store.ManagedByFile}))
	must(t, s.CreateSubscription(ctx, store.Subscription{ID: "s1", SinkID: "k1", Name: "sub", ManagedBy: store.ManagedByFile}))
	must(t, s.CreateSink(ctx, store.Sink{ID: "k2", Kind: "webhook", ConfigEncrypted: []byte(`{}`), Name: "api-hook"}))

	must(t, s.InConfigTx(ctx, func(tx store.ConfigTx) error {
		if err := tx.AdoptRow(ctx, store.KindWatch, "w1", "a", t0); err != nil {
			return err
		}
		if err := tx.AdoptRow(ctx, store.KindSink, "k1", "hook", t0); err != nil {
			return err
		}
		return tx.AdoptRow(ctx, store.KindSubscription, "s1", "sub", t0.Add(time.Second))
	}))
	w, err := s.GetWatch(ctx, "w1")
	must(t, err)
	if w.ManagedBy != store.ManagedByAPI || !w.BaselineDone || w.Name != "a" {
		t.Fatalf("adopted watch: %+v", w)
	}
	must(t, s.InConfigTx(ctx, func(tx store.ConfigTx) error {
		for _, owner := range []string{store.ManagedByFile, store.ManagedByAPI} {
			ws, err := tx.ListManagedWatches(ctx, owner)
			must(t, err)
			ks, err := tx.ListManagedSinks(ctx, owner)
			must(t, err)
			ss, err := tx.ListManagedSubscriptions(ctx, owner)
			must(t, err)
			want := 0
			if owner == store.ManagedByAPI {
				want = 1
			}
			if len(ws) != want || len(ss) != want || len(ks) != 2*want {
				t.Fatalf("%s rows: %d watches, %d sinks, %d subscriptions", owner, len(ws), len(ks), len(ss))
			}
		}
		recs, err := tx.ListAdoptions(ctx)
		must(t, err)
		if len(recs) != 3 || recs[0].Kind != store.KindSink || recs[0].Name != "hook" || !recs[0].AdoptedAt.Equal(t0) ||
			recs[2].Kind != store.KindWatch || recs[1].Kind != store.KindSubscription || !recs[1].AdoptedAt.Equal(t0.Add(time.Second)) {
			t.Fatalf("records: %+v", recs)
		}
		return nil
	}))
	for _, c := range []struct{ kind, id string }{
		{store.KindWatch, "w1"}, // already adopted
		{store.KindSink, "k2"},  // an API row
		{store.KindSubscription, "nope"},
	} {
		err := s.InConfigTx(ctx, func(tx store.ConfigTx) error { return tx.AdoptRow(ctx, c.kind, c.id, "x", t0) })
		wantErr(t, err, store.ErrNotFound, "adopt "+c.kind+" "+c.id)
	}
	wantErr(t, s.InConfigTx(ctx, func(tx store.ConfigTx) error { return tx.AdoptRow(ctx, "secret", "k1", "x", t0) }), store.ErrInvalid, "unknown kind")

	must(t, s.InConfigTx(ctx, func(tx store.ConfigTx) error {
		must(t, tx.DeleteAdoption(ctx, store.KindSink, "hook"))
		must(t, tx.DeleteAdoption(ctx, store.KindSink, "never")) // no record: no error
		recs, err := tx.ListAdoptions(ctx)
		must(t, err)
		if len(recs) != 2 {
			t.Fatalf("after delete: %+v", recs)
		}
		return nil
	}))
}
