package scheduler

import (
	"log/slog"
	"testing"

	"github.com/angelia-dev/angelia/internal/store/pgtest"
	"github.com/angelia-dev/angelia/internal/store/postgres"
)

var _ Store = (*postgres.Store)(nil)

func openPostgres(t *testing.T, dsn string) *postgres.Store {
	t.Helper()
	st, err := postgres.Open(t.Context(), dsn, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// AC 86-A: on Postgres, schedulers on two store instances (two processes)
// never check a target twice at once, and check each one once per round.
func TestNoTargetCheckedTwiceConcurrentlyOnPostgres(t *testing.T) {
	dsn := pgtest.DSN(t)
	noTargetCheckedTwice(t, openPostgres(t, dsn), openPostgres(t, dsn))
}
