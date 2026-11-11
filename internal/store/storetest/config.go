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
