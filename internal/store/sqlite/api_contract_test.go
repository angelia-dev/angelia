package sqlite

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
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

// sinkColumns lists the columns of the sink table.
func sinkColumns(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT name FROM pragma_table_info('sink')`)
	must(t, err)
	defer func() { must(t, rows.Close()) }()
	cols := map[string]bool{}
	for rows.Next() {
		var n string
		must(t, rows.Scan(&n))
		cols[n] = true
	}
	must(t, rows.Err())
	return cols
}

// Story #197: migration 00016 renames sink.config_json_encrypted to
// config_json and keeps every row; Down renames it back, also keeping rows.
func TestSinkConfigRenameMigration(t *testing.T) {
	ctx := t.Context()
	db := migratedTo(t, 15)
	if c := sinkColumns(t, db); !c["config_json_encrypted"] || c["config_json"] {
		t.Fatalf("before: columns %v", c)
	}
	execAll(t, db, `INSERT INTO sink (id, kind, config_json_encrypted) VALUES ('k1','webhook',x'7b7d')`)

	p, err := newProvider(db)
	must(t, err)
	_, err = p.UpTo(ctx, 16)
	must(t, err)
	if c := sinkColumns(t, db); c["config_json_encrypted"] || !c["config_json"] {
		t.Fatalf("after up: columns %v", c)
	}
	var cfg []byte
	must(t, db.QueryRowContext(ctx, `SELECT config_json FROM sink WHERE id = 'k1'`).Scan(&cfg))
	if string(cfg) != "{}" {
		t.Fatalf("config after up = %q", cfg)
	}

	_, err = p.DownTo(ctx, 15)
	must(t, err)
	if c := sinkColumns(t, db); !c["config_json_encrypted"] || c["config_json"] {
		t.Fatalf("after down: columns %v", c)
	}
	must(t, db.QueryRowContext(ctx, `SELECT config_json_encrypted FROM sink WHERE id = 'k1'`).Scan(&cfg))
	if string(cfg) != "{}" {
		t.Fatalf("config after down = %q", cfg)
	}
}

func targetColumns(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT name FROM pragma_table_info('target')`)
	must(t, err)
	defer func() { _ = rows.Close() }()
	cols := map[string]bool{}
	for rows.Next() {
		var n string
		must(t, rows.Scan(&n))
		cols[n] = true
	}
	must(t, rows.Err())
	return cols
}

// Story #205: migration 00017 adds the nullable target.source_kind. It
// rewrites no row (existing targets read NULL, "the same as kind"), and Down
// drops the column and keeps the targets.
func TestTargetSourceKindMigration(t *testing.T) {
	ctx := t.Context()
	db := migratedTo(t, 16)
	if targetColumns(t, db)["source_kind"] {
		t.Fatal("before: source_kind exists")
	}
	execAll(t, db, `INSERT INTO target (id, kind, coordinates_json, next_check_at) VALUES ('t1','npm','{"name":"x"}',1)`)

	p, err := newProvider(db)
	must(t, err)
	_, err = p.UpTo(ctx, 17)
	must(t, err)
	if !targetColumns(t, db)["source_kind"] {
		t.Fatal("after up: no source_kind")
	}
	var kind, coords string
	var sk sql.NullString
	must(t, db.QueryRowContext(ctx, `SELECT kind, coordinates_json, source_kind FROM target WHERE id = 't1'`).Scan(&kind, &coords, &sk))
	if kind != "npm" || coords != `{"name":"x"}` || sk.Valid {
		t.Fatalf("after up: kind %q coordinates %q source_kind %v, want the row untouched and NULL", kind, coords, sk)
	}

	_, err = p.DownTo(ctx, 16)
	must(t, err)
	if targetColumns(t, db)["source_kind"] {
		t.Fatal("after down: source_kind remains")
	}
	must(t, db.QueryRowContext(ctx, `SELECT kind, coordinates_json FROM target WHERE id = 't1'`).Scan(&kind, &coords))
	if kind != "npm" || coords != `{"name":"x"}` {
		t.Fatalf("after down: kind %q coordinates %q", kind, coords)
	}
}
