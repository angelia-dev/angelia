package event

import (
	"encoding/json"
	"log/slog"
	"slices"
	"testing"

	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/pgtest"
	"github.com/angelia-dev/angelia/internal/store/postgres"
)

// AC 86-A: on Postgres, a subscription written through one instance (process
// A) invalidates the matcher cache of another (process B) through
// subscription_generation: B's next emit delivers to it.
func TestMatcherCacheSeesSubscriptionsOfAnotherInstanceOnPostgres(t *testing.T) {
	dsn := pgtest.DSN(t)
	open := func() *postgres.Store {
		st, err := postgres.Open(t.Context(), dsn, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return st
	}
	a, b := open(), open()
	tg := seedFixture(t, a)
	cache := NewMatcherCache()
	if got, _ := emitOne(t, b, tg, cache, "9.0.0"); !slices.Equal(got, []string{"s-all", "s-cool", "s-final"}) {
		t.Fatalf("before: delivered %v", got)
	}
	must(t, a.CreateSubscription(t.Context(), store.Subscription{ID: "s-new", SinkID: "k", Filter: json.RawMessage(`{"package":"req*"}`)}))
	if got, _ := emitOne(t, b, tg, cache, "9.1.0"); !slices.Equal(got, []string{"s-all", "s-cool", "s-final", "s-new"}) {
		t.Fatalf("after a subscription written by the other instance: delivered %v", got)
	}
	must(t, a.InConfigTx(t.Context(), func(tx store.ConfigTx) error { return tx.DeleteSubscription(t.Context(), "s-all") }))
	if got, _ := emitOne(t, b, tg, cache, "9.2.0"); !slices.Equal(got, []string{"s-cool", "s-final", "s-new"}) {
		t.Fatalf("after a deletion by the other instance: delivered %v", got)
	}
}
