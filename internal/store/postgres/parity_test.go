package postgres

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"

	"github.com/angelia-dev/angelia/internal/store/pgtest"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
)

// sqliteHead migrates a fresh SQLite database to the head with the SQLite
// engine's own Open and returns a plain connection to it.
func sqliteHead(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "parity.db")
	s, err := sqlite.Open(t.Context(), path, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// openSchema returns a database on a fresh test schema (pgtest), migrated to
// the head.
func openSchema(t *testing.T) *sql.DB {
	t.Helper()
	db := openRaw(t, pgtest.DSN(t))
	if _, err := Migrate(t.Context(), db, discardLogger()); err != nil {
		t.Fatal(err)
	}
	return db
}

func openRaw(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open(DriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestSchemaParity migrates a fresh SQLite database and a fresh Postgres
// schema to the head and compares their normalised catalogues: tables,
// columns with type class, nullability and default, primary, unique and
// foreign keys with their ON DELETE action, the columns that carry a CHECK,
// indexes with their columns and predicate, and triggers with their timing
// and event (ADR 0028 §7).
func TestSchemaParity(t *testing.T) {
	pg := openSchema(t)
	want, err := sqliteCatalogue(t.Context(), sqliteHead(t))
	if err != nil {
		t.Fatal(err)
	}
	got, err := postgresCatalogue(t.Context(), pg)
	if err != nil {
		t.Fatal(err)
	}
	if d := diff(want, got); d != "" {
		t.Fatalf("Postgres schema differs from SQLite's at the head (- SQLite only, + Postgres only):\n%s", d)
	}
	sqVersion := headVersion(t, sqliteHead(t))
	pgVersion := headVersion(t, pg)
	if sqVersion != pgVersion {
		t.Fatalf("head version: SQLite %d, Postgres %d", sqVersion, pgVersion)
	}
}

func headVersion(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var v int64
	if err := db.QueryRowContext(t.Context(), `SELECT max(version_id) FROM `+migrationsTable).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestSQLiteCatalogue checks the SQLite side of the parity test on its own,
// so that a parser that reads nothing cannot make the comparison pass.
func TestSQLiteCatalogue(t *testing.T) {
	c, err := sqliteCatalogue(t.Context(), sqliteHead(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"table consumer",
		"column event.seq int64 not null",
		"column watch.baseline_done bool not null default false",
		"column sink.config_json bytes not null",
		"column subscription.template text not null default ''",
		"primary key version (target_id,raw)",
		"unique delivery (event_id,subscription_id)",
		"foreign key pending_release (target_id,raw) references version (target_id,raw) on delete cascade",
		"foreign key watch (target_id) references target (id) on delete no action",
		"check event.payload_json",
		"check consumer.acked_seq",
		"index event_watch_subject on event (watch_id,<expr>)",
		"index delivery_held on delivery (subscription_id) where held_until is not null",
		"index watch_name on watch unique (name)",
		"trigger event_immutable on event before update",
		"trigger subscription_generation_update on subscription after update",
		"trigger event_prune_seq on event after delete",
	} {
		if !slices.Contains(c, line) {
			t.Errorf("SQLite catalogue lacks %q", line)
		}
	}
	for _, line := range c {
		if line == "check watch.baseline_done" || line == "table schema_migrations" || line == "table sqlite_sequence" {
			t.Errorf("SQLite catalogue has %q", line)
		}
	}
}

func TestDiff(t *testing.T) {
	if d := diff(catalogue{"a", "b"}, catalogue{"a", "b"}); d != "" {
		t.Fatalf("diff of equal catalogues = %q", d)
	}
	if d, want := diff(catalogue{"a", "b x"}, catalogue{"a", "b y", "c"}), "- b x\n+ b y\n+ c"; d != want {
		t.Fatalf("diff = %q, want %q", d, want)
	}
}

func TestSQLiteCheckedColumns(t *testing.T) {
	got := sqliteCheckedColumns(`CREATE TABLE x (
    a TEXT NOT NULL CHECK (json_valid(a)), -- a comment, with (parens) and a 'quote
    b INTEGER DEFAULT ',' ,
    "c" INTEGER CHECK (c IN (0, 1)),
    CHECK (a <> b)
) STRICT`)
	if want := []string{"a", "c", "<table>"}; !slices.Equal(got, want) {
		t.Fatalf("checked columns = %v, want %v", got, want)
	}
	if got := sqliteCheckedColumns("no parentheses"); got != nil {
		t.Fatalf("checked columns = %v, want none", got)
	}
}
