package storetest

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// testNoSkippedEventUnderConcurrentWriters is the store-level form of the
// API's TestEventPaginationConcurrentWriters (ADR 0008): writers append
// events in transactions of their own and sleep inside them, so a later seq
// could commit before an earlier one. A reader pages with the highest Seq it
// has seen as the cursor; every committed event must come out exactly once.
func testNoSkippedEventUnderConcurrentWriters(t *testing.T, open opener) {
	s := open(t)
	seed(t, s)
	// A failed read must not leave writers running past the test.
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	const writers, perWriter = 6, 8

	var mu sync.Mutex
	committed := map[string]bool{}
	for w := range writers {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(uint64(w), 1)) //nolint:gosec // test jitter, not a secret
			for i := range perWriter {
				id := fmt.Sprintf("e-%d-%02d", w, i)
				err := s.InTx(ctx, func(tx store.Tx) error {
					if err := tx.AppendEvent(ctx, event(id), nil); err != nil {
						return err
					}
					// Hold the transaction open: a later seq may commit first.
					time.Sleep(time.Duration(r.IntN(3000)) * time.Microsecond)
					return nil
				})
				if err != nil {
					if ctx.Err() == nil {
						t.Errorf("writer %d: %v", w, err)
					}
					return
				}
				mu.Lock()
				committed[id] = true
				mu.Unlock()
			}
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	seen := map[string]int{}
	var cursor int64
	finished := false
	for {
		select {
		case <-done:
			finished = true
		default:
		}
		page, err := s.ListEventsAfter(ctx, cursor, 5)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page {
			if e.Seq <= cursor {
				t.Fatalf("event %s has seq %d, not after the cursor %d", e.ID, e.Seq, cursor)
			}
			seen[e.ID]++
			cursor = e.Seq
		}
		// Stop after a drain that began once every writer had committed.
		if finished && len(page) == 0 {
			break
		}
	}
	if len(seen) != len(committed) || len(committed) != writers*perWriter {
		t.Fatalf("read %d distinct events, %d committed, want %d", len(seen), len(committed), writers*perWriter)
	}
	for id, n := range seen {
		if !committed[id] || n != 1 {
			t.Errorf("event %s read %d times (committed: %v)", id, n, committed[id])
		}
	}
}

// testLeasesExcludeAcrossStoreInstances: two store values on one database
// stand in for two processes. Their concurrent lease calls never return the
// same id to both.
func testLeasesExcludeAcrossStoreInstances(t *testing.T, e Engine) {
	first := e.Open(t)
	second := e.OpenSecond(t, first)
	seed(t, first)
	ctx := t.Context()
	const n, callers = 40, 3
	for i := range n {
		must(t, first.CreateTarget(ctx, store.Target{
			ID: fmt.Sprintf("t%02d", i), Kind: "pypi", Coordinates: map[string]string{"name": fmt.Sprint(i)}, NextCheckAt: t0,
		}))
		must(t, first.AppendEvent(ctx, event(fmt.Sprintf("e%02d", i)), []store.Delivery{{
			ID: fmt.Sprintf("d%02d", i), SubscriptionID: "s1", Status: store.DeliveryPending,
		}}))
	}

	var mu sync.Mutex
	targets, deliveries := map[string]string{}, map[string]string{}
	claim := func(into map[string]string, id, holder string) {
		mu.Lock()
		defer mu.Unlock()
		if prev, ok := into[id]; ok {
			t.Errorf("%s leased by %s and by %s", id, prev, holder)
		}
		into[id] = holder
	}
	var wg sync.WaitGroup
	for i, s := range []store.Store{first, second} {
		for c := range callers {
			holder := fmt.Sprintf("store%d-caller%d", i, c)
			wg.Go(func() {
				for {
					ts, err := s.LeaseDueTargets(ctx, holder, t0, t0.Add(time.Minute), 3)
					if err != nil {
						t.Error(err)
						return
					}
					ds, err := s.LeaseDueDeliveries(ctx, holder, t0, t0.Add(time.Minute), 3)
					if err != nil {
						t.Error(err)
						return
					}
					if len(ts)+len(ds) == 0 {
						return
					}
					for _, x := range ts {
						claim(targets, x.ID, holder)
					}
					for _, x := range ds {
						claim(deliveries, x.Delivery.ID, holder)
					}
				}
			})
		}
	}
	wg.Wait()
	// seed's target t1 is due too.
	for kind, got := range map[string]map[string]string{"targets": targets, "deliveries": deliveries} {
		want := n
		if kind == "targets" {
			want++
		}
		if len(got) != want {
			ids := make([]string, 0, len(got))
			for id := range got {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			t.Errorf("leased %d %s, want %d: %v", len(got), kind, want, ids)
		}
	}
}
