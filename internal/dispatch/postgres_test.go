package dispatch

import (
	"testing"

	"github.com/angelia-dev/angelia/internal/store/pgtest"
	"github.com/angelia-dev/angelia/internal/store/postgres"
)

var _ Store = (*postgres.Store)(nil)

func openPostgres(t testing.TB, dsn string) *postgres.Store {
	t.Helper()
	st, err := postgres.Open(t.Context(), dsn, discard())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// AC 86-A: on Postgres, dispatchers on their own store instances (one per
// process) never send a delivery twice or at the same time, and record every
// outcome.
func TestConcurrentDispatchersNeverOverlapOnPostgres(t *testing.T) {
	dsn := pgtest.DSN(t)
	st := openPostgres(t, dsn)
	seedSink(t, st)
	dispatchersNeverOverlap(t, st, func() Store { return openPostgres(t, dsn) })
}
