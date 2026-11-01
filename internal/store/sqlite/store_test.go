package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/migrations"
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

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
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
	for pragma, want := range map[string]string{"journal_mode": "wal", "foreign_keys": "1", "busy_timeout": "5000"} {
		var got string
		must(t, s.db.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got))
		if got != want {
			t.Errorf("PRAGMA %s = %q, want %q", pragma, got, want)
		}
	}
}

func TestDriverIsModernc(t *testing.T) {
	s, _ := openTest(t)
	if got := reflect.TypeOf(s.db.Driver()).Elem().PkgPath(); got != "modernc.org/sqlite" {
		t.Fatalf("driver package = %s", got)
	}
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

func TestUpgradeMigrationOnPopulatedDB(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "angelia.db")
	db, err := openDB(path)
	must(t, err)
	defer func() { _ = db.Close() }()

	latest, err := latestVersion()
	must(t, err)
	if latest < 2 {
		t.Fatalf("need at least two migrations, have %d", latest)
	}
	if _, err := migrate(ctx, db, discard(), latest-1); err != nil {
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

func TestRoundTrip(t *testing.T) {
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
	if _, err := s.GetTarget(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetTarget missing: %v", err)
	}
	if _, err := s.GetWatch(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetWatch missing: %v", err)
	}
	if err := s.CreateTarget(ctx, store.Target{ID: "t1", Kind: "pypi"}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("duplicate target: %v", err)
	}
	if err := s.CreateWatch(ctx, store.Watch{ID: "w2", TargetID: "missing"}); err == nil || errors.Is(err, store.ErrConflict) {
		t.Errorf("foreign key not enforced: %v", err)
	}
	if err := s.CreateWatch(ctx, store.Watch{ID: "w3", TargetID: "t1", Policy: json.RawMessage(`{bad`)}); err == nil {
		t.Error("invalid JSON accepted")
	}
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
	e := store.Event{ID: "e1", WatchID: "w1", Type: "release.published", Payload: json.RawMessage(`{"v":1}`), CreatedAt: t0}
	must(t, s.AppendEvent(ctx, e, []store.Delivery{
		{ID: "d1", EventID: "e1", SubscriptionID: "s1", Status: "pending", NextAttemptAt: t0},
		{ID: "d2", EventID: "e1", SubscriptionID: "s2", Status: "pending", LastError: "boom"},
	}))
	ds, err := s.ListDeliveries(ctx, "e1")
	must(t, err)
	if len(ds) != 2 || !ds[0].NextAttemptAt.Equal(t0) || ds[1].LastError != "boom" || !ds[1].NextAttemptAt.IsZero() {
		t.Fatalf("deliveries = %+v", ds)
	}

	// A second delivery for (e2, s1) violates the unique key: nothing is written.
	e.ID = "e2"
	err = s.AppendEvent(ctx, e, []store.Delivery{
		{ID: "d3", EventID: "e2", SubscriptionID: "s1", Status: "pending"},
		{ID: "d4", EventID: "e2", SubscriptionID: "s1", Status: "pending"},
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate delivery: %v", err)
	}
	evs, err := s.ListEventsAfter(ctx, "", 10)
	must(t, err)
	if len(evs) != 1 || evs[0].ID != "e1" || string(evs[0].Payload) != `{"v":1}` || !evs[0].CreatedAt.Equal(t0) {
		t.Fatalf("events = %+v (outbox not atomic?)", evs)
	}
	if evs, _ := s.ListEventsAfter(ctx, "e1", 10); len(evs) != 0 {
		t.Fatalf("events after cursor = %+v", evs)
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

// TestConcurrentWriters runs writers from several goroutines; with WAL,
// busy_timeout and immediate transactions none may fail with SQLITE_BUSY.
func TestConcurrentWriters(t *testing.T) {
	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()
	const workers, perWorker = 16, 25
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker*3)
	for w := range workers {
		wg.Go(func() {
			for i := range perWorker {
				id := fmt.Sprintf("e%02d%03d", w, i)
				ds := []store.Delivery{{ID: "d" + id, EventID: id, SubscriptionID: "s1", Status: "pending"}}
				errs <- s.AppendEvent(ctx, store.Event{ID: id, WatchID: "w1", Type: "x", Payload: json.RawMessage(`{}`), CreatedAt: t0}, ds)
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
}

// latestVersion returns the highest embedded migration version.
func latestVersion() (int64, error) {
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return 0, fmt.Errorf("store: list migrations: %w", err)
	}
	var latest int64
	for _, n := range names {
		v, err := goose.NumericComponent(n)
		if err != nil {
			return 0, fmt.Errorf("store: migration %s: %w", n, err)
		}
		latest = max(latest, v)
	}
	return latest, nil
}
