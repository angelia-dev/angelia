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

	evs, err := s.ListEventsAfter(ctx, "", 0)
	must(t, err)
	if len(evs) != 1 || evs[0].ID != "e1" || string(evs[0].Payload) != `{"v":1}` || !evs[0].CreatedAt.Equal(t0) {
		t.Fatalf("events = %+v (outbox not atomic?)", evs)
	}
	if evs, _ := s.ListEventsAfter(ctx, "e1", 10); len(evs) != 0 {
		t.Fatalf("events after cursor = %+v", evs)
	}
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
	evs, _ := s.ListEventsAfter(ctx, "", 10)
	got, _ := s.GetTarget(ctx, "t1")
	if len(vs) != 0 || len(evs) != 0 || string(got.FetchState) != `{"etag":"x"}` {
		t.Fatalf("rollback incomplete: versions=%v events=%v target=%+v", vs, evs, got)
	}

	must(t, s.InTx(ctx, func(tx store.Tx) error { return check(tx, "e2", false) }))
	vs, _ = s.ListVersions(ctx, "t1")
	evs, _ = s.ListEventsAfter(ctx, "", 10)
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
						_, err = s.ListEventsAfter(ctx, "", 5)
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
			evs, err := s.ListEventsAfter(ctx, "", workers*perWorker+1)
			must(t, err)
			if len(evs) != workers*perWorker {
				t.Fatalf("events = %d, want %d", len(evs), workers*perWorker)
			}
		})
	}
}
