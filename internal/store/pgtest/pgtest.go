// Package pgtest gives each test its own Postgres schema on the server named
// by ANGELIA_TEST_POSTGRES_DSN (story #85, ADR 0028 §6).
//
// Without the variable a test that asks for Postgres is skipped, unless
// ANGELIA_TEST_POSTGRES_REQUIRED=1, which makes it fail instead, so CI cannot
// pass silently without Postgres. Each call creates a schema with a random
// name, drops it (with everything in it) when the test ends, and returns the
// DSN with search_path set to it. Packages can therefore run in parallel
// against one server, and the advisory-lock keys of the engine, derived from
// current_schema(), never collide between tests.
package pgtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
)

const (
	// EnvDSN names the variable that holds the DSN of the test server, for
	// example postgres://postgres:angelia@localhost:5432/postgres?sslmode=disable.
	EnvDSN = "ANGELIA_TEST_POSTGRES_DSN"
	// EnvRequired names the variable that, set to 1, makes a test without
	// EnvDSN fail instead of skip.
	EnvRequired = "ANGELIA_TEST_POSTGRES_REQUIRED"

	// SchemaPrefix starts the name of every schema this package creates.
	SchemaPrefix = "angelia_test_"

	// DriverName is the database/sql driver name of pgx/v5/stdlib.
	DriverName = "pgx"

	cleanupTimeout = 30 * time.Second
)

// errNoServer says that EnvDSN is unset.
var errNoServer = errors.New(EnvDSN + " is not set")

// serverDSN returns the server DSN from getenv. With no DSN, it returns
// errNoServer and whether the test must fail rather than skip.
func serverDSN(getenv func(string) string) (dsn string, required bool, err error) {
	dsn = strings.TrimSpace(getenv(EnvDSN))
	if dsn == "" {
		return "", getenv(EnvRequired) == "1", errNoServer
	}
	return dsn, false, nil
}

// ServerDSN returns the DSN of the test server, as set in EnvDSN. Without it,
// it skips the test, or fails it when EnvRequired is 1.
func ServerDSN(t testing.TB) string {
	t.Helper()
	dsn, required, err := serverDSN(os.Getenv)
	if err != nil {
		if required {
			t.Fatalf("pgtest: %v and %s=1", err, EnvRequired)
		}
		t.Skipf("pgtest: %v; skipping the Postgres test", err)
	}
	return dsn
}

// DSN creates a schema for the test and returns a DSN whose search_path is
// that schema. See [Schema].
func DSN(t testing.TB) string {
	t.Helper()
	dsn, _ := Schema(t)
	return dsn
}

// Schema creates a schema with a random name for the test, registers its
// removal (DROP SCHEMA … CASCADE) as a cleanup, and returns a DSN whose
// search_path is that schema, and the schema's name. Close every database
// opened on the DSN in a cleanup registered after this call, so it runs first.
func Schema(t testing.TB) (dsn, name string) {
	t.Helper()
	server := ServerDSN(t)
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("pgtest: schema name: %v", err)
	}
	name = SchemaPrefix + hex.EncodeToString(b[:])
	dsn, err := WithSearchPath(server, name)
	if err != nil {
		t.Fatalf("pgtest: %v", err)
	}
	db, err := sql.Open(DriverName, server)
	if err != nil {
		t.Fatalf("pgtest: open: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if _, err := db.ExecContext(ctx, "DROP SCHEMA "+pgx.Identifier{name}.Sanitize()+" CASCADE"); err != nil {
			t.Errorf("pgtest: drop schema %s: %v", name, err)
		}
		if err := db.Close(); err != nil {
			t.Errorf("pgtest: close: %v", err)
		}
	})
	if _, err := db.ExecContext(t.Context(), "CREATE SCHEMA "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("pgtest: create schema %s: %v", name, err)
	}
	return dsn, name
}

// WithSearchPath returns dsn with its search_path set to schema, replacing
// any search_path it had. It accepts both DSN forms pgx reads: a postgres://
// or postgresql:// URL, and keyword/value pairs. Errors never include the DSN,
// which may hold a password.
func WithSearchPath(dsn, schema string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", errors.New("the DSN is not a valid URL")
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	// Keyword/value form: a later keyword overrides an earlier one.
	return fmt.Sprintf("%s search_path='%s'", dsn, strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(schema)), nil
}
