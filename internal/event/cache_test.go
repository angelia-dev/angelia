package event

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/policy"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
)

// countingStore counts how many times the subscriptions are listed, outside
// and inside a transaction.
type countingStore struct {
	*sqlite.Store
	outside, inside atomic.Int64
}

func (c *countingStore) ListSubscriptions(ctx context.Context) ([]store.Subscription, error) {
	c.outside.Add(1)
	return c.Store.ListSubscriptions(ctx)
}

func (c *countingStore) InTx(ctx context.Context, fn func(tx store.Tx) error) error {
	return c.Store.InTx(ctx, func(tx store.Tx) error { return fn(&countingTx{Tx: tx, c: c}) })
}

type countingTx struct {
	store.Tx
	c *countingStore
}

func (t *countingTx) ListSubscriptions(ctx context.Context) ([]store.Subscription, error) {
	t.c.inside.Add(1)
	return t.Tx.ListSubscriptions(ctx)
}

// emitOne emits one release of watch wNow through cache and returns the ids
// of the subscriptions that got a live delivery of it, and of the dead ones.
func emitOne(t *testing.T, st store.Store, tg store.Target, cache *MatcherCache, raw string) (delivered, dead []string) {
	t.Helper()
	ctx := t.Context()
	rel := []policy.WatchReleases{{
		Watch:    store.Watch{ID: wNow, Labels: map[string]string{"team": "platform"}},
		Releases: []policy.Release{{Raw: raw, Scheme: "semver", DetectedAt: check2}},
	}}
	err := st.InTx(ctx, func(tx store.Tx) error {
		_, err := Emit(ctx, tx, rel, EmitInput{Target: tg, Instance: "angelia", Now: check2, Matcher: cache})
		return err
	})
	must(t, err)
	evs, err := st.ListEventsAfter(ctx, 0, 0)
	must(t, err)
	ds, err := st.ListDeliveries(ctx, evs[len(evs)-1].ID)
	must(t, err)
	for _, d := range ds {
		if d.Status == store.DeliveryDead {
			dead = append(dead, d.SubscriptionID)
		} else {
			delivered = append(delivered, d.SubscriptionID)
		}
	}
	sort.Strings(delivered)
	sort.Strings(dead)
	return delivered, dead
}

func TestEmitSeesSubscriptionsAddedEditedAndDeletedThroughTheCache(t *testing.T) {
	st, tg := fixture(t)
	ctx := t.Context()
	cache := NewMatcherCache()
	check := func(step string, wantDelivered ...string) {
		t.Helper()
		got, dead := emitOne(t, st, tg, cache, "9."+step)
		if !slices.Equal(got, wantDelivered) || !slices.Equal(dead, []string{"s-broken"}) {
			t.Errorf("%s: delivered %v dead %v, want delivered %v dead [s-broken]", step, got, dead, wantDelivered)
		}
	}
	check("0.0", "s-all", "s-cool", "s-final")

	// Added.
	must(t, st.CreateSubscription(ctx, store.Subscription{ID: "s-new", SinkID: "k", Filter: json.RawMessage(`{"package":"req*"}`)}))
	check("1.0", "s-all", "s-cool", "s-final", "s-new")

	// Edited: s-all now takes npm only; s-new gets a policy that drops pre-releases only, a final one stays.
	must(t, st.InConfigTx(ctx, func(tx store.ConfigTx) error {
		return tx.UpdateSubscription(ctx, store.Subscription{ID: "s-all", SinkID: "k", Filter: json.RawMessage(`{"source":["npm"]}`)})
	}))
	check("2.0", "s-cool", "s-final", "s-new")

	// An edit that makes a stored filter invalid turns it into a dead delivery.
	must(t, st.InConfigTx(ctx, func(tx store.ConfigTx) error {
		return tx.UpdateSubscription(ctx, store.Subscription{ID: "s-final", SinkID: "k", Filter: json.RawMessage(`{"nope":1}`)})
	}))
	got, dead := emitOne(t, st, tg, cache, "3.0.0")
	if !slices.Equal(got, []string{"s-cool", "s-new"}) || !slices.Equal(dead, []string{"s-broken", "s-final"}) {
		t.Errorf("invalidated: delivered %v dead %v", got, dead)
	}

	// Deleted.
	must(t, st.InConfigTx(ctx, func(tx store.ConfigTx) error { return tx.DeleteSubscription(ctx, "s-new") }))
	must(t, st.InConfigTx(ctx, func(tx store.ConfigTx) error { return tx.DeleteSubscription(ctx, "s-final") }))
	got, dead = emitOne(t, st, tg, cache, "4.0.0")
	if !slices.Equal(got, []string{"s-cool"}) || !slices.Equal(dead, []string{"s-broken"}) {
		t.Errorf("deleted: delivered %v dead %v", got, dead)
	}
}

func TestMatcherCacheReloadsOnlyWhenSubscriptionsChange(t *testing.T) {
	base, tg := fixture(t)
	st := &countingStore{Store: base}
	ctx := t.Context()
	cache := NewMatcherCache()

	emitOne(t, st, tg, cache, "9.0.0")
	if st.inside.Load() != 1 {
		t.Fatalf("first emit: %d in-transaction loads, want 1", st.inside.Load())
	}
	for i := range 3 {
		emitOne(t, st, tg, cache, fmt.Sprintf("9.0.%d", i+1))
	}
	if st.inside.Load() != 1 || st.outside.Load() != 0 {
		t.Fatalf("unchanged subscriptions were reloaded: %d inside, %d outside", st.inside.Load(), st.outside.Load())
	}

	must(t, st.CreateSubscription(ctx, store.Subscription{ID: "s-more", SinkID: "k"}))
	emitOne(t, st, tg, cache, "9.1.0")
	emitOne(t, st, tg, cache, "9.1.1")
	if st.inside.Load() != 2 {
		t.Fatalf("after one subscription change: %d in-transaction loads, want 2", st.inside.Load())
	}

	// A nil cache is the old behaviour: one load per Emit.
	before := st.inside.Load()
	emitOne(t, st, tg, nil, "9.2.0")
	emitOne(t, st, tg, nil, "9.2.1")
	if st.inside.Load() != before+2 {
		t.Fatalf("nil cache: %d loads, want %d", st.inside.Load(), before+2)
	}
}

func TestCommitWarmsTheCacheBeforeTheTransaction(t *testing.T) {
	base, tg := fixture(t)
	st := &countingStore{Store: base}
	ctx := t.Context()
	cache := NewMatcherCache()
	in := secondCheck(tg)
	in.Matcher = cache
	n, err := Commit(ctx, st, in)
	if err != nil {
		t.Fatal(err)
	}
	if n != (Emitted{Events: 2, Deliveries: 7, Held: 2, Dead: 2}) {
		t.Fatalf("emitted %+v", n)
	}
	if st.outside.Load() != 1 || st.inside.Load() != 0 {
		t.Fatalf("loads: %d outside, %d inside; want 1 outside and none under the write lock", st.outside.Load(), st.inside.Load())
	}

	// A subscription written between the warm-up and the transaction is
	// still seen: the generation read inside the transaction differs.
	next := CheckInput{Target: tg, Instance: "angelia", Now: check2.Add(time.Hour), Matcher: cache, Fetched: source.Result{Versions: append(
		[]source.Version{pypiVersion("2.35.0", check2.Add(30*time.Minute))}, in.Fetched.Versions...)}}
	must(t, st.CreateSubscription(ctx, store.Subscription{ID: "s-late", SinkID: "k"}))
	if _, err := Commit(ctx, st, next); err != nil {
		t.Fatal(err)
	}
	evs, err := st.ListEventsAfter(ctx, 0, 0)
	must(t, err)
	ds, err := st.ListDeliveries(ctx, evs[len(evs)-1].ID)
	must(t, err)
	if !slices.ContainsFunc(ds, func(d store.Delivery) bool { return d.SubscriptionID == "s-late" }) {
		t.Fatalf("deliveries of the last event miss s-late: %+v", ds)
	}
}

// failingReader fails each read.
type failingReader struct{ gen, list bool }

func (f failingReader) SubscriptionGeneration(context.Context) (int64, error) {
	if f.gen {
		return 0, errCrash
	}
	return 1, nil
}

func (f failingReader) ListSubscriptions(context.Context) ([]store.Subscription, error) {
	if f.list {
		return nil, errCrash
	}
	return nil, nil
}

func TestWarmErrors(t *testing.T) {
	ctx := t.Context()
	logger := slog.New(slog.DiscardHandler)
	for _, r := range []failingReader{{gen: true}, {list: true}} {
		if err := NewMatcherCache().Warm(ctx, r, logger); !errors.Is(err, errCrash) {
			t.Errorf("Warm(%+v) = %v", r, err)
		}
	}
	// Warm on a current cache does not list again.
	c := NewMatcherCache()
	must(t, c.Warm(ctx, failingReader{}, logger))
	must(t, c.Warm(ctx, failingReader{list: true}, logger))
}

// errGenTx fails SubscriptionGeneration.
type errGenTx struct{ store.Tx }

func (errGenTx) SubscriptionGeneration(context.Context) (int64, error) { return 0, errCrash }

func (errGenTx) ListSubscriptions(context.Context) ([]store.Subscription, error) {
	return nil, errCrash
}

func TestEmitWithCacheErrors(t *testing.T) {
	rel := []policy.WatchReleases{{Watch: store.Watch{ID: wNow}, Releases: []policy.Release{{Raw: "1.0.0", Scheme: "semver", DetectedAt: check1}}}}
	tg := store.Target{ID: "t", Kind: "npm", Coordinates: map[string]string{"name": "x"}}
	if _, err := Emit(t.Context(), errGenTx{}, rel, EmitInput{Target: tg, Instance: "angelia", Now: check1, Matcher: NewMatcherCache()}); !errors.Is(err, errCrash) {
		t.Errorf("generation error: %v", err)
	}
	// Generation reads, the cache is empty, listing fails.
	if _, err := Emit(t.Context(), listFailTx{}, rel, EmitInput{Target: tg, Instance: "angelia", Now: check1, Matcher: NewMatcherCache()}); !errors.Is(err, errCrash) {
		t.Errorf("list error: %v", err)
	}
}

type listFailTx struct{ store.Tx }

func (listFailTx) SubscriptionGeneration(context.Context) (int64, error) { return 1, nil }

func (listFailTx) ListSubscriptions(context.Context) ([]store.Subscription, error) {
	return nil, errCrash
}

func TestMatcherCacheConcurrentUse(t *testing.T) {
	base, tg := fixture(t)
	ctx := t.Context()
	cache := NewMatcherCache()
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for g := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 5 {
				if i == 2 && g == 0 {
					if err := base.CreateSubscription(ctx, store.Subscription{ID: "s-conc", SinkID: "k"}); err != nil {
						errs <- err
						return
					}
				}
				if err := cache.Warm(ctx, base, discardLogger()); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got, _ := emitOne(t, base, tg, cache, "8.0.0"); !slices.Contains(got, "s-conc") {
		t.Errorf("delivered %v, want s-conc among them", got)
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// subscriptionsFixture opens a store with n subscriptions of mixed filters.
func subscriptionsFixture(tb testing.TB, n int) (*sqlite.Store, store.Target) {
	tb.Helper()
	ctx := tb.Context()
	st, err := sqlite.Open(ctx, filepath.Join(tb.TempDir(), "b.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = st.Close() })
	tg := store.Target{ID: "t-requests", Kind: "pypi", Coordinates: map[string]string{"name": "requests"}, NextCheckAt: check1}
	for _, err := range []error{
		st.CreateTarget(ctx, tg),
		st.CreateWatch(ctx, store.Watch{ID: wNow, TargetID: tg.ID, Labels: map[string]string{"team": "platform"}}),
		st.CreateSink(ctx, store.Sink{ID: "k", Kind: "webhook", ConfigEncrypted: []byte{1}}),
	} {
		if err != nil {
			tb.Fatal(err)
		}
	}
	for i := range n {
		var f string
		switch i % 5 {
		case 0: // a handful take this package
			f = fmt.Sprintf(`{"package":"req*%d","type":["release.published"]}`, i)
			if i%1000 == 0 {
				f = `{"package":"requests","type":["release.published"]}`
			}
		case 1:
			f = fmt.Sprintf(`{"source":["npm"],"package":"@scope%d/*"}`, i)
		case 2:
			f = fmt.Sprintf(`{"labels":{"team":"team-%d"}}`, i)
		case 3:
			f = `{"source":["pypi"],"bump":["major"]}`
		default:
			f = fmt.Sprintf(`{"package":"pkg-%d","bump":["minor","patch"]}`, i)
		}
		err := st.CreateSubscription(ctx, store.Subscription{
			ID: fmt.Sprintf("s-%05d", i), SinkID: "k", Filter: json.RawMessage(f), Policy: json.RawMessage(`{"cooldown":"1h"}`),
		})
		if err != nil {
			tb.Fatal(err)
		}
	}
	return st, tg
}

// emitTx runs one Emit in a transaction and returns how long the
// transaction (the write lock) lasted.
func emitTx(tb testing.TB, st *sqlite.Store, tg store.Target, cache *MatcherCache, raw string) time.Duration {
	tb.Helper()
	ctx := tb.Context()
	rel := []policy.WatchReleases{{
		Watch:    store.Watch{ID: wNow},
		Releases: []policy.Release{{Raw: raw, Scheme: "semver", DetectedAt: check2}},
	}}
	start := time.Now()
	err := st.InTx(ctx, func(tx store.Tx) error {
		_, err := Emit(ctx, tx, rel, EmitInput{Target: tg, Instance: "angelia", Now: check2, Matcher: cache})
		return err
	})
	d := time.Since(start)
	if err != nil {
		tb.Fatal(err)
	}
	return d
}

// TestEmitHoldsTheTransactionBriefly checks the #166 target: with 10,000
// subscriptions, a warmed cache keeps the write transaction of an Emit under
// 5 ms. The median of several runs is compared, to ride out a noisy host; the
// race detector slows the matcher several times over, so the bound is only
// asserted without it.
func TestEmitHoldsTheTransactionBriefly(t *testing.T) {
	st, tg := subscriptionsFixture(t, 10_000)
	cache := NewMatcherCache()
	if err := cache.Warm(t.Context(), st, discardLogger()); err != nil {
		t.Fatal(err)
	}
	var cached, uncached []time.Duration
	for i := range 15 {
		cached = append(cached, emitTx(t, st, tg, cache, fmt.Sprintf("1.0.%d", i)))
	}
	for i := range 3 {
		uncached = append(uncached, emitTx(t, st, tg, nil, fmt.Sprintf("2.0.%d", i)))
	}
	slices.Sort(cached)
	slices.Sort(uncached)
	c, u := cached[len(cached)/2], uncached[len(uncached)/2]
	t.Logf("10k subscriptions: median transaction %v with the cache, %v without", c, u)
	if !raceEnabled && c >= 5*time.Millisecond {
		t.Errorf("median transaction with a warm cache = %v, want < 5ms", c)
	}
	if c*3 > u {
		t.Errorf("the cache saves too little: %v with, %v without", c, u)
	}
}

func BenchmarkEmit10kSubscriptions(b *testing.B) {
	st, tg := subscriptionsFixture(b, 10_000)
	for _, tc := range []struct {
		name  string
		cache *MatcherCache
	}{{"cached", NewMatcherCache()}, {"uncached", nil}} {
		b.Run(tc.name, func(b *testing.B) {
			if tc.cache != nil {
				if err := tc.cache.Warm(b.Context(), st, discardLogger()); err != nil {
					b.Fatal(err)
				}
			}
			var worst time.Duration
			for i := 0; b.Loop(); i++ {
				worst = max(worst, emitTx(b, st, tg, tc.cache, fmt.Sprintf("3.%s.%d", tc.name, i)))
			}
			b.ReportMetric(float64(worst.Microseconds())/1000, "ms-worst-tx")
		})
	}
}
