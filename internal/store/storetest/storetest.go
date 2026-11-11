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

// Engine describes the engine under test.
type Engine struct {
	// Open opens an empty, migrated store on a database of its own and
	// registers its cleanup on t. It is called once per case.
	Open func(t *testing.T) store.APIStore
	// OpenSecond opens a second store value on the same database as first,
	// the stand-in for a second process. It is required.
	OpenSecond func(t *testing.T, first store.APIStore) store.APIStore
}

// Run runs every case of the suite as a subtest. It fails at once when the
// engine description is incomplete, so no case is skipped silently.
func Run(t *testing.T, e Engine) {
	t.Helper()
	if e.Open == nil || e.OpenSecond == nil {
		t.Fatal("storetest.Run: Engine.Open and Engine.OpenSecond are both required")
	}
	for _, tc := range cases(e) {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, e.Open) })
	}
}

type testCase struct {
	name string
	fn   func(t *testing.T, open opener)
}
