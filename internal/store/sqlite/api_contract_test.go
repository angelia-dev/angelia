package sqlite

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// Store changes behind the OpenAPI v1 contract (story #185, spec docs/api.md
// decisions 2, 10 and 12).

// migratedTo opens a fresh database migrated up to version.
func migratedTo(t *testing.T, version int64) *sql.DB {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "angelia.db"))
	must(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = migrate(t.Context(), db, discard(), version)
	must(t, err)
	return db
}

func execAll(t *testing.T, db *sql.DB, qs ...string) {
	t.Helper()
	for _, q := range qs {
		if _, err := db.ExecContext(t.Context(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func schemaVersion(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	p, err := newProvider(db)
	must(t, err)
	v, err := p.GetDBVersion(t.Context())
	must(t, err)
	return v
}

// Decision 2: a name is unique per resource type whoever owns the row.
func TestNamesUniqueAcrossOwners(t *testing.T) {
	s, _ := openTest(t)
	ctx := t.Context()
	must(t, s.CreateTarget(ctx, store.Target{ID: "t1", Kind: "pypi", Coordinates: map[string]string{"name": "a"}, NextCheckAt: t0}))
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w1", TargetID: "t1", Name: "a", ManagedBy: store.ManagedByFile}))
	wantErr(t, s.CreateWatch(ctx, store.Watch{ID: "w2", TargetID: "t1", Name: "a", ManagedBy: store.ManagedByAPI}), store.ErrConflict, "watch name across owners")
	must(t, s.CreateSink(ctx, store.Sink{ID: "k1", Kind: "webhook", Name: "k", ManagedBy: store.ManagedByAPI}))
	wantErr(t, s.CreateSink(ctx, store.Sink{ID: "k2", Kind: "webhook", Name: "k", ManagedBy: store.ManagedByFile}), store.ErrConflict, "sink name across owners")
	must(t, s.CreateSubscription(ctx, store.Subscription{ID: "s1", SinkID: "k1", Name: "s", ManagedBy: store.ManagedByFile}))
	wantErr(t, s.CreateSubscription(ctx, store.Subscription{ID: "s2", SinkID: "k1", Name: "s"}), store.ErrConflict, "subscription name across owners")
	// The same name in two resource types is fine, and unnamed rows never collide.
	must(t, s.CreateSink(ctx, store.Sink{ID: "k3", Kind: "webhook", Name: "a"}))
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w3", TargetID: "t1"}))
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w4", TargetID: "t1"}))
}

// The 00011 migration rejects duplicate names with a message that lists them,
// leaves the schema at 00010 and changes no row. Once the duplicates are gone
// it applies.
func TestUniqueNamesMigrationRefusesDuplicates(t *testing.T) {
	ctx := t.Context()
	db := migratedTo(t, 10)
	execAll(t, db,
		`INSERT INTO target (id, kind, coordinates_json, next_check_at) VALUES ('t1','npm','{"name":"x"}',1)`,
		`INSERT INTO watch (id, target_id, name, managed_by) VALUES ('w1','t1','a','file'), ('w2','t1','a','api'), ('w3','t1','b','file'), ('w4','t1',NULL,'api'), ('w5','t1',NULL,'api')`,
		`INSERT INTO sink (id, kind, config_json_encrypted, name, managed_by) VALUES ('k1','webhook',x'00','k','api'), ('k2','webhook',x'00','k','file')`,
		`INSERT INTO subscription (id, sink_id, name, managed_by) VALUES ('s1','k1','s','api'), ('s2','k2','t','file')`,
	)
	_, err := migrate(ctx, db, discard(), 0)
	if err == nil {
		t.Fatal("migration applied over duplicate names")
	}
	t.Log(err)
	for _, want := range []string{
		"names must be unique per resource type across owners",
		`watch "a" (managed by api, file)`,
		`sink "k" (managed by api, file)`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), `"b"`) || strings.Contains(err.Error(), "subscription") {
		t.Errorf("error lists names that are not duplicates: %v", err)
	}
	if v := schemaVersion(t, db); v != 10 {
		t.Fatalf("schema version = %d, want 10", v)
	}
	var watches, sinks int
	must(t, db.QueryRowContext(ctx, `SELECT count(*) FROM watch`).Scan(&watches))
	must(t, db.QueryRowContext(ctx, `SELECT count(*) FROM sink`).Scan(&sinks))
	if watches != 5 || sinks != 2 {
		t.Fatalf("rows changed: %d watches, %d sinks", watches, sinks)
	}

	execAll(t, db, `UPDATE watch SET name = 'a2' WHERE id = 'w2'`, `UPDATE sink SET name = 'k2' WHERE id = 'k2'`)
	_, err = migrate(ctx, db, discard(), 0)
	must(t, err)
	if _, err := db.ExecContext(ctx, `INSERT INTO watch (id, target_id, name, managed_by) VALUES ('w6','t1','b','api')`); err == nil {
		t.Fatal("duplicate name across owners accepted after migration")
	}
	if _, err := db.ExecContext(ctx, `SELECT 1 FROM temp.migration_00011_check`); err == nil {
		t.Fatal("temporary check table left behind")
	}
}

// Down from 00011 restores the per-owner unique index.
func TestUniqueNamesMigrationDown(t *testing.T) {
	ctx := t.Context()
	db := migratedTo(t, 11)
	p, err := newProvider(db)
	must(t, err)
	_, err = p.DownTo(ctx, 10)
	must(t, err)
	execAll(t, db,
		`INSERT INTO target (id, kind, coordinates_json, next_check_at) VALUES ('t1','npm','{"name":"x"}',1)`,
		`INSERT INTO watch (id, target_id, name, managed_by) VALUES ('w1','t1','a','file'), ('w2','t1','a','api')`,
	)
	if _, err := db.ExecContext(ctx, `INSERT INTO watch (id, target_id, name, managed_by) VALUES ('w3','t1','a','api')`); err == nil {
		t.Fatal("per-owner uniqueness not restored")
	}
}

// Decision 10: the interval a watch asked for is stored on the watch and read
// back by every read path. Zero stays unset.
func TestWatchIntervalRoundTrip(t *testing.T) {
	s, _ := openTest(t)
	ctx := t.Context()
	must(t, s.CreateTarget(ctx, store.Target{ID: "t1", Kind: "pypi", Coordinates: map[string]string{"name": "a"}, NextCheckAt: t0, Interval: 5 * time.Minute}))
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w1", TargetID: "t1", Name: "a", ManagedBy: store.ManagedByFile, Interval: time.Hour}))
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w2", TargetID: "t1"}))

	w, err := s.GetWatch(ctx, "w1")
	must(t, err)
	if w.Interval != time.Hour {
		t.Fatalf("GetWatch interval = %s, want 1h", w.Interval)
	}
	if w, _ = s.GetWatch(ctx, "w2"); w.Interval != 0 {
		t.Fatalf("unset interval = %s", w.Interval)
	}
	ws, err := s.ListWatches(ctx, "t1")
	must(t, err)
	if len(ws) != 2 || ws[0].Interval != time.Hour || ws[1].Interval != 0 {
		t.Fatalf("ListWatches: %+v", ws)
	}
	must(t, s.InConfigTx(ctx, func(tx store.ConfigTx) error {
		ws, err := tx.ListManagedWatches(ctx, store.ManagedByFile)
		if err != nil || len(ws) != 1 || ws[0].Interval != time.Hour {
			t.Fatalf("ListManagedWatches: %+v %v", ws, err)
		}
		return tx.UpdateWatch(ctx, store.Watch{ID: "w1", TargetID: "t1", Interval: 10 * time.Minute})
	}))
	if w, _ = s.GetWatch(ctx, "w1"); w.Interval != 10*time.Minute {
		t.Fatalf("updated interval = %s", w.Interval)
	}
	// The target keeps its own (effective) interval.
	tg, err := s.GetTarget(ctx, "t1")
	must(t, err)
	if tg.Interval != 5*time.Minute {
		t.Fatalf("target interval = %s", tg.Interval)
	}
}

// Decision 12: the highest pruned seq starts at 0, follows every prune, and
// never goes down.
func TestHighestPrunedSeq(t *testing.T) {
	s, _ := openTest(t)
	seed(t, s)
	ctx := t.Context()
	got := func() int64 {
		t.Helper()
		n, err := s.HighestPrunedSeq(ctx)
		must(t, err)
		return n
	}
	if n := got(); n != 0 {
		t.Fatalf("fresh store = %d, want 0", n)
	}
	must(t, s.AppendEvent(ctx, eventAt("e1", pruneOld), nil))
	must(t, s.AppendEvent(ctx, eventAt("e2", pruneOld), []store.Delivery{deliveryWith("d2", "e2", "s1", store.DeliveryPending)}))
	must(t, s.AppendEvent(ctx, eventAt("e3", pruneOld), nil))
	must(t, s.AppendEvent(ctx, eventAt("e4", t0), nil))
	evs, err := s.ListEventsAfter(ctx, 0, 10)
	must(t, err)
	seq := map[string]int64{}
	for _, e := range evs {
		seq[e.ID] = e.Seq
	}

	_, err = s.PruneHistory(ctx, pruneBefore, 1)
	must(t, err)
	// e1 and e3 are pruned; e2 waits for its pending delivery.
	if n := got(); n != seq["e3"] {
		t.Fatalf("after prune = %d, want seq of e3 (%d)", n, seq["e3"])
	}
	// Pruning the older e2 later does not lower it.
	if _, err := s.db.ExecContext(ctx, `UPDATE delivery SET status = 'delivered' WHERE id = 'd2'`); err != nil {
		t.Fatal(err)
	}
	res, err := s.PruneHistory(ctx, pruneBefore, 0)
	must(t, err)
	if res.Events != 1 {
		t.Fatalf("second prune = %+v, want e2", res)
	}
	if n := got(); n != seq["e3"] {
		t.Fatalf("after pruning older e2 = %d, want %d", n, seq["e3"])
	}
	// A prune that deletes nothing leaves it alone.
	_, err = s.PruneHistory(ctx, pruneBefore, 0)
	must(t, err)
	if n := got(); n != seq["e3"] {
		t.Fatalf("after empty prune = %d", n)
	}
}

// The 00013 migration recovers the highest pruned seq from the gaps that
// earlier prunes left in the event log.
func TestHighestPrunedSeqBackfill(t *testing.T) {
	for _, tc := range []struct {
		name    string
		events  int
		deleted []int
		want    int64
	}{
		{name: "empty log", want: 0},
		{name: "nothing pruned", events: 3, want: 0},
		{name: "oldest pruned", events: 5, deleted: []int{1, 2}, want: 2},
		{name: "gap above a kept event", events: 5, deleted: []int{1, 3, 4}, want: 4},
		{name: "newest pruned", events: 5, deleted: []int{1, 2, 5}, want: 5},
		{name: "all pruned", events: 3, deleted: []int{1, 2, 3}, want: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			db := migratedTo(t, 12)
			for i := 1; i <= tc.events; i++ {
				if _, err := db.ExecContext(ctx, `INSERT INTO event (id, watch_id, type, payload_json, created_at) VALUES (?, 'w1', 'x', '{}', 1)`, "e"+string(rune('0'+i))); err != nil {
					t.Fatal(err)
				}
			}
			for _, seq := range tc.deleted {
				if _, err := db.ExecContext(ctx, `DELETE FROM event WHERE seq = ?`, seq); err != nil {
					t.Fatal(err)
				}
			}
			_, err := migrate(ctx, db, discard(), 0)
			must(t, err)
			s := &Store{db: db, q: New(db)}
			got, err := s.HighestPrunedSeq(ctx)
			must(t, err)
			if got != tc.want {
				t.Fatalf("HighestPrunedSeq = %d, want %d", got, tc.want)
			}
		})
	}
}
