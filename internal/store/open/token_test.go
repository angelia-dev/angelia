package open

import (
	"bytes"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/pgtest"
)

// tokenEngines runs fn on a migrated store of each engine, with its DSN and a
// connection to its database for raw SQL. Postgres is skipped without a test
// server.
func tokenEngines(t *testing.T, fn func(t *testing.T, dsn string, raw *sql.DB)) {
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "a.db")
		dsn := "sqlite://" + path
		migrated(t, dsn)
		raw, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = raw.Close() })
		fn(t, dsn, raw)
	})
	t.Run("postgres", func(t *testing.T) {
		dsn, _ := pgDSN(t)
		migrated(t, dsn)
		raw, err := sql.Open(pgtest.DriverName, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = raw.Close() })
		fn(t, dsn, raw)
	})
}

func migrated(t *testing.T, dsn string) {
	t.Helper()
	st, err := Open(t.Context(), dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

func newToken(id, name string) store.APIToken {
	return store.APIToken{
		ID: id, Name: name, Scope: store.TokenScopeAdmin, Hash: bytes.Repeat([]byte{7}, 32),
		CreatedAt: time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC), CreatedBy: "local",
	}
}

// TokenWriter writes tokens on a schema at 00021 or newer, on both engines,
// and needs no keyring (it takes none).
func TestTokenWriterWrites(t *testing.T) {
	tokenEngines(t, func(t *testing.T, dsn string, _ *sql.DB) {
		w, err := TokenWriter(t.Context(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = w.Close() }()
		if err := w.CreateAPIToken(t.Context(), newToken("01JCZ8W3T6Q9N2H4K7M5R0V8XA", "me")); err != nil {
			t.Fatal(err)
		}
		if n, err := w.CountActiveAdminTokens(t.Context(), time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)); err != nil || n != 1 {
			t.Fatalf("CountActiveAdminTokens = %d, %v", n, err)
		}
	})
}

// A schema older than 00021 is refused with a clear error, and nothing is
// migrated: the version stays where it was.
func TestTokenWriterRefusesAnOldSchema(t *testing.T) {
	tokenEngines(t, func(t *testing.T, dsn string, raw *sql.DB) {
		ctx := t.Context()
		for _, q := range []string{`DROP TABLE api_token`, `DELETE FROM schema_migrations WHERE version_id >= 21`} {
			if _, err := raw.ExecContext(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		_, err := TokenWriter(ctx, dsn)
		if !errors.Is(err, ErrSchemaTooOld) || !strings.Contains(err.Error(), "version 20") || !strings.Contains(err.Error(), "angelia serve") {
			t.Fatalf("TokenWriter on schema 20 = %v", err)
		}
		var v int64
		if err := raw.QueryRowContext(ctx, `SELECT MAX(version_id) FROM schema_migrations`).Scan(&v); err != nil || v != 20 {
			t.Fatalf("schema version after TokenWriter = %d, %v (it migrated)", v, err)
		}
	})
}

// A missing database, or one Angelia never migrated, is ErrNoDatabase, and
// TokenWriter creates nothing.
func TestTokenWriterNeverCreates(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing.db")
		if _, err := TokenWriter(t.Context(), "sqlite://"+path); !errors.Is(err, ErrNoDatabase) {
			t.Fatalf("TokenWriter(missing) = %v", err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("TokenWriter created %s: %v", path, err)
		}
		empty := filepath.Join(t.TempDir(), "empty.db")
		if err := os.WriteFile(empty, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := TokenWriter(t.Context(), "sqlite://"+empty); !errors.Is(err, ErrNoDatabase) {
			t.Fatalf("TokenWriter(empty file) = %v", err)
		}
	})
	t.Run("postgres", func(t *testing.T) {
		dsn, _ := pgDSN(t)
		if _, err := TokenWriter(t.Context(), dsn); !errors.Is(err, ErrNoDatabase) {
			t.Fatalf("TokenWriter(empty schema) = %v", err)
		}
		if err := Exists(t.Context(), dsn); !errors.Is(err, ErrNoDatabase) {
			t.Fatalf("TokenWriter created tables: Exists = %v", err)
		}
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set("search_path", "angelia_no_such_schema")
		u.RawQuery = q.Encode()
		if _, err := TokenWriter(t.Context(), u.String()); !errors.Is(err, ErrNoDatabase) {
			t.Fatalf("TokenWriter(no such schema) = %v", err)
		}
	})
	if _, err := TokenWriter(t.Context(), "host=x password=hunter2hunter2"); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("TokenWriter(keyword DSN) = %v", err)
	}
}
