package postgres

import (
	"testing"

	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/pgtest"
	"github.com/angelia-dev/angelia/internal/store/storetest"
)

// openStore opens a Store on a schema of its own and closes it when the test
// ends, before pgtest drops the schema.
func openStore(t *testing.T) *Store {
	t.Helper()
	return openStoreDSN(t, pgtest.DSN(t))
}

func openStoreDSN(t *testing.T, dsn string) *Store {
	t.Helper()
	s, err := Open(t.Context(), dsn, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// dsns remembers the DSN each store was opened on, for OpenSecond.
type dsns map[store.APIStore]string

// TestStoreSuite runs the engine-independent store tests (#85) against
// Postgres, each case on a schema of its own.
func TestStoreSuite(t *testing.T) {
	opened := dsns{}
	storetest.Run(t, storetest.Engine{
		Open: func(t *testing.T) store.APIStore {
			t.Helper()
			dsn := pgtest.DSN(t)
			s := openStoreDSN(t, dsn)
			opened[s] = dsn
			return s
		},
		// A second *Store on the same schema is what a second process holds.
		OpenSecond: func(t *testing.T, first store.APIStore) store.APIStore {
			t.Helper()
			return openStoreDSN(t, opened[first])
		},
	})
}
