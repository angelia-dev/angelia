package sqlite

import (
	"errors"
	"path/filepath"
	"slices"
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
	// Seed at schema 15, the last one with sink.config_json_encrypted
	// (renamed by 00016), so later migrations do not break the seed.
	_, err = migrate(ctx, db, discard(), 15)
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
func TestUpdateWatchKeepsBaseline(t *testing.T) {
	s, _ := openTest(t)
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

// Story #205: a target stores the kind of the source it was created for,
// NULL when that is its own kind; the stored sources can be listed.
func TestTargetSourceKindRoundTrip(t *testing.T) {
	s, _ := openTest(t)
	ctx := t.Context()
	at := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	mk := func(id, kind, srcKind, name string) {
		t.Helper()
		must(t, s.CreateTarget(ctx, store.Target{ID: id, Kind: kind, SourceKind: srcKind, Coordinates: map[string]string{"name": name}, NextCheckAt: at}))
	}
	mk("t1", "npm", "", "a")
	mk("t2", "npm", "npm", "b") // redundant: stored as NULL
	mk("t3", "npm-internal", "npm", "a")
	for id, want := range map[string]string{"t1": "npm", "t2": "npm", "t3": "npm"} {
		got, err := s.GetTarget(ctx, id)
		must(t, err)
		if got.EffectiveSourceKind() != want {
			t.Errorf("%s: effective source kind %q, want %q", id, got.EffectiveSourceKind(), want)
		}
	}
	if g, _ := s.GetTarget(ctx, "t2"); g.SourceKind != "" {
		t.Errorf("a source kind equal to the kind is stored as NULL, got %q", g.SourceKind)
	}
	if g, _ := s.GetTarget(ctx, "t3"); g.SourceKind != "npm" || g.Kind != "npm-internal" {
		t.Errorf("t3 = kind %q source kind %q", g.Kind, g.SourceKind)
	}
	// The same coordinates under two names are two targets.
	if a, b := mustTarget(t, s, "t1"), mustTarget(t, s, "t3"); a.ID == b.ID {
		t.Fatal("instance and kind share a target")
	}
	got, err := s.TargetSources(ctx)
	must(t, err)
	want := []store.TargetSource{{Name: "npm", Targets: 2}, {Name: "npm-internal", Kind: "npm", Targets: 1}}
	if !slices.Equal(got, want) {
		t.Fatalf("TargetSources = %+v, want %+v", got, want)
	}
}

func mustTarget(t *testing.T, s *Store, id string) store.Target {
	t.Helper()
	tg, err := s.GetTarget(t.Context(), id)
	must(t, err)
	return tg
}

// Story #220: targets are grouped by source name and interval, for the
// host demand estimate.
func TestTargetDemand(t *testing.T) {
	s, _ := openTest(t)
	ctx := t.Context()
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	mk := func(id, kind, name string, interval time.Duration) {
		t.Helper()
		must(t, s.CreateTarget(ctx, store.Target{ID: id, Kind: kind, Coordinates: map[string]string{"name": name}, NextCheckAt: at, Interval: interval}))
	}
	if got, err := s.TargetDemand(ctx); err != nil || len(got) != 0 {
		t.Fatalf("empty store: %+v, %v", got, err)
	}
	mk("t1", "npm", "a", time.Hour)
	mk("t2", "npm", "b", time.Hour)
	mk("t3", "npm", "c", 5*time.Minute)
	mk("t4", "pypi", "a", 0) // interval not set
	got, err := s.TargetDemand(ctx)
	must(t, err)
	want := []store.TargetDemand{
		{Name: "npm", Interval: 5 * time.Minute, Targets: 1},
		{Name: "npm", Interval: time.Hour, Targets: 2},
		{Name: "pypi", Targets: 1},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("TargetDemand = %+v, want %+v", got, want)
	}
}
