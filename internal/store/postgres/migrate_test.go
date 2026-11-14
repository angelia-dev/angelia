package postgres

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/angelia-dev/angelia/internal/store/pgtest"
)

func TestMigrateIsIdempotentAndLogs(t *testing.T) {
	db := openRaw(t, pgtest.DSN(t))
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	n, err := Migrate(t.Context(), db, logger)
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatalf("applied %d migrations on an empty schema", n)
	}
	if !strings.Contains(logs.String(), "00020_initial_schema.sql") {
		t.Fatalf("log lacks the applied migration:\n%s", logs.String())
	}
	if n, err := Migrate(t.Context(), db, nil); err != nil || n != 0 {
		t.Fatalf("second Migrate = %d, %v; want 0, nil", n, err)
	}
}

// TestConcurrentStartup: processes migrating the same empty schema at once
// all succeed, and goose records each version once (ADR 0028 §7). Each
// process has its own pool, as separate processes would.
func TestConcurrentStartup(t *testing.T) {
	dsn := pgtest.DSN(t)
	const processes = 3
	var wg sync.WaitGroup
	applied := make([]int, processes)
	errs := make([]error, processes)
	for i := range processes {
		db := openRaw(t, dsn)
		wg.Go(func() { applied[i], errs[i] = Migrate(t.Context(), db, discardLogger()) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, n := range applied {
		total += n
	}
	db := openRaw(t, dsn)
	if want := int(count(t, db, `SELECT count(*) FROM `+migrationsTable+` WHERE version_id > 0`)); total != want {
		t.Fatalf("processes applied %v migrations in total, the schema has %d", applied, want)
	}
	if n := count(t, db, `SELECT count(*) FROM (SELECT version_id FROM `+migrationsTable+` GROUP BY version_id HAVING count(*) > 1) d`); n != 0 {
		t.Fatalf("%d versions recorded more than once", n)
	}
	if n := count(t, db, `SELECT count(*) FROM target`); n != 0 {
		t.Fatal("the schema is not usable")
	}
}

// TestSchemaKey: the key is stable for one schema and differs between schemas,
// so advisory locks of two tests or two instances never contend.
func TestSchemaKey(t *testing.T) {
	dsnA := pgtest.DSN(t)
	a1, err := SchemaKey(t.Context(), openRaw(t, dsnA))
	if err != nil {
		t.Fatal(err)
	}
	a2, err := SchemaKey(t.Context(), openRaw(t, dsnA))
	if err != nil {
		t.Fatal(err)
	}
	b, err := SchemaKey(t.Context(), openRaw(t, pgtest.DSN(t)))
	if err != nil {
		t.Fatal(err)
	}
	if a1 != a2 || a1 == b {
		t.Fatalf("keys: schema A %d and %d, schema B %d", a1, a2, b)
	}
	if id := migrationLockID(a1); id>>32 != int64(a1) || id&0xffffffff != 0 {
		t.Fatalf("migration lock id %#x does not read as (%d, 0)", id, a1)
	}
}

// TestNoSchema: a search_path that names no existing schema is refused before
// anything is created.
func TestNoSchema(t *testing.T) {
	dsn, err := pgtest.WithSearchPath(pgtest.ServerDSN(t), "angelia_test_does_not_exist")
	if err != nil {
		t.Fatal(err)
	}
	db := openRaw(t, dsn)
	if _, err := SchemaKey(t.Context(), db); !errors.Is(err, ErrNoSchema) {
		t.Fatalf("SchemaKey = %v, want ErrNoSchema", err)
	}
	if _, err := Migrate(t.Context(), db, nil); !errors.Is(err, ErrNoSchema) {
		t.Fatalf("Migrate = %v, want ErrNoSchema", err)
	}
}

func TestMigrationLockID(t *testing.T) {
	for _, key := range []int32{0, 1, -1, 1 << 30, -1 << 31} {
		id := migrationLockID(key)
		if id>>32 != int64(key) || id&0xffffffff != 0 {
			t.Errorf("migrationLockID(%d) = %#x", key, id)
		}
	}
}
