package pgtest

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestServerDSNFromEnvironment(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	tests := []struct {
		name         string
		vars         map[string]string
		wantDSN      string
		wantRequired bool
		wantErr      bool
	}{
		{name: "set", vars: map[string]string{EnvDSN: " postgres://h/db "}, wantDSN: "postgres://h/db"},
		{name: "unset skips", vars: map[string]string{}, wantErr: true},
		{name: "unset but required fails", vars: map[string]string{EnvRequired: "1"}, wantRequired: true, wantErr: true},
		{name: "required only when 1", vars: map[string]string{EnvRequired: "true"}, wantErr: true},
		{name: "required ignored when set", vars: map[string]string{EnvDSN: "postgres://h/db", EnvRequired: "1"}, wantDSN: "postgres://h/db"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dsn, required, err := serverDSN(env(tt.vars))
			if dsn != tt.wantDSN || required != tt.wantRequired || (err != nil) != tt.wantErr {
				t.Fatalf("serverDSN = %q, %v, %v; want %q, %v, error %v", dsn, required, err, tt.wantDSN, tt.wantRequired, tt.wantErr)
			}
			if err != nil && !errors.Is(err, errNoServer) {
				t.Fatalf("err = %v, want errNoServer", err)
			}
		})
	}
}

func TestWithSearchPath(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
	}{
		{name: "url", dsn: "postgres://u@localhost:5432/db?sslmode=disable"},
		{name: "postgresql url", dsn: "postgresql://u@localhost/db"},
		{name: "url replaces search_path", dsn: "postgres://u@localhost/db?search_path=public&sslmode=disable"},
		{name: "keyword value", dsn: "host=localhost user=u dbname=db sslmode=disable"},
		{name: "keyword value replaces search_path", dsn: "host=localhost search_path=public user=u"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := WithSearchPath(tt.dsn, "angelia_test_x")
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := pgconn.ParseConfig(got)
			if err != nil {
				t.Fatalf("pgconn cannot parse %q: %v", got, err)
			}
			if sp := cfg.RuntimeParams["search_path"]; sp != "angelia_test_x" {
				t.Fatalf("search_path = %q in %q", sp, got)
			}
			if cfg.Host != "localhost" {
				t.Fatalf("host = %q, the rest of the DSN was lost: %q", cfg.Host, got)
			}
		})
	}
}

func TestWithSearchPathBadURLHidesDSN(t *testing.T) {
	_, err := WithSearchPath("postgres://u:hunter2@[::1", "s")
	if err == nil {
		t.Fatal("want an error for an invalid URL")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error leaks the password: %v", err)
	}
}

// TestSchemaIsolatedAndDropped runs on a Postgres server: the schema is the
// connection's current schema, two tests get different schemas, and the
// schema is gone after the test.
func TestSchemaIsolatedAndDropped(t *testing.T) {
	server := ServerDSN(t)
	var first, second string
	t.Run("a", func(t *testing.T) {
		dsn, name := Schema(t)
		first = name
		if got := currentSchema(t, dsn); got != name {
			t.Fatalf("current_schema() = %q, want %q", got, name)
		}
	})
	t.Run("b", func(t *testing.T) {
		second = DSN(t)
	})
	if first == "" || second == "" || strings.Contains(second, first) {
		t.Fatalf("schemas not distinct: %q, DSN %q", first, second)
	}
	db, err := sql.Open(DriverName, server)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_namespace WHERE nspname = $1`, first).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("schema %s still exists after its test", first)
	}
}

func currentSchema(t *testing.T, dsn string) string {
	t.Helper()
	db, err := sql.Open(DriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var s sql.NullString
	if err := db.QueryRowContext(t.Context(), `SELECT current_schema()`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s.String
}
