package sqlite

import (
	"testing"

	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/storetest"
)

// TestStoreSuite runs the engine-independent store tests (#85, 85-B) against
// SQLite. The tests that look inside this engine stay in this package.
func TestStoreSuite(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.APIStore {
		t.Helper()
		s, _ := openTest(t)
		return s
	})
}
