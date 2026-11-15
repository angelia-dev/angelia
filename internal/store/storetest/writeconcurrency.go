package storetest

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// The cases of this file show the anomalies the write-concurrency model of
// ADR 0028 §2 prevents. SQLite serializes every write transaction; Postgres
// takes the config lock in config, API and secret transactions, and a check
// reads its watches FOR SHARE. Each case sleeps inside a transaction to open
// the window an unserialized engine would lose an update in.

// window is how long a transaction stays open in these cases.
const window = 100 * time.Millisecond

// testConcurrentConfigApplies: two applies of the same watch file at once
// both succeed and leave one target and one watch.
func testConcurrentConfigApplies(t *testing.T, open opener) {
	s := open(t)
	ctx := t.Context()
	coords := map[string]string{"name": "requests"}
	apply := func(i int) error {
		return s.InConfigTx(ctx, func(tx store.ConfigTx) error {
			tg, err := tx.FindTarget(ctx, "pypi", coords)
			if errors.Is(err, store.ErrNotFound) {
				time.Sleep(window) // both would create the target
				tg = store.Target{ID: fmt.Sprintf("t%d", i), Kind: "pypi", Coordinates: coords, NextCheckAt: t0, Interval: time.Hour}
				err = tx.CreateTarget(ctx, tg)
			}
			if err != nil {
				return err
			}
			ws, err := tx.ListManagedWatches(ctx, store.ManagedByFile)
			if err != nil {
				return err
			}
			w := store.Watch{ID: fmt.Sprintf("w%d", i), TargetID: tg.ID, Name: "requests", ManagedBy: store.ManagedByFile, Interval: time.Hour}
			if len(ws) == 1 {
				w.ID = ws[0].ID
				err = tx.UpdateWatch(ctx, w)
			} else {
				err = tx.CreateWatch(ctx, w)
			}
			if err != nil {
				return err
			}
			_, _, err = tx.RecomputeTargetInterval(ctx, tg.ID, t0)
			return err
		})
	}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() { errs[i] = apply(i) })
	}
	wg.Wait()
	must(t, errors.Join(errs...))
	var ws []store.Watch
	must(t, s.InConfigTx(ctx, func(tx store.ConfigTx) (err error) {
		ws, err = tx.ListManagedWatches(ctx, store.ManagedByFile)
		return err
	}))
	if len(ws) != 1 {
		t.Fatalf("watches after two applies: %+v, want one", ws)
	}
	n, err := s.CountDueTargets(ctx, t0)
	must(t, err)
	if n != 1 {
		t.Fatalf("%d targets after two applies, want 1", n)
	}
}

// testConcurrentRecomputeTargetInterval: two transactions each add a watch
// and recompute the interval of the same target; the target ends with the
// smaller interval, not the one of the transaction that wrote last. Without
// serialization, the 10m transaction writes first and commits late, while the
// 20m one reads before that commit (seeing neither 10m nor its watch) and
// overwrites it after.
func testConcurrentRecomputeTargetInterval(t *testing.T, open opener) {
	s := open(t)
	ctx := t.Context()
	must(t, s.CreateTarget(ctx, store.Target{
		ID: "t1", Kind: "pypi", Coordinates: map[string]string{"name": "a"}, NextCheckAt: t0.Add(time.Hour), Interval: 2 * time.Hour,
	}))
	runs := []struct {
		interval, before, after time.Duration
	}{{10 * time.Minute, 0, 2 * window}, {20 * time.Minute, window, 0}}
	errs := make([]error, len(runs))
	var wg sync.WaitGroup
	for i, r := range runs {
		wg.Go(func() {
			errs[i] = s.InConfigTx(ctx, func(tx store.ConfigTx) error {
				if err := tx.CreateWatch(ctx, store.Watch{ID: fmt.Sprintf("w%d", i), TargetID: "t1", Interval: r.interval}); err != nil {
					return err
				}
				time.Sleep(r.before)
				_, _, err := tx.RecomputeTargetInterval(ctx, "t1", t0)
				time.Sleep(r.after)
				return err
			})
		})
	}
	wg.Wait()
	must(t, errors.Join(errs...))
	tg, err := s.GetTarget(ctx, "t1")
	must(t, err)
	if tg.Interval != 10*time.Minute {
		t.Fatalf("target interval = %s, want 10m0s (the minimum of its watches)", tg.Interval)
	}
}

// testConcurrentUpdateSecretSameRevision: two writers that read revision 1
// update the secret at once; exactly one wins (ADR 0024 §5).
func testConcurrentUpdateSecretSameRevision(t *testing.T, open opener) {
	s := open(t)
	ctx := t.Context()
	must(t, s.InSecretTx(ctx, func(tx store.SecretTx) error {
		return tx.CreateSecret(ctx, testSecret("01SECRETA", "token", "aaaaaaaaaaaaaaaa"))
	}))
	won := make([]bool, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range won {
		wg.Go(func() {
			errs[i] = s.InSecretTx(ctx, func(tx store.SecretTx) error {
				sec := testSecret("01SECRETA", "token", fmt.Sprintf("%016d", i))
				sec.Revision = 1
				time.Sleep(window / 2)
				ok, err := tx.UpdateSecret(ctx, sec)
				won[i] = ok
				return err
			})
		})
	}
	wg.Wait()
	must(t, errors.Join(errs...))
	if won[0] == won[1] {
		t.Fatalf("UpdateSecret at the same revision: wins %v, want exactly one", won)
	}
	got, err := s.SecretByName(ctx, "token")
	must(t, err)
	winner := 0
	if won[1] {
		winner = 1
	}
	if got.Revision != 2 || got.KeyID != fmt.Sprintf("%016d", winner) {
		t.Fatalf("secret = %+v, want revision 2 from writer %d", got, winner)
	}
}

// testAppendEventRefusedInConfigAndAPITx: config and API transactions append
// no event (ADR 0028 agent-decided item 1), so they never take the event lock
// after the config lock.
func testAppendEventRefusedInConfigAndAPITx(t *testing.T, open opener) {
	s := open(t)
	seed(t, s)
	ctx := t.Context()
	wantErr(t, s.InConfigTx(ctx, func(tx store.ConfigTx) error {
		return tx.AppendEvent(ctx, event("e1"), nil)
	}), store.ErrInvalid, "AppendEvent in InConfigTx")
	wantErr(t, s.InAPITx(ctx, func(tx store.APITx) error {
		return tx.AppendEvent(ctx, event("e2"), []store.Delivery{pending("s1")})
	}), store.ErrInvalid, "AppendEvent in InAPITx")
	evs, err := s.ListEventsAfter(ctx, 0, 0)
	must(t, err)
	if len(evs) != 0 {
		t.Fatalf("events = %+v, want none", evs)
	}
}

// testMovedWatchLeavesNoStaleHold: a check holds a version for a watch while
// a config transaction moves the watch to another target and forgets its
// held versions (UpdateWatch, then DeletePendingReleasesOfWatch). The held
// version must not survive, or it would come back when the watch moves back
// (ADR 0014, ADR 0028 §2: the check reads its watches FOR SHARE).
func testMovedWatchLeavesNoStaleHold(t *testing.T, open opener) {
	s := open(t)
	seed(t, s)
	ctx := t.Context()
	must(t, s.CreateTarget(ctx, store.Target{ID: "t2", Kind: "npm", Coordinates: map[string]string{"name": "b"}, NextCheckAt: t0}))
	_, err := s.AddVersion(ctx, store.Version{TargetID: "t1", Raw: "1.0", Normalized: "1.0", FirstSeenAt: t0})
	must(t, err)
	w, err := s.GetWatch(ctx, "w1")
	must(t, err)

	read := make(chan struct{})
	checkErr := make(chan error, 1)
	go func() {
		checkErr <- s.InTx(ctx, func(tx store.Tx) error {
			if _, err := tx.ListWatches(ctx, "t1"); err != nil {
				close(read)
				return err
			}
			close(read)
			time.Sleep(window) // the config transaction starts meanwhile
			return tx.AddPendingRelease(ctx, store.PendingRelease{
				WatchID: "w1", TargetID: "t1", Raw: "1.0", HeldAt: t0, Release: []byte(`{}`),
			})
		})
	}()
	<-read
	move := func(target string) error {
		return s.InConfigTx(ctx, func(tx store.ConfigTx) error {
			w.TargetID = target
			if err := tx.UpdateWatch(ctx, w); err != nil {
				return err
			}
			_, err := tx.DeletePendingReleasesOfWatch(ctx, w.ID)
			return err
		})
	}
	must(t, move("t2"))
	must(t, <-checkErr)
	must(t, s.InConfigTx(ctx, func(tx store.ConfigTx) error { w.TargetID = "t1"; return tx.UpdateWatch(ctx, w) }))
	held, err := s.ListPendingReleases(ctx, "t1")
	must(t, err)
	if len(held) != 0 {
		t.Fatalf("held versions after moving the watch away and back: %+v, want none", held)
	}
}
