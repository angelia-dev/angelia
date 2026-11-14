package migrations

import (
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestSQLiteMigrationsEmbedded(t *testing.T) {
	names, err := fs.Glob(SQLite(), "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) < 2 || names[0] != "00001_initial_schema.sql" {
		t.Fatalf("embedded migrations = %v", names)
	}
}

func TestPostgresMigrationsEmbedded(t *testing.T) {
	names, err := fs.Glob(Postgres(), "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) < 1 || names[0] != "00020_initial_schema.sql" {
		t.Fatalf("embedded migrations = %v", names)
	}
}

// versions returns the version numbers of the migrations in fsys, in order.
func versions(t *testing.T, fsys fs.FS) []int {
	t.Helper()
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	out := make([]int, 0, len(names))
	for _, n := range names {
		num, _, ok := strings.Cut(n, "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil {
			t.Fatalf("migration %q is not named NNNNN_description.sql", n)
		}
		out = append(out, v)
	}
	return out
}

// TestEveryMigrationShipsBothEngines checks ADR 0028 §7: Postgres starts with
// one initial migration at the SQLite head of its day, and from there on every
// SQLite migration has a Postgres twin with the same number, and the reverse.
func TestEveryMigrationShipsBothEngines(t *testing.T) {
	sq, pg := versions(t, SQLite()), versions(t, Postgres())
	first := pg[0]
	if !slices.Contains(sq, first) {
		t.Fatalf("the initial Postgres migration %05d has no SQLite migration with the same number", first)
	}
	i := slices.Index(sq, first)
	if want := sq[i:]; !slices.Equal(pg, want) {
		t.Fatalf("Postgres migrations %v, want one twin per SQLite migration from %05d: %v", pg, first, want)
	}
}
