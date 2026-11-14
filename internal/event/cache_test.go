package event

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
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
	outside, inside, gens atomic.Int64
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

func (t *countingTx) SubscriptionGeneration(ctx context.Context) (int64, error) {
	t.c.gens.Add(1)
	return t.Tx.SubscriptionGeneration(ctx)
}

func (t *countingTx) ListSubscriptions(ctx context.Context) ([]store.Subscription, error) {
	t.c.inside.Add(1)
	return t.Tx.ListSubscriptions(ctx)
}

// emitOne emits one release of watch wNow through cache and returns the ids
// of the subscriptions that got a live delivery of it, and of the dead ones.
// The cache is warmed first, as Commit does.
func emitOne(t *testing.T, st store.Store, tg store.Target, cache *MatcherCache, raw string) (delivered, dead []string) {
	t.Helper()
	if r, ok := st.(SubscriptionReader); ok && cache != nil {
		must(t, cache.Warm(t.Context(), r, discardLogger()))
	}
	return emitUnwarmed(t, st, tg, cache, raw)
}

// emitUnwarmed is emitOne without the warm-up: Emit meets whatever the cache
// holds.
func emitUnwarmed(t *testing.T, st store.Store, tg store.Target, cache *MatcherCache, raw string) (delivered, dead []string) {
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
	for i := range 3 {
		emitOne(t, st, tg, cache, fmt.Sprintf("9.0.%d", i+1))
	}
	if st.outside.Load() != 1 || st.inside.Load() != 0 {
		t.Fatalf("unchanged subscriptions: %d loads outside, %d inside; want 1 and 0", st.outside.Load(), st.inside.Load())
	}

	must(t, st.CreateSubscription(ctx, store.Subscription{ID: "s-more", SinkID: "k"}))
	emitOne(t, st, tg, cache, "9.1.0")
	emitOne(t, st, tg, cache, "9.1.1")
	if st.outside.Load() != 2 || st.inside.Load() != 0 {
		t.Fatalf("after one change: %d loads outside, %d inside; want 2 and 0", st.outside.Load(), st.inside.Load())
	}

	// An edit of a column the matcher does not read (the sink) is no change.
	must(t, st.InConfigTx(ctx, func(tx store.ConfigTx) error {
		return tx.UpdateSubscription(ctx, store.Subscription{ID: "s-more", SinkID: "k", Template: "x"})
	}))
	emitOne(t, st, tg, cache, "9.1.2")
	if st.outside.Load() != 2 {
		t.Fatalf("a template edit reloaded the cache: %d loads", st.outside.Load())
	}

	// A nil cache is the old behaviour: one load per Emit, inside.
	before := st.inside.Load()
	emitOne(t, st, tg, nil, "9.2.0")
	emitOne(t, st, tg, nil, "9.2.1")
	if st.inside.Load() != before+2 {
		t.Fatalf("nil cache: %d loads, want %d", st.inside.Load(), before+2)
	}
}

// Emit meeting a cache that is not current loads for itself and leaves the
// cache alone: what a transaction sees may still be rolled back.
func TestEmitNeverCachesWhatATransactionSees(t *testing.T) {
	st, tg := fixture(t)
	ctx := t.Context()
	cache := NewMatcherCache()
	delivered, _ := emitOne(t, st, tg, cache, "9.0.0")
	want := []string{"s-all", "s-cool", "s-final"}
	if !slices.Equal(delivered, want) {
		t.Fatalf("delivered %v, want %v", delivered, want)
	}

	// Create a subscription and emit in the same transaction, then roll back.
	rel := []policy.WatchReleases{{
		Watch:    store.Watch{ID: wNow, Labels: map[string]string{"team": "platform"}},
		Releases: []policy.Release{{Raw: "9.1.0", Scheme: "semver", DetectedAt: check2}},
	}}
	errRollback := errors.New("rollback")
	err := st.InConfigTx(ctx, func(tx store.ConfigTx) error {
		must(t, tx.CreateSubscription(ctx, store.Subscription{ID: "s-ghost", SinkID: "k"}))
		n, err := Emit(ctx, discardAppends{tx}, rel, EmitInput{Target: tg, Instance: "angelia", Now: check2, Matcher: cache})
		if err != nil || n.Deliveries != 5 { // the three above, s-ghost and the dead s-broken
			t.Errorf("in-transaction emit = %+v, %v", n, err)
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatalf("InConfigTx: %v", err)
	}
	// A different change commits; it takes the generation number the
	// rolled-back insert had used.
	must(t, st.CreateSubscription(ctx, store.Subscription{ID: "s-real", SinkID: "k"}))
	for name, emit := range map[string]func(*testing.T, store.Store, store.Target, *MatcherCache, string) ([]string, []string){
		"unwarmed": emitUnwarmed, "warmed": emitOne,
	} {
		got, _ := emit(t, st, tg, cache, "9.2."+name)
		if !slices.Equal(got, []string{"s-all", "s-cool", "s-final", "s-real"}) {
			t.Errorf("%s: delivered %v after the rollback, want s-real and no s-ghost", name, got)
		}
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

	// A warm Commit reads the generation once inside its transaction.
	if st.gens.Load() != 1 {
		t.Fatalf("%d generation reads in the transaction, want 1", st.gens.Load())
	}
	// A later subscription is seen by the next Commit.
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
	st := &countingStore{Store: base}
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
	must(t, cache.Warm(ctx, base, discardLogger()))
	gen, err := base.SubscriptionGeneration(ctx)
	must(t, err)
	if cur := cache.get(); cur == nil || cur.gen != gen {
		t.Fatalf("cached %+v, want generation %d", cur, gen)
	}
	inside := st.inside.Load()
	if got, _ := emitUnwarmed(t, st, tg, cache, "8.0.0"); !slices.Contains(got, "s-conc") {
		t.Errorf("delivered %v, want s-conc among them", got)
	}
	if st.inside.Load() != inside {
		t.Errorf("a current cache still loaded %d times inside the transaction", st.inside.Load()-inside)
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
	subs := make([]store.Subscription, n)
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
		subs[i] = store.Subscription{
			ID: fmt.Sprintf("s-%05d", i), SinkID: "k", Filter: json.RawMessage(f), Policy: json.RawMessage(`{"cooldown":"1h"}`),
		}
	}
	// One transaction: 10,000 autocommitted inserts would take seconds.
	err = st.InConfigTx(ctx, func(tx store.ConfigTx) error {
		for _, sub := range subs {
			if err := tx.CreateSubscription(ctx, sub); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		tb.Fatal(err)
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

// TestWarmCommitDoesNoSubscriptionLoadInTheTransaction is the count-based
// form of the #166 target, which CI can check on any host: with 10,000
// subscriptions, a Commit whose cache is current lists no subscription inside
// its write transaction and reads the generation exactly once.
func TestWarmCommitDoesNoSubscriptionLoadInTheTransaction(t *testing.T) {
	base, tg := subscriptionsFixture(t, 10_000)
	st := &countingStore{Store: base}
	cache := NewMatcherCache()
	ctx := t.Context()
	commit := func(now time.Time, raws ...string) Emitted {
		t.Helper()
		var vs []source.Version
		for _, r := range raws {
			vs = append(vs, pypiVersion(r, now.Add(-time.Minute)))
		}
		n, err := Commit(ctx, st, CheckInput{Target: tg, Instance: "angelia", Now: now, Matcher: cache, Fetched: source.Result{Versions: vs}})
		must(t, err)
		return n
	}
	commit(check1, "1.0.0") // baseline: nothing emitted, the cache is warmed
	st.outside.Store(0)
	st.inside.Store(0)
	st.gens.Store(0)
	n := commit(check2, "1.0.0", "1.1.0")
	if n.Events != 1 || n.Deliveries == 0 {
		t.Fatalf("emitted %+v", n)
	}
	if st.inside.Load() != 0 || st.outside.Load() != 0 || st.gens.Load() != 1 {
		t.Fatalf("warm commit: %d loads inside, %d outside, %d generation reads inside; want 0, 0, 1",
			st.inside.Load(), st.outside.Load(), st.gens.Load())
	}
}

// TestEmitHoldsTheTransactionBriefly checks the #166 wall-clock target: with
// 10,000 subscriptions, a warmed cache keeps the write transaction of an Emit
// under 5 ms (median of several runs). It depends on the host, so it runs only
// with ANGELIA_TIMING_TESTS=1, and never under the race detector, which slows
// the matcher several times over. CI relies on the count-based test above.
func TestEmitHoldsTheTransactionBriefly(t *testing.T) {
	if raceEnabled {
		t.Skip("wall-clock bound is not meaningful under the race detector")
	}
	if os.Getenv("ANGELIA_TIMING_TESTS") != "1" {
		t.Skip("set ANGELIA_TIMING_TESTS=1 to run the wall-clock check")
	}
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
	if c >= 5*time.Millisecond {
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

// discardAppends lets Emit run in a config transaction, which refuses
// AppendEvent (ADR 0028): the test needs only what Emit reads there.
type discardAppends struct{ store.ConfigTx }

func (discardAppends) AppendEvent(context.Context, store.Event, []store.Delivery) error { return nil }
