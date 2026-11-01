package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

var _ store.Store = (*Store)(nil)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "angelia.db")
	s, err := Open(t.Context(), path, discard())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

// seed creates target t1, watch w1, sink k1 and subscriptions s1, s2.
func seed(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	must(t, s.CreateTarget(ctx, store.Target{
		ID: "t1", Kind: "pypi", Coordinates: map[string]string{"name": "requests"},
		FetchState: json.RawMessage(`{"etag":"x"}`), NextCheckAt: t0,
	}))
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w1", TargetID: "t1", Labels: map[string]string{"team": "a"}}))
	must(t, s.CreateSink(ctx, store.Sink{ID: "k1", Kind: "webhook", ConfigEncrypted: []byte{1, 2}}))
	must(t, s.CreateSubscription(ctx, store.Subscription{ID: "s1", SinkID: "k1"}))
	must(t, s.CreateSubscription(ctx, store.Subscription{ID: "s2", SinkID: "k1", Filter: json.RawMessage(`{"bump":"major"}`)}))
}

func event(id string) store.Event {
	return store.Event{ID: id, WatchID: "w1", Type: "release.published", Payload: json.RawMessage(`{"v":1}`), CreatedAt: t0}
}

func pending(subID string) store.Delivery {
	return store.Delivery{ID: "d-" + subID, SubscriptionID: subID, Status: store.DeliveryPending}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t *testing.T, err, target error, what string) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Errorf("%s: got %v, want %v", what, err, target)
	}
}

func TestOpenCreatesSchemaWithSettings(t *testing.T) {
	s, _ := openTest(t)
	ctx := t.Context()
	for _, table := range []string{"target", "watch", "version", "event", "subscription", "sink", "delivery", "lease", "schema_migrations"} {
		var n int
		must(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type='table' AND name=?`, table).Scan(&n))
		if n != 1 {
			t.Errorf("table %s missing", table)
		}
	}
	if got := s.db.Stats().MaxOpenConnections; got != maxConns {
		t.Errorf("pool size = %d, want %d", got, maxConns)
	}
	// Hold every pooled connection at once: each must carry the settings.
	for i := range maxConns {
		conn, err := s.db.Conn(ctx)
		must(t, err)
		defer func() { _ = conn.Close() }()
		for pragma, want := range map[string]string{"journal_mode": "wal", "foreign_keys": "1", "busy_timeout": "5000", "synchronous": "1"} {
			var got string
			must(t, conn.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got))
			if got != want {
				t.Errorf("conn %d: PRAGMA %s = %q, want %q", i, pragma, got, want)
			}
		}
	}
}

func TestDriverIsModernc(t *testing.T) {
	s, _ := openTest(t)
	if got := reflect.TypeOf(s.db.Driver()).Elem().PkgPath(); got != "modernc.org/sqlite" {
		t.Fatalf("driver package = %s", got)
	}
}

func TestOpenNilLogger(t *testing.T) {
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "a.db"), nil)
	must(t, err)
	must(t, s.Close())
}

func TestRestartIsNoop(t *testing.T) {
	s, path := openTest(t)
	seed(t, s)
	must(t, s.Close())

	db, err := openDB(path)
	must(t, err)
	defer func() { _ = db.Close() }()
	applied, err := migrate(t.Context(), db, discard(), 0)
	must(t, err)
	if applied != 0 {
		t.Fatalf("restart applied %d migrations, want 0", applied)
	}

	s2, err := Open(t.Context(), path, discard())
	must(t, err)
	defer func() { _ = s2.Close() }()
	if _, err := s2.GetWatch(t.Context(), "w1"); err != nil {
		t.Fatalf("data lost on restart: %v", err)
	}
}

func TestMigrationsDownAndUp(t *testing.T) {
	ctx := t.Context()
	db, err := openDB(filepath.Join(t.TempDir(), "angelia.db"))
	must(t, err)
	defer func() { _ = db.Close() }()
	n, err := migrate(ctx, db, discard(), 0)
	must(t, err)
	p, err := newProvider(db)
	must(t, err)
	down, err := p.DownTo(ctx, 0)
	must(t, err)
	if len(down) != n {
		t.Fatalf("down ran %d migrations, want %d", len(down), n)
	}
	var tables int
	must(t, db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type='table' AND name <> ? AND name NOT LIKE 'sqlite_%'`, migrationsTable).Scan(&tables))
	if tables != 0 {
		t.Fatalf("%d tables left after down", tables)
	}
	if again, err := migrate(ctx, db, discard(), 0); err != nil || again != n {
		t.Fatalf("up after down: %d %v", again, err)
	}
}

func TestUpgradeMigrationOnPopulatedDB(t *testing.T) {
	ctx := t.Context()
	db, err := openDB(filepath.Join(t.TempDir(), "angelia.db"))
	must(t, err)
	defer func() { _ = db.Close() }()

	p, err := newProvider(db)
	must(t, err)
	sources := p.ListSources()
	if len(sources) < 2 {
		t.Fatalf("need at least two migrations, have %d", len(sources))
	}
	if _, err := migrate(ctx, db, discard(), sources[len(sources)-2].Version); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO target (id, kind, coordinates_json, next_check_at) VALUES ('t1','npm','{"name":"left-pad"}',1)`,
		`INSERT INTO watch (id, target_id) VALUES ('w1','t1')`,
		`INSERT INTO version (target_id, raw, normalized, first_seen_at) VALUES ('t1','1.0.0','1.0.0',2)`,
		`INSERT INTO event (id, watch_id, type, payload_json, created_at) VALUES ('e1','w1','x','{}',3)`,
		`INSERT INTO sink (id, kind, config_json_encrypted) VALUES ('k1','webhook',x'00')`,
		`INSERT INTO subscription (id, sink_id) VALUES ('s1','k1')`,
		`INSERT INTO delivery (id, event_id, subscription_id, status) VALUES ('d1','e1','s1','pending')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	applied, err := migrate(ctx, db, discard(), 0)
	must(t, err)
	if applied != 1 {
		t.Fatalf("applied %d migrations, want 1", applied)
	}
	s := &Store{db: db, q: New(db)}
	if vs, err := s.ListVersions(ctx, "t1"); err != nil || len(vs) != 1 {
		t.Fatalf("versions after upgrade: %v %v", vs, err)
	}
	if ds, err := s.ListDeliveries(ctx, "e1"); err != nil || len(ds) != 1 {
		t.Fatalf("deliveries after upgrade: %v %v", ds, err)
	}
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	must(t, err)
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		t.Fatal("foreign key violations after upgrade")
	}
	must(t, rows.Err())
}

func TestRoundTripAndValidation(t *testing.T) {
	s, _ := openTest(t)
	seed(t, s)
	ctx := t.Context()

	tg, err := s.GetTarget(ctx, "t1")
	must(t, err)
	if tg.Coordinates["name"] != "requests" || string(tg.FetchState) != `{"etag":"x"}` ||
		!tg.NextCheckAt.Equal(t0) || tg.NextCheckAt.Location() != time.UTC || !tg.LastCheckedAt.IsZero() {
		t.Errorf("target = %+v", tg)
	}
	w, err := s.GetWatch(ctx, "w1")
	must(t, err)
	if w.TargetID != "t1" || w.Labels["team"] != "a" || string(w.Policy) != "{}" || w.BaselineDone {
		t.Errorf("watch = %+v", w)
	}
	_, err = s.GetTarget(ctx, "nope")
	wantErr(t, err, store.ErrNotFound, "GetTarget missing")
	_, err = s.GetWatch(ctx, "nope")
	wantErr(t, err, store.ErrNotFound, "GetWatch missing")

	dup := store.Target{ID: "t1", Kind: "npm", NextCheckAt: t0}
	wantErr(t, s.CreateTarget(ctx, dup), store.ErrConflict, "duplicate target id")
	same := store.Target{ID: "t9", Kind: "pypi", Coordinates: map[string]string{"name": "requests"}, NextCheckAt: t0}
	wantErr(t, s.CreateTarget(ctx, same), store.ErrConflict, "duplicate (kind, coordinates)")
	wantErr(t, s.CreateTarget(ctx, store.Target{ID: "t8", Kind: "npm"}), store.ErrInvalid, "zero next_check_at")
	wantErr(t, s.CreateWatch(ctx, store.Watch{ID: "w2", TargetID: "missing"}), store.ErrInvalidRef, "unknown target")
	wantErr(t, s.CreateWatch(ctx, store.Watch{ID: "w3", TargetID: "t1", Policy: json.RawMessage(`{bad`)}), store.ErrInvalid, "invalid JSON")
	wantErr(t, s.CreateSubscription(ctx, store.Subscription{ID: "s9", SinkID: "k1", Filter: json.RawMessage(`[`)}), store.ErrInvalid, "invalid filter")
	_, err = s.AddVersion(ctx, store.Version{TargetID: "t1", Raw: "1", Normalized: "1"})
	wantErr(t, err, store.ErrInvalid, "zero first_seen_at")

	tg.FetchState, tg.LastCheckedAt, tg.NextCheckAt, tg.ErrorCount = json.RawMessage(`{"etag":"y"}`), t0, t0.Add(time.Hour), 2
	must(t, s.UpdateTarget(ctx, tg))
	got, err := s.GetTarget(ctx, "t1")
	must(t, err)
	if string(got.FetchState) != `{"etag":"y"}` || !got.LastCheckedAt.Equal(t0) || !got.NextCheckAt.Equal(t0.Add(time.Hour)) || got.ErrorCount != 2 {
		t.Errorf("updated target = %+v", got)
	}
	wantErr(t, s.UpdateTarget(ctx, store.Target{ID: "nope", NextCheckAt: t0}), store.ErrNotFound, "update missing target")
}

func TestVersionUniqueness(t *testing.T) {
	s, _ := openTest(t)
	seed(t, s)
	ctx := t.Context()
	v := store.Version{TargetID: "t1", Raw: "2.0.0", Normalized: "2.0.0", PublishedAt: t0, Prerelease: true, FirstSeenAt: t0}
	for i, want := range []bool{true, false} {
		got, err := s.AddVersion(ctx, v)
		must(t, err)
		if got != want {
			t.Fatalf("AddVersion #%d = %v, want %v", i, got, want)
		}
	}
	vs, err := s.ListVersions(ctx, "t1")
	must(t, err)
	if len(vs) != 1 || !vs[0].Prerelease || vs[0].Yanked || !vs[0].PublishedAt.Equal(t0) {
		t.Fatalf("versions = %+v", vs)
	}
}

func TestAppendEventAndDeliveryUniqueness(t *testing.T) {
	s, _ := openTest(t)
	seed(t, s)
	ctx := t.Context()
	d2 := pending("s2")
	d2.LastError, d2.NextAttemptAt = "boom", t0
	must(t, s.AppendEvent(ctx, event("e1"), []store.Delivery{pending("s1"), d2}))
	ds, err := s.ListDeliveries(ctx, "e1")
	must(t, err)
	if len(ds) != 2 || ds[0].EventID != "e1" || !ds[0].NextAttemptAt.IsZero() || ds[1].LastError != "boom" || !ds[1].NextAttemptAt.Equal(t0) {
		t.Fatalf("deliveries = %+v", ds)
	}

	// A second delivery for (e2, s1) violates the unique key: nothing is written.
	d3, d4 := pending("s1"), pending("s1")
	d3.ID, d4.ID = "d3", "d4"
	wantErr(t, s.AppendEvent(ctx, event("e2"), []store.Delivery{d3, d4}), store.ErrConflict, "duplicate delivery")

	d5 := pending("s1")
	d5.ID, d5.EventID = "d5", "other"
	wantErr(t, s.AppendEvent(ctx, event("e3"), []store.Delivery{d5}), store.ErrInvalid, "delivery for another event")
	d6 := pending("s1")
	d6.ID, d6.Status = "d6", "bogus"
	if err := s.AppendEvent(ctx, event("e4"), []store.Delivery{d6}); err == nil {
		t.Error("unknown delivery status accepted")
	}
	for _, p := range []string{"", "null", " null "} {
		e := event("e5")
		e.Payload = json.RawMessage(p)
		wantErr(t, s.AppendEvent(ctx, e, nil), store.ErrInvalid, fmt.Sprintf("payload %q", p))
	}
	e := event("e6")
	e.CreatedAt = time.Time{}
	wantErr(t, s.AppendEvent(ctx, e, nil), store.ErrInvalid, "zero created_at")

	evs, err := s.ListEventsAfter(ctx, 0, 0)
	must(t, err)
	if len(evs) != 1 || evs[0].ID != "e1" || evs[0].Seq <= 0 || string(evs[0].Payload) != `{"v":1}` || !evs[0].CreatedAt.Equal(t0) {
		t.Fatalf("events = %+v (outbox not atomic?)", evs)
	}
	if evs, _ := s.ListEventsAfter(ctx, evs[0].Seq, 10); len(evs) != 0 {
		t.Fatalf("events after cursor = %+v", evs)
	}
}

// TestEventCursorFollowsCommitOrder: a writer can mint a smaller ULID and
// commit after a reader has paged past a larger one. The Seq cursor follows
// commit order, so the reader still sees the late event; an ID cursor would
// skip it (ADR 0008).
func TestEventCursorFollowsCommitOrder(t *testing.T) {
	s, _ := openTest(t)
	seed(t, s)
	ctx := t.Context()
	const early, late = "01JBZZZZZZZZZZZZZZZZZZZZZZ", "01JBAAAAAAAAAAAAAAAAAAAAAA" // late < early as IDs
	must(t, s.AppendEvent(ctx, event(early), nil))

	page1, err := s.ListEventsAfter(ctx, 0, 10)
	must(t, err)
	if len(page1) != 1 || page1[0].ID != early {
		t.Fatalf("page 1 = %+v", page1)
	}
	cursor := page1[0].Seq

	must(t, s.AppendEvent(ctx, event(late), nil))
	page2, err := s.ListEventsAfter(ctx, cursor, 10)
	must(t, err)
	if len(page2) != 1 || page2[0].ID != late || page2[0].Seq <= cursor {
		t.Fatalf("page 2 after cursor %d = %+v, want the later commit %s", cursor, page2, late)
	}

	all, err := s.ListEventsAfter(ctx, 0, 1)
	must(t, err)
	if len(all) != 1 || all[0].ID != early {
		t.Fatalf("limit 1 = %+v, want the first commit", all)
	}
}

// TestDeleteConfigKeepsHistory: watches, subscriptions and sinks can be
// deleted while events and deliveries that name them are retained.
func TestDeleteConfigKeepsHistory(t *testing.T) {
	s, _ := openTest(t)
	seed(t, s)
	ctx := t.Context()
	must(t, s.AppendEvent(ctx, event("e1"), []store.Delivery{pending("s1"), pending("s2")}))

	must(t, s.DeleteWatch(ctx, "w1"))
	_, err := s.GetWatch(ctx, "w1")
	wantErr(t, err, store.ErrNotFound, "deleted watch")
	evs, err := s.ListEventsAfter(ctx, 0, 10)
	must(t, err)
	if len(evs) != 1 || evs[0].WatchID != "w1" {
		t.Fatalf("events after watch delete = %+v", evs)
	}

	must(t, s.DeleteSubscription(ctx, "s1"))
	wantErr(t, s.DeleteSink(ctx, "k1"), store.ErrInvalidRef, "sink still used by s2")
	must(t, s.DeleteSubscription(ctx, "s2"))
	must(t, s.DeleteSink(ctx, "k1"))
	ds, err := s.ListDeliveries(ctx, "e1")
	must(t, err)
	if len(ds) != 2 || ds[0].SubscriptionID != "s1" || ds[1].SubscriptionID != "s2" {
		t.Fatalf("deliveries after subscription and sink delete = %+v", ds)
	}

	wantErr(t, s.DeleteWatch(ctx, "w1"), store.ErrNotFound, "delete missing watch")
	wantErr(t, s.DeleteSubscription(ctx, "s1"), store.ErrNotFound, "delete missing subscription")
	wantErr(t, s.DeleteSink(ctx, "k1"), store.ErrNotFound, "delete missing sink")

	// Config foreign keys still hold, and delivery still requires its event.
	wantErr(t, s.CreateSubscription(ctx, store.Subscription{ID: "s3", SinkID: "k1"}), store.ErrInvalidRef, "subscription to deleted sink")
	if _, err := s.db.ExecContext(ctx, `INSERT INTO delivery (id, event_id, subscription_id, status) VALUES ('d9','nope','s1','pending')`); err == nil {
		t.Error("delivery for a missing event accepted")
	}
	rows, err := s.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	must(t, err)
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		t.Fatal("foreign key violations after deletes")
	}
	must(t, rows.Err())
}

func TestInTx(t *testing.T) {
	s, _ := openTest(t)
	seed(t, s)
	ctx := t.Context()
	tg, err := s.GetTarget(ctx, "t1")
	must(t, err)
	tg.FetchState, tg.LastCheckedAt = json.RawMessage(`{"etag":"new"}`), t0
	check := func(tx store.Tx, eventID string, fail bool) error {
		if _, err := tx.AddVersion(ctx, store.Version{TargetID: "t1", Raw: eventID, Normalized: eventID, FirstSeenAt: t0}); err != nil {
			return err
		}
		if err := tx.AppendEvent(ctx, event(eventID), []store.Delivery{pending("s1")}); err != nil {
			return err
		}
		if err := tx.UpdateTarget(ctx, tg); err != nil {
			return err
		}
		if fail {
			return errors.New("boom")
		}
		return nil
	}

	// Failure after every write: none of them survives.
	if err := s.InTx(ctx, func(tx store.Tx) error { return check(tx, "e1", true) }); err == nil || err.Error() != "boom" {
		t.Fatalf("InTx error = %v", err)
	}
	vs, _ := s.ListVersions(ctx, "t1")
	evs, _ := s.ListEventsAfter(ctx, 0, 10)
	got, _ := s.GetTarget(ctx, "t1")
	if len(vs) != 0 || len(evs) != 0 || string(got.FetchState) != `{"etag":"x"}` {
		t.Fatalf("rollback incomplete: versions=%v events=%v target=%+v", vs, evs, got)
	}

	must(t, s.InTx(ctx, func(tx store.Tx) error { return check(tx, "e2", false) }))
	vs, _ = s.ListVersions(ctx, "t1")
	evs, _ = s.ListEventsAfter(ctx, 0, 10)
	got, _ = s.GetTarget(ctx, "t1")
	if len(vs) != 1 || len(evs) != 1 || string(got.FetchState) != `{"etag":"new"}` {
		t.Fatalf("commit incomplete: versions=%v events=%v target=%+v", vs, evs, got)
	}
}

func TestLease(t *testing.T) {
	s, _ := openTest(t)
	ctx := t.Context()
	until := t0.Add(time.Minute)
	steps := []struct {
		holder string
		now    time.Time
		want   bool
	}{
		{"a", t0, true},                     // free
		{"b", t0.Add(time.Second), false},   // held by a
		{"a", t0.Add(time.Second), true},    // renewal
		{"b", until.Add(time.Second), true}, // expired, taken over
	}
	for i, st := range steps {
		got, err := s.AcquireLease(ctx, "scheduler", st.holder, st.now, st.now.Add(time.Minute))
		must(t, err)
		if got != st.want {
			t.Fatalf("step %d: AcquireLease(%s) = %v, want %v", i, st.holder, got, st.want)
		}
	}
	must(t, s.ReleaseLease(ctx, "scheduler", "b"))
	if ok, err := s.AcquireLease(ctx, "scheduler", "a", t0, until); err != nil || !ok {
		t.Fatalf("after release: %v %v", ok, err)
	}
}

// TestConcurrentWriters runs writers from many goroutines; with a bounded
// pool, WAL, busy_timeout and immediate transactions none may fail with
// SQLITE_BUSY.
func TestConcurrentWriters(t *testing.T) {
	for _, workers := range []int{16, 64} {
		t.Run(fmt.Sprint(workers), func(t *testing.T) {
			s, _ := openTest(t)
			seed(t, s)
			ctx := context.Background()
			const perWorker = 20
			var wg sync.WaitGroup
			errs := make(chan error, workers*perWorker*3)
			for w := range workers {
				wg.Go(func() {
					for i := range perWorker {
						id := fmt.Sprintf("e%02d%03d", w, i)
						d := pending("s1")
						d.ID = "d" + id
						errs <- s.AppendEvent(ctx, event(id), []store.Delivery{d})
						_, err := s.AddVersion(ctx, store.Version{TargetID: "t1", Raw: id, Normalized: id, FirstSeenAt: t0})
						errs <- err
						_, err = s.ListEventsAfter(ctx, 0, 5)
						errs <- err
					}
				})
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			evs, err := s.ListEventsAfter(ctx, 0, workers*perWorker+1)
			must(t, err)
			if len(evs) != workers*perWorker {
				t.Fatalf("events = %d, want %d", len(evs), workers*perWorker)
			}
			for i := 1; i < len(evs); i++ {
				if evs[i].Seq <= evs[i-1].Seq {
					t.Fatalf("seq not increasing: %d then %d", evs[i-1].Seq, evs[i].Seq)
				}
			}
		})
	}
}

func TestLeaseDueTargets(t *testing.T) {
	s, _ := openTest(t)
	ctx := t.Context()
	for i, due := range []time.Duration{-2 * time.Minute, -time.Minute, 0, time.Minute} {
		must(t, s.CreateTarget(ctx, store.Target{
			ID: fmt.Sprintf("t%d", i), Kind: "pypi", Coordinates: map[string]string{"name": fmt.Sprint(i)},
			NextCheckAt: t0.Add(due),
		}))
	}
	ids := func(ts []store.Target) []string {
		var out []string
		for _, x := range ts {
			out = append(out, x.ID)
		}
		return out
	}
	count := func(now time.Time) int64 {
		t.Helper()
		n, err := s.CountDueTargets(ctx, now)
		must(t, err)
		return n
	}
	if n := count(t0); n != 3 {
		t.Fatalf("CountDueTargets = %d, want 3", n)
	}
	until := t0.Add(time.Minute)

	got, err := s.LeaseDueTargets(ctx, "a", t0, until, 2)
	must(t, err)
	if !reflect.DeepEqual(ids(got), []string{"t0", "t1"}) { // oldest due first, limit applies
		t.Fatalf("a leased %v", ids(got))
	}
	if got[0].Coordinates["name"] != "0" || !got[0].NextCheckAt.Equal(t0.Add(-2*time.Minute)) {
		t.Errorf("target not decoded: %+v", got[0])
	}
	got, err = s.LeaseDueTargets(ctx, "b", t0, until, 10)
	must(t, err)
	if !reflect.DeepEqual(ids(got), []string{"t2"}) { // t0, t1 are leased; t3 is not due
		t.Fatalf("b leased %v", ids(got))
	}
	if n := count(t0); n != 0 {
		t.Fatalf("CountDueTargets with every due target leased = %d, want 0", n)
	}
	if ok, err := s.AcquireLease(ctx, store.TargetLeaseName("t0"), "b", t0, until); err != nil || ok {
		t.Fatalf("b took a's lease: %v %v", ok, err)
	}

	// a releases t0; the leases on t1 and t2 expire at until.
	must(t, s.ReleaseLease(ctx, store.TargetLeaseName("t0"), "a"))
	got, err = s.LeaseDueTargets(ctx, "c", until, until.Add(time.Minute), 10)
	must(t, err)
	if !reflect.DeepEqual(ids(got), []string{"t0", "t1", "t2", "t3"}) {
		t.Fatalf("c leased %v", ids(got))
	}

	if got, err := s.LeaseDueTargets(ctx, "c", t0, until, 0); err != nil || got != nil {
		t.Errorf("limit 0: %v %v", got, err)
	}
	_, err = s.LeaseDueTargets(ctx, "c", t0, t0, 1)
	wantErr(t, err, store.ErrInvalid, "until == now")
	_, err = s.LeaseDueTargets(ctx, "", t0, until, 1)
	wantErr(t, err, store.ErrInvalid, "empty holder")
}

// TestLeaseDueTargetsConcurrent: callers racing for the same due targets
// never lease one twice.
func TestLeaseDueTargetsConcurrent(t *testing.T) {
	s, _ := openTest(t)
	ctx := t.Context()
	const targets, callers = 50, 8
	for i := range targets {
		must(t, s.CreateTarget(ctx, store.Target{
			ID: fmt.Sprintf("t%02d", i), Kind: "pypi", Coordinates: map[string]string{"name": fmt.Sprint(i)}, NextCheckAt: t0,
		}))
	}
	var mu sync.Mutex
	seen := map[string]string{}
	var wg sync.WaitGroup
	for c := range callers {
		wg.Go(func() {
			holder := fmt.Sprint("h", c)
			for {
				got, err := s.LeaseDueTargets(ctx, holder, t0, t0.Add(time.Minute), 3)
				if err != nil {
					t.Error(err)
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, x := range got {
					if prev, ok := seen[x.ID]; ok {
						t.Errorf("%s leased by %s and %s", x.ID, prev, holder)
					}
					seen[x.ID] = holder
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(seen) != targets {
		t.Fatalf("leased %d targets, want %d", len(seen), targets)
	}
}

func TestHasDueTargets(t *testing.T) {
	s, _ := openTest(t)
	ctx := t.Context()
	has := func(now time.Time) bool {
		t.Helper()
		ok, err := s.HasDueTargets(ctx, now)
		must(t, err)
		return ok
	}
	if has(t0) {
		t.Fatal("empty store has due targets")
	}
	must(t, s.CreateTarget(ctx, store.Target{ID: "t1", Kind: "pypi", Coordinates: map[string]string{"name": "a"}, NextCheckAt: t0}))
	if has(t0.Add(-time.Second)) || !has(t0) {
		t.Fatal("due boundary wrong")
	}
	_, err := s.LeaseDueTargets(ctx, "a", t0, t0.Add(time.Minute), 1)
	must(t, err)
	if has(t0) {
		t.Fatal("leased target counted as due")
	}
	if !has(t0.Add(time.Minute)) {
		t.Fatal("target with an expired lease not due")
	}
}

func TestUpdateTargetLeased(t *testing.T) {
	s, _ := openTest(t)
	ctx := t.Context()
	must(t, s.CreateTarget(ctx, store.Target{ID: "t1", Kind: "pypi", Coordinates: map[string]string{"name": "a"}, NextCheckAt: t0}))
	lease := store.TargetLeaseName("t1")
	until := t0.Add(time.Minute)
	_, err := s.LeaseDueTargets(ctx, "a", t0, until, 1)
	must(t, err)
	upd := func(next time.Time, holder string, now time.Time) (bool, error) {
		return s.UpdateTargetLeased(ctx, store.Target{ID: "t1", NextCheckAt: next, ErrorCount: 2}, lease, holder, now)
	}
	next := func() time.Time {
		got, err := s.GetTarget(ctx, "t1")
		must(t, err)
		return got.NextCheckAt
	}

	if ok, err := upd(t0.Add(time.Hour), "b", t0); err != nil || ok {
		t.Fatalf("other holder wrote: %v %v", ok, err)
	}
	if ok, err := upd(t0.Add(time.Hour), "a", until); err != nil || ok {
		t.Fatalf("expired lease wrote: %v %v", ok, err)
	}
	if !next().Equal(t0) {
		t.Fatal("fenced write changed the target")
	}
	if ok, err := upd(t0.Add(time.Hour), "a", t0); err != nil || !ok {
		t.Fatalf("holder could not write: %v %v", ok, err)
	}
	if !next().Equal(t0.Add(time.Hour)) {
		t.Fatal("write not applied")
	}
	// A takeover after expiry fences out the old holder.
	ok, err := s.AcquireLease(ctx, lease, "b", until, until.Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("takeover: %v %v", ok, err)
	}
	if ok, err := upd(t0.Add(2*time.Hour), "a", t0); err != nil || ok {
		t.Fatalf("old holder wrote after takeover: %v %v", ok, err)
	}
	_, err = s.UpdateTargetLeased(ctx, store.Target{ID: "missing", NextCheckAt: t0}, lease, "b", until)
	wantErr(t, err, store.ErrNotFound, "missing target")
	_, err = s.UpdateTargetLeased(ctx, store.Target{ID: "t1"}, lease, "b", until)
	wantErr(t, err, store.ErrInvalid, "zero next_check_at")
}

func TestDeleteExpiredTargetLeases(t *testing.T) {
	s, _ := openTest(t)
	ctx := t.Context()
	take := func(name string, until time.Time) {
		t.Helper()
		ok, err := s.AcquireLease(ctx, name, "h", t0, until)
		if err != nil || !ok {
			t.Fatalf("acquire %s: %v %v", name, ok, err)
		}
	}
	take(store.TargetLeaseName("old"), t0.Add(time.Minute))
	take(store.TargetLeaseName("live"), t0.Add(time.Hour))
	take("compactor", t0.Add(time.Minute)) // not a target lease
	n, err := s.DeleteExpiredTargetLeases(ctx, t0.Add(time.Minute))
	must(t, err)
	if n != 1 {
		t.Fatalf("deleted %d leases, want 1", n)
	}
	// The live target lease and the other lease are still held.
	for _, name := range []string{store.TargetLeaseName("live"), "compactor"} {
		if ok, err := s.AcquireLease(ctx, name, "other", t0, t0.Add(time.Second)); err != nil || ok {
			t.Errorf("%s was deleted: %v %v", name, ok, err)
		}
	}
}
