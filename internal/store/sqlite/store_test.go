package sqlite

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

var _ store.Store = (*Store)(nil)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "angelia.db")
	s, err := Open(t.Context(), path, discard())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

// seed creates target t1, watch w1, sink k1 and subscriptions s1, s2.
func seed(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	must(t, s.CreateTarget(ctx, store.Target{
		ID: "t1", Kind: "pypi", Coordinates: map[string]string{"name": "requests"},
		FetchState: json.RawMessage(`{"etag":"x"}`), NextCheckAt: t0,
	}))
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w1", TargetID: "t1", Labels: map[string]string{"team": "a"}}))
	must(t, s.CreateSink(ctx, store.Sink{ID: "k1", Kind: "webhook", ConfigEncrypted: []byte{1, 2}}))
	must(t, s.CreateSubscription(ctx, store.Subscription{ID: "s1", SinkID: "k1"}))
	must(t, s.CreateSubscription(ctx, store.Subscription{ID: "s2", SinkID: "k1", Filter: json.RawMessage(`{"bump":"major"}`)}))
}

func event(id string) store.Event {
	return store.Event{ID: id, WatchID: "w1", Type: "release.published", Payload: json.RawMessage(`{"v":1}`), CreatedAt: t0}
}

func pending(subID string) store.Delivery {
	return store.Delivery{ID: "d-" + subID, SubscriptionID: subID, Status: store.DeliveryPending}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestOpenCreatesSchemaWithSettings(t *testing.T) {
	s, _ := openTest(t)
	ctx := t.Context()
	for _, table := range []string{"target", "watch", "version", "event", "subscription", "sink", "delivery", "lease", "schema_migrations"} {
		var n int
		must(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type='table' AND name=?`, table).Scan(&n))
		if n != 1 {
			t.Errorf("table %s missing", table)
		}
	}
	if got := s.db.Stats().MaxOpenConnections; got != maxConns {
		t.Errorf("pool size = %d, want %d", got, maxConns)
	}
	// Hold every pooled connection at once: each must carry the settings.
	for i := range maxConns {
		conn, err := s.db.Conn(ctx)
		must(t, err)
		defer func() { _ = conn.Close() }()
		for pragma, want := range map[string]string{"journal_mode": "wal", "foreign_keys": "1", "busy_timeout": "5000", "synchronous": "1"} {
			var got string
			must(t, conn.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got))
			if got != want {
				t.Errorf("conn %d: PRAGMA %s = %q, want %q", i, pragma, got, want)
			}
		}
	}
}

func TestDriverIsModernc(t *testing.T) {
	s, _ := openTest(t)
	if got := reflect.TypeOf(s.db.Driver()).Elem().PkgPath(); got != "modernc.org/sqlite" {
		t.Fatalf("driver package = %s", got)
	}
}

func TestOpenNilLogger(t *testing.T) {
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "a.db"), nil)
	must(t, err)
	must(t, s.Close())
}

func TestRestartIsNoop(t *testing.T) {
	s, path := openTest(t)
	seed(t, s)
	must(t, s.Close())

	db, err := openDB(path)
	must(t, err)
	defer func() { _ = db.Close() }()
	applied, err := migrate(t.Context(), db, discard(), 0)
	must(t, err)
	if applied != 0 {
		t.Fatalf("restart applied %d migrations, want 0", applied)
	}

	s2, err := Open(t.Context(), path, discard())
	must(t, err)
	defer func() { _ = s2.Close() }()
	if _, err := s2.GetWatch(t.Context(), "w1"); err != nil {
		t.Fatalf("data lost on restart: %v", err)
	}
}

func TestMigrationsDownAndUp(t *testing.T) {
	ctx := t.Context()
	db, err := openDB(filepath.Join(t.TempDir(), "angelia.db"))
	must(t, err)
	defer func() { _ = db.Close() }()
	n, err := migrate(ctx, db, discard(), 0)
	must(t, err)
	p, err := newProvider(db)
	must(t, err)
	down, err := p.DownTo(ctx, 0)
	must(t, err)
	if len(down) != n {
		t.Fatalf("down ran %d migrations, want %d", len(down), n)
	}
	var tables int
	must(t, db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type='table' AND name <> ? AND name NOT LIKE 'sqlite_%'`, migrationsTable).Scan(&tables))
	if tables != 0 {
		t.Fatalf("%d tables left after down", tables)
	}
	if again, err := migrate(ctx, db, discard(), 0); err != nil || again != n {
		t.Fatalf("up after down: %d %v", again, err)
	}
}

// 00019 drops the redundant prefix index; its down recreates it exactly as
// 00002 defined it, and leaves the 00018 index alone.
func TestDropOldDeliveryIndexDownRecreatesIt(t *testing.T) {
	ctx := t.Context()
	db, err := openDB(filepath.Join(t.TempDir(), "angelia.db"))
	must(t, err)
	defer func() { _ = db.Close() }()
	// Stop at 00019: later migrations have their own tests.
	_, err = migrate(ctx, db, discard(), 19)
	must(t, err)
	indexSQL := func(name string) string {
		t.Helper()
		var def string
		err := db.QueryRowContext(ctx, `SELECT coalesce(sql, '') FROM sqlite_schema WHERE type = 'index' AND name = ?`, name).Scan(&def)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		return def
	}
	if got := indexSQL("delivery_status_next_attempt_at"); got != "" {
		t.Fatalf("old index present after up: %s", got)
	}
	p, err := newProvider(db)
	must(t, err)
	// Down rolls back only the latest migration, 00019.
	if _, err := p.Down(ctx); err != nil {
		t.Fatal(err)
	}
	const want = "CREATE INDEX delivery_status_next_attempt_at ON delivery (status, next_attempt_at)"
	if got := indexSQL("delivery_status_next_attempt_at"); got != want {
		t.Fatalf("down recreated %q, want %q", got, want)
	}
	if indexSQL("delivery_status_next_attempt_at_id") == "" {
		t.Fatal("down of 00019 removed the 00018 index")
	}
}

// 00020 only adds the consumer table (story #79-B); its down drops it.
func TestConsumerMigrationDown(t *testing.T) {
	ctx := t.Context()
	db, err := openDB(filepath.Join(t.TempDir(), "angelia.db"))
	must(t, err)
	defer func() { _ = db.Close() }()
	_, err = migrate(ctx, db, discard(), 0)
	must(t, err)
	tables := func() (n int) {
		t.Helper()
		must(t, db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'consumer'`).Scan(&n))
		return n
	}
	if tables() != 1 {
		t.Fatal("consumer table missing after up")
	}
	// The checks of the table: a negative cursor and a non-JSON filter are refused.
	for _, q := range []string{
		`INSERT INTO consumer (id, name, acked_seq, created_at, updated_at) VALUES ('c1','a',-1,1,1)`,
		`INSERT INTO consumer (id, name, filter_json, created_at, updated_at) VALUES ('c2','b','{bad',1,1)`,
	} {
		if _, err := db.ExecContext(ctx, q); err == nil {
			t.Fatalf("%s: accepted", q)
		}
	}
	p, err := newProvider(db)
	must(t, err)
	if _, err := p.Down(ctx); err != nil {
		t.Fatal(err)
	}
	if tables() != 0 {
		t.Fatal("consumer table present after down")
	}
}

func TestUpgradeMigrationOnPopulatedDB(t *testing.T) {
	ctx := t.Context()
	db, err := openDB(filepath.Join(t.TempDir(), "angelia.db"))
	must(t, err)
	defer func() { _ = db.Close() }()

	// Seed at schema 15, the last one with sink.config_json_encrypted
	// (renamed by 00016), so later migrations do not break the seed.
	if _, err := migrate(ctx, db, discard(), 15); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO target (id, kind, coordinates_json, next_check_at) VALUES ('t1','npm','{"name":"left-pad"}',1)`,
		`INSERT INTO watch (id, target_id) VALUES ('w1','t1')`,
		`INSERT INTO version (target_id, raw, normalized, first_seen_at) VALUES ('t1','1.0.0','1.0.0',2)`,
		`INSERT INTO event (id, watch_id, type, payload_json, created_at) VALUES ('e1','w1','x','{}',3)`,
		`INSERT INTO sink (id, kind, config_json_encrypted) VALUES ('k1','webhook',x'00')`,
		`INSERT INTO subscription (id, sink_id) VALUES ('s1','k1')`,
		`INSERT INTO delivery (id, event_id, subscription_id, status) VALUES ('d1','e1','s1','pending')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	applied, err := migrate(ctx, db, discard(), 0)
	must(t, err)
	if applied != 5 { // 00016 to 00020
		t.Fatalf("applied %d migrations, want 5", applied)
	}
	var idx int // 00018 adds the lease order index (story #232)
	must(t, db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type = 'index' AND name = 'delivery_status_next_attempt_at_id'`).Scan(&idx))
	if idx != 1 {
		t.Fatal("index delivery_status_next_attempt_at_id missing after upgrade")
	}
	// 00019 drops its prefix, the old index (story #234).
	must(t, db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type = 'index' AND name = 'delivery_status_next_attempt_at'`).Scan(&idx))
	if idx != 0 {
		t.Fatal("index delivery_status_next_attempt_at still present after upgrade")
	}
	s := &Store{db: db, q: New(db)}
	if vs, err := s.ListVersions(ctx, "t1"); err != nil || len(vs) != 1 {
		t.Fatalf("versions after upgrade: %v %v", vs, err)
	}
	if ds, err := s.ListDeliveries(ctx, "e1"); err != nil || len(ds) != 1 {
		t.Fatalf("deliveries after upgrade: %v %v", ds, err)
	}
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	must(t, err)
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		t.Fatal("foreign key violations after upgrade")
	}
	must(t, rows.Err())
}

// Deleting config rows leaves history that still satisfies the foreign keys,
// and the schema refuses a delivery for a missing event.
func TestForeignKeysHoldAfterConfigDeletes(t *testing.T) {
	s, _ := openTest(t)
	seed(t, s)
	ctx := t.Context()
	must(t, s.AppendEvent(ctx, event("e1"), []store.Delivery{pending("s1"), pending("s2")}))
	must(t, s.DeleteWatch(ctx, "w1"))
	must(t, s.DeleteSubscription(ctx, "s1"))
	must(t, s.DeleteSubscription(ctx, "s2"))
	must(t, s.DeleteSink(ctx, "k1"))

	if _, err := s.db.ExecContext(ctx, `INSERT INTO delivery (id, event_id, subscription_id, status) VALUES ('d9','nope','s1','pending')`); err == nil {
		t.Error("delivery for a missing event accepted")
	}
	rows, err := s.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	must(t, err)
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		t.Fatal("foreign key violations after deletes")
	}
	must(t, rows.Err())
}
