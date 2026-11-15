package open

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/pgtest"
	"github.com/angelia-dev/angelia/internal/store/postgres"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
)

func TestEngineChoosesFromTheDSNScheme(t *testing.T) {
	for _, c := range []struct {
		dsn  string
		want Engine
		bad  bool
	}{
		{"sqlite:///var/lib/angelia/angelia.db", SQLite, false},
		{"SQLite:///var/lib/angelia/angelia.db", SQLite, false},
		{"postgres://u@h/db", Postgres, false},
		{"postgresql://u@h/db", Postgres, false},
		{"POSTGRES://u@h/db", Postgres, false},
		// Not URLs: config validation refuses them at load, and so does the opener.
		{"host=/var/run/postgresql dbname=angelia", "", true},
		{"/var/lib/angelia/angelia.db", "", true},
		{"file:angelia.db", "", true},
		{"mysql://u@h/db", "", true},
		{"", "", true},
	} {
		got, err := EngineOf(c.dsn)
		if c.bad != (err != nil) || got != c.want {
			t.Errorf("EngineOf(%q) = %q, %v; want %q (bad=%v)", c.dsn, got, err, c.want, c.bad)
		}
		if err != nil && strings.Contains(err.Error(), "host=") {
			t.Errorf("EngineOf(%q) error shows the DSN: %v", c.dsn, err)
		}
	}
}

func TestOpenSQLiteIsTheDefaultEngine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.db")
	st, err := Open(t.Context(), "sqlite://"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.(*sqlite.Store); !ok {
		t.Fatalf("Open(sqlite) returned %T", st)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRefusesANonDSN(t *testing.T) {
	if _, err := Open(t.Context(), "host=127.0.0.1 password=hunter2hunter2", nil); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("Open(keyword DSN) = %v", err)
	}
}

// pgDSN returns a schema of the test server as a URL DSN, the only form the
// opener takes; a keyword/value test DSN skips the test.
func pgDSN(t *testing.T) (dsn, schema string) {
	t.Helper()
	dsn, schema = pgtest.Schema(t)
	if _, err := EngineOf(dsn); err != nil {
		t.Skip("needs a URL-form ANGELIA_TEST_POSTGRES_DSN: " + err.Error())
	}
	return dsn, schema
}

func TestOpenPostgres(t *testing.T) {
	dsn, _ := pgDSN(t)
	st, err := Open(t.Context(), dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, ok := st.(*postgres.Store); !ok {
		t.Fatalf("Open(postgres) returned %T", st)
	}
}

// The read-only helpers never create or migrate a database, on either engine.
func TestSQLiteHelpersNeverCreateTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	dsn := "sqlite://" + path
	checkNoDatabase(t, dsn)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a helper created %s: %v", path, err)
	}
}

func TestPostgresHelpersNeverCreateOrMigrate(t *testing.T) {
	dsn, schema := pgDSN(t)
	ctx := t.Context()
	// An existing database with no Angelia tables: every helper reads empty
	// (like a SQLite file the secret migration has not reached), Exists says
	// it is not initialised, and nothing is created.
	if c, err := SecretKeyCounts(ctx, dsn); err != nil || len(c) != 0 {
		t.Fatalf("SecretKeyCounts = %v, %v", c, err)
	}
	if h, err := SecretHosts(ctx, dsn); err != nil || len(h) != 0 {
		t.Fatalf("SecretHosts = %v, %v", h, err)
	}
	if s, err := StoredTargetSources(ctx, dsn); err != nil || len(s) != 0 {
		t.Fatalf("StoredTargetSources = %v, %v", s, err)
	}
	if err := Exists(ctx, dsn); !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("Exists on an empty schema = %v, want ErrNoDatabase", err)
	}
	l, err := SecretReader(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.SecretByName(ctx, "x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("SecretByName = %v", err)
	}
	_ = l.Close()
	db, err := sql.Open(pgtest.DriverName, pgtest.ServerDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	q := `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1`
	if err := db.QueryRowContext(ctx, q, schema).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the helpers left %d relations in the schema (%v)", n, err)
	}

	// After the store created the schema, the helpers see its rows.
	st, err := Open(ctx, dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if err := st.InSecretTx(ctx, func(tx store.SecretTx) error {
		return tx.CreateSecret(ctx, store.Secret{ID: "s1", Name: "tok", Hosts: []string{"h.example"}, KeyID: "k", Ciphertext: []byte("c"), CreatedAt: now, UpdatedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	if err := Exists(ctx, dsn); err != nil {
		t.Fatalf("Exists = %v", err)
	}
	if c, err := SecretKeyCounts(ctx, dsn); err != nil || c["k"] != 1 {
		t.Fatalf("SecretKeyCounts = %v, %v", c, err)
	}
	if h, err := SecretHosts(ctx, dsn); err != nil || len(h["tok"]) != 1 {
		t.Fatalf("SecretHosts = %v, %v", h, err)
	}
}

func TestMissingPostgresDatabaseIsNoDatabase(t *testing.T) {
	dsn := "postgres://x@127.0.0.1:1/none?sslmode=disable"
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	// A refused connection is an error, not "no database": the DSN may be
	// right and the server down.
	if _, err := SecretKeyCounts(ctx, dsn); err == nil || errors.Is(err, ErrNoDatabase) {
		t.Fatalf("SecretKeyCounts on a dead server = %v", err)
	}
}

func checkNoDatabase(t *testing.T, dsn string) {
	t.Helper()
	ctx := t.Context()
	if _, err := SecretKeyCounts(ctx, dsn); !errors.Is(err, ErrNoDatabase) {
		t.Errorf("SecretKeyCounts = %v", err)
	}
	if _, err := SecretHosts(ctx, dsn); !errors.Is(err, ErrNoDatabase) {
		t.Errorf("SecretHosts = %v", err)
	}
	if _, err := StoredTargetSources(ctx, dsn); !errors.Is(err, ErrNoDatabase) {
		t.Errorf("StoredTargetSources = %v", err)
	}
	if _, err := SecretReader(ctx, dsn); !errors.Is(err, ErrNoDatabase) {
		t.Errorf("SecretReader = %v", err)
	}
	if err := Exists(ctx, dsn); !errors.Is(err, ErrNoDatabase) {
		t.Errorf("Exists = %v", err)
	}
}
