package main

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store/pgtest"
	"github.com/angelia-dev/angelia/internal/store/postgres"
)

// postgresFixture migrates a schema of its own with angelia's Postgres
// store, so that a schema change that breaks the queries fails here and not
// in a run, and seeds it as TestReadSnapshot seeds SQLite.
func postgresFixture(t *testing.T) (dsn string, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	dsn = pgtest.DSN(t)
	st, err := postgres.Open(ctx, dsn, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	db, err = sql.Open(pgtest.DriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, q := range []string{
		`INSERT INTO target (id, kind, coordinates_json, next_check_at, last_checked_at) VALUES ('t0', 'pypi', '{"name":"pkg-00000"}', 0, 5)`,
		`INSERT INTO watch (id, target_id) VALUES ('w0', 't0')`,
		`INSERT INTO version (target_id, raw, normalized, first_seen_at) VALUES ('t0', '1.0.0', '1.0.0', 1), ('t0', '1.1.0', '1.1.0', 2)`,
		`INSERT INTO event (id, watch_id, type, payload_json, created_at) VALUES ('e1', 'w0', 'dev.angelia.release.published.v1', '{"data":{"package":"pkg-00000","version":"1.1.0"}}', 2)`,
		`INSERT INTO sink (id, kind, config_json, name) VALUES ('k0', 'webhook', '{}'::bytea, 'hook-0'), ('k1', 'webhook', '{}'::bytea, 'hook-1')`,
		`INSERT INTO subscription (id, sink_id, name) VALUES ('s0', 'k0', 'sub-0'), ('s1', 'k1', 'sub-1')`,
		`INSERT INTO delivery (id, event_id, subscription_id, status, attempts) VALUES ('d0', 'e1', 's0', 'delivered', 2)`,
		`INSERT INTO delivery (id, event_id, subscription_id, status, attempts, last_error) VALUES ('d1', 'e1', 's1', 'in_flight', 1, 'boom')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return dsn, db
}

// The snapshot of a Postgres database holds what the SQLite snapshot holds
// for the same rows, through the same functions.
func TestReadSnapshotPostgres(t *testing.T) {
	ctx := context.Background()
	dsn, _ := postgresFixture(t)
	if !isPostgres(dsn) || isPostgres("/tmp/angelia.db") {
		t.Fatal("isPostgres")
	}

	s, err := readSnapshot(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Postgres || s.Packages["w0"] != "pkg-00000" || len(s.Versions["pkg-00000"]) != 2 || len(s.Events) != 1 || len(s.Deliveries) != 2 {
		t.Fatalf("snapshot %+v", s)
	}
	if e := s.Events[0]; e.Package != "pkg-00000" || e.Version != "1.1.0" || e.WatchID != "w0" {
		t.Errorf("event %+v", e)
	}
	if s.Subscriptions["s0"] != "hook-0" || s.Subscriptions["s1"] != "hook-1" {
		t.Errorf("subscriptions %v", s.Subscriptions)
	}
	if d := s.Deliveries[1]; d.ID != "d1" || d.Status != "in_flight" || d.Attempts != 1 || d.LastError != "boom" {
		t.Errorf("delivery %+v", d)
	}
	if len(s.Integrity) != 0 {
		t.Errorf("integrity of a sound database: %v", s.Integrity)
	}
	// The verification names the check for the engine.
	checks, _ := Verify(s, nil, Truth{})
	if !strings.Contains(checks[0].Name, "foreign keys, constraints, indexes") {
		t.Errorf("first check %q", checks[0].Name)
	}

	state, err := readState(ctx, dsn, time.UnixMilli(1))
	if err != nil || state.Backlog != (Backlog{InFlight: 1}) || state.MaxSeq != 1 || state.Checks != 1 || !state.OldestCheck.Equal(time.UnixMilli(5)) {
		t.Errorf("state %+v, %v", state, err)
	}
	if later, _ := readState(ctx, dsn, time.UnixMilli(5)); later.Checks != 0 {
		t.Errorf("checks after the last check: %d", later.Checks)
	}

	// The harness only reads: its session is read-only.
	rdb, err := openDB(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rdb.Close() }()
	if _, err := rdb.ExecContext(ctx, `DELETE FROM delivery`); err == nil {
		t.Error("the read session wrote")
	}
}

// The Postgres integrity check reports what foreign_key_check and
// integrity_check report on SQLite.
func TestPostgresIntegrityFindsFaults(t *testing.T) {
	ctx := context.Background()
	dsn, db := postgresFixture(t)
	exec := func(q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// An orphan row: the foreign key is gone, as after a restore that
	// skipped constraints.
	var fk string
	if err := db.QueryRowContext(ctx, `SELECT conname FROM pg_constraint WHERE conrelid = 'delivery'::regclass AND contype = 'f'`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	exec(`ALTER TABLE delivery DROP CONSTRAINT ` + fk)
	exec(`INSERT INTO delivery (id, event_id, subscription_id, status) VALUES ('d2', 'ghost', 's0', 'pending')`)
	s, err := readSnapshot(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Integrity) != 1 || !strings.Contains(s.Integrity[0], "foreign_key_check: 1 rows of delivery refer to a missing event") {
		t.Errorf("orphan: integrity %v", s.Integrity)
	}

	// A constraint that is not validated, and a foreign key the benchmark
	// does not list.
	exec(`ALTER TABLE delivery ADD CONSTRAINT delivery_sub_fk FOREIGN KEY (subscription_id) REFERENCES subscription (id) NOT VALID`)
	s, err = readSnapshot(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(s.Integrity, "\n")
	for _, want := range []string{"constraint delivery_sub_fk on delivery is not validated", "foreign key from delivery to subscription is not checked"} {
		if !strings.Contains(joined, want) {
			t.Errorf("integrity lacks %q:\n%s", want, joined)
		}
	}

	// An invalid index, as a crashed CREATE INDEX CONCURRENTLY leaves.
	exec(`CREATE TABLE scratch (a int)`)
	exec(`INSERT INTO scratch VALUES (1), (1)`)
	if _, err := db.ExecContext(ctx, `CREATE UNIQUE INDEX CONCURRENTLY scratch_a ON scratch (a)`); err == nil {
		t.Fatal("the unique index on duplicates was built")
	}
	s, err = readSnapshot(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(s.Integrity, "\n"), "index scratch_a is not valid") {
		t.Errorf("invalid index: integrity %v", s.Integrity)
	}
}

// The foreign keys the benchmark lists are exactly the schema's.
func TestForeignKeysListIsTheSchemas(t *testing.T) {
	ctx := context.Background()
	_, db := postgresFixture(t)
	problems, err := checkIntegrityPostgres(ctx, db)
	if err != nil || len(problems) != 0 {
		t.Fatalf("a migrated schema: %v, %v", problems, err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_constraint WHERE connamespace = current_schema()::regnamespace AND contype = 'f'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(foreignKeys) {
		t.Errorf("the schema has %d foreign keys, the benchmark lists %d", n, len(foreignKeys))
	}
}

// requireEmptyPostgres takes a new database and refuses one with tables.
func TestRequireEmptyPostgres(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.DSN(t) // a schema with no tables
	v, err := requireEmptyPostgres(ctx, dsn)
	if err != nil || v == "" {
		t.Fatalf("new database: %q, %v", v, err)
	}
	dsn2, _ := postgresFixture(t)
	if _, err := requireEmptyPostgres(ctx, dsn2); err == nil || !strings.Contains(err.Error(), "new database") {
		t.Errorf("migrated database: %v", err)
	}
	// An unreachable server fails without echoing the DSN's password.
	_, err = requireEmptyPostgres(ctx, "postgres://u:s3cret@127.0.0.1:1/x?sslmode=disable&connect_timeout=1")
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("unreachable: %v", err)
	}
}
