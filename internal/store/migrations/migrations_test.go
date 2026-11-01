package migrations

import (
	"io/fs"
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
