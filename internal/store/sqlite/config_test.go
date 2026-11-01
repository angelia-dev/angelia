package sqlite

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// Rows that exist before the ownership migration become 'api' rows without a
// name, and the migration drops nothing.
func TestOwnershipMigrationOnPopulatedDB(t *testing.T) {
	ctx := t.Context()
	db, err := openDB(filepath.Join(t.TempDir(), "a.db"))
	must(t, err)
	defer func() { _ = db.Close() }()
	p, err := newProvider(db)
	must(t, err)
	srcs := p.ListSources()
	prev := srcs[len(srcs)-2].Version
	if srcs[len(srcs)-1].Version < 4 {
		t.Skip("ownership migration is not the latest")
	}
	_, err = migrate(ctx, db, discard(), prev)
	must(t, err)
	for _, q := range []string{
		`INSERT INTO target (id, kind, coordinates_json, next_check_at) VALUES ('t1','npm','{"name":"x"}',1)`,
		`INSERT INTO watch (id, target_id) VALUES ('w1','t1')`,
		`INSERT INTO sink (id, kind, config_json_encrypted) VALUES ('k1','webhook',x'00')`,
		`INSERT INTO subscription (id, sink_id) VALUES ('s1','k1')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	_, err = migrate(ctx, db, discard(), 0)
	must(t, err)
	s := &Store{db: db, q: New(db)}
	w, err := s.GetWatch(ctx, "w1")
	must(t, err)
	if w.ManagedBy != store.ManagedByAPI || w.Name != "" {
		t.Fatalf("watch: %+v", w)
	}
	tg, err := s.GetTarget(ctx, "t1")
	must(t, err)
	if tg.Interval != 0 {
		t.Fatalf("interval = %s", tg.Interval)
	}
	var owners int
	must(t, db.QueryRowContext(ctx, `SELECT count(*) FROM sink WHERE managed_by='api' AND name IS NULL`).Scan(&owners))
	if owners != 1 {
		t.Fatal("sink not migrated to api")
	}
}

func TestConfigTxOwnershipAndUniqueNames(t *testing.T) {
	s, _ := openTest(t)
	ctx := t.Context()
	must(t, s.CreateTarget(ctx, store.Target{ID: "t1", Kind: "pypi", Coordinates: map[string]string{"name": "a"}, NextCheckAt: t0, Interval: time.Hour}))
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w1", TargetID: "t1", Name: "a", ManagedBy: store.ManagedByFile}))
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w2", TargetID: "t1", Name: "a"})) // api row, same name
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w3", TargetID: "t1"}))            // unnamed
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w4", TargetID: "t1"}))            // unnamed rows never collide
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
