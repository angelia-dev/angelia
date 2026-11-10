// Package storetest is the engine-independent test suite of the store
// contracts (store.Store, store.APIStore and store.SecretStore). An engine
// calls Run from its own tests with a function that opens an empty store;
// the SQLite engine does so today and the Postgres engine will (#85).
//
// Tests that look inside an engine (query plans, pragmas, migrations from an
// older schema) stay in the engine's package.
package storetest

import (
	"testing"

	"github.com/angelia-dev/angelia/internal/store"
)

// opener opens an empty, migrated store and registers its cleanup on t.
type opener func(t *testing.T) store.APIStore

// Run runs every case of the suite as a subtest. open is called once per case
// and must return an empty store on a database of its own.
func Run(t *testing.T, open func(t *testing.T) store.APIStore) {
	t.Helper()
	for _, tc := range cases() {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, open) })
	}
}

type testCase struct {
	name string
	fn   func(t *testing.T, open opener)
}
