package postgres

import (
	"net/url"
	"strings"
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

// withServerOption adds a server setting to dsn through its options
// parameter, in either DSN form.
func withServerOption(t *testing.T, dsn, option string) string {
	t.Helper()
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set("options", option)
		u.RawQuery = q.Encode()
		return u.String()
	}
	return dsn + " options='" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(option) + "'"
}

// repeatableRead makes REPEATABLE READ the session default, as a server or
// role setting could.
const repeatableRead = `-c default_transaction_isolation=repeatable\ read`

// TestStoreSuiteUnderRepeatableReadDefault: the engine does not depend on the
// server's default isolation level; the whole suite, its concurrency cases
// included, passes when the session default is REPEATABLE READ.
func TestStoreSuiteUnderRepeatableReadDefault(t *testing.T) {
	opened := dsns{}
	storetest.Run(t, storetest.Engine{
		Open: func(t *testing.T) store.APIStore {
			t.Helper()
			dsn := withServerOption(t, pgtest.DSN(t), repeatableRead)
			s := openStoreDSN(t, dsn)
			opened[s] = dsn
			return s
		},
		OpenSecond: func(t *testing.T, first store.APIStore) store.APIStore {
			t.Helper()
			return openStoreDSN(t, opened[first])
		},
	})
}

// TestTransactionsRunReadCommitted: with REPEATABLE READ as the session
// default, the engine's transactions and single statements still run READ
// COMMITTED (the DSN setting is overridden).
func TestTransactionsRunReadCommitted(t *testing.T) {
	s := openStoreDSN(t, withServerOption(t, pgtest.DSN(t), repeatableRead))
	ctx := t.Context()
	var inTx, def string
	must(t, s.withTx(ctx, lockNone, func(q *Queries) error {
		return q.db.QueryRowContext(ctx, `SHOW transaction_isolation`).Scan(&inTx)
	}))
	must(t, s.db.QueryRowContext(ctx, `SHOW default_transaction_isolation`).Scan(&def))
	if inTx != "read committed" || def != "read committed" {
		t.Fatalf("transaction %q, session default %q; want read committed", inTx, def)
	}
}

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
