package sqlite

import (
	"testing"

	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/storetest"
)

// TestStoreSuite runs the engine-independent store tests (#85, 85-B) against
// SQLite. The tests that look inside this engine stay in this package.
func TestStoreSuite(t *testing.T) {
	storetest.Run(t, storetest.Engine{
		Open: func(t *testing.T) store.APIStore {
			t.Helper()
			s, _ := openTest(t)
			return s
		},
		// A second *Store on the same file is what a second process would hold.
		OpenSecond: func(t *testing.T, first store.APIStore) store.APIStore {
			t.Helper()
			var path string
			must(t, first.(*Store).db.QueryRowContext(t.Context(), `SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&path))
			s, err := Open(t.Context(), path, discard())
			must(t, err)
			t.Cleanup(func() { _ = s.Close() })
			return s
		},
	})
}
