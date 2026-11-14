package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/pgtest"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// seedStore creates target t1, watch w1, sink k1 and subscription s1.
func seedStore(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	must(t, s.CreateTarget(ctx, store.Target{ID: "t1", Kind: "pypi", Coordinates: map[string]string{"name": "a"}, NextCheckAt: t0}))
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w1", TargetID: "t1"}))
	must(t, s.CreateSink(ctx, store.Sink{ID: "k1", Kind: "webhook", ConfigEncrypted: []byte(`{}`)}))
	must(t, s.CreateSubscription(ctx, store.Subscription{ID: "s1", SinkID: "k1"}))
}

func storeEvent(id, payload string) store.Event {
	return store.Event{ID: id, WatchID: "w1", Type: "release.published", Payload: json.RawMessage(payload), CreatedAt: t0}
}

// TestWrapMapsSQLStates: each SQLSTATE the schema raises maps onto the store
// sentinel the SQLite engine returns for the same case; the others, the
// event_immutable trigger's P0001 among them, keep no sentinel.
func TestWrapMapsSQLStates(t *testing.T) {
	sentinels := []error{store.ErrNotFound, store.ErrConflict, store.ErrInvalidRef, store.ErrInvalid}
	for _, c := range []struct {
		name string
		err  error
		want error // nil: no sentinel
	}{
		{"no rows", sql.ErrNoRows, store.ErrNotFound},
		{"unique violation", &pgconn.PgError{Code: "23505"}, store.ErrConflict},
		{"foreign key violation", &pgconn.PgError{Code: "23503"}, store.ErrInvalidRef},
		{"check violation", &pgconn.PgError{Code: "23514"}, store.ErrInvalid},
		{"invalid json", &pgconn.PgError{Code: "22P02"}, store.ErrInvalid},
		{"unsupported unicode escape", &pgconn.PgError{Code: "22P05"}, store.ErrInvalid},
		{"nul byte in text", &pgconn.PgError{Code: "22021"}, store.ErrInvalid},
		{"raised exception (event_immutable)", &pgconn.PgError{Code: "P0001"}, nil},
		{"deadlock", &pgconn.PgError{Code: "40P01"}, nil},
		{"other", errors.New("boom"), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := wrap(fmt.Errorf("driver: %w", c.err), "op", "id")
			for _, s := range sentinels {
				if errors.Is(got, s) != (c.want != nil && errors.Is(c.want, s)) {
					t.Errorf("wrap(%v) = %v; errors.Is(%v) = %v", c.err, got, s, errors.Is(got, s))
				}
			}
			if !errors.Is(got, c.err) && !errors.Is(c.err, sql.ErrNoRows) {
				t.Errorf("wrap(%v) = %v lost the driver error", c.err, got)
			}
		})
	}
	if wrap(nil, "op", "id") != nil {
		t.Error("wrap(nil) != nil")
	}
}

// TestEngineErrorsFromTheServer: the server's own errors reach the caller
// with the sentinels of TestWrapMapsSQLStates.
func TestEngineErrorsFromTheServer(t *testing.T) {
	s := openStore(t)
	seedStore(t, s)
	ctx := t.Context()
	must(t, s.AppendEvent(ctx, storeEvent("e1", `{"v":1}`), nil))

	// The schema refuses an UPDATE of an event (P0001): no sentinel, as on
	// SQLite.
	_, err := s.db.ExecContext(ctx, `UPDATE event SET type = 'x' WHERE id = 'e1'`)
	err = wrap(err, "update event", "e1")
	if err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("UPDATE event = %v, want the trigger's refusal", err)
	}
	for _, sentinel := range []error{store.ErrNotFound, store.ErrConflict, store.ErrInvalidRef, store.ErrInvalid} {
		if errors.Is(err, sentinel) {
			t.Errorf("UPDATE event = %v, maps to %v", err, sentinel)
		}
	}
	// A CHECK the engine does not validate first (23514).
	err = s.InTx(ctx, func(tx store.Tx) error {
		return tx.AppendEvent(ctx, storeEvent("e2", `{}`), []store.Delivery{{ID: "d", SubscriptionID: "s1", Status: "lost"}})
	})
	if !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("delivery with an unknown status = %v, want ErrInvalid", err)
	}
	// A NUL byte in a TEXT value (22021): Postgres stores none.
	if _, err := s.AddVersion(ctx, store.Version{TargetID: "t1", Raw: "1\x00", Normalized: "1", FirstSeenAt: t0}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("version with a NUL byte = %v, want ErrInvalid", err)
	}
}

// TestEventPayloadEscapedNULIsInvalid: an event payload holding the escape
// \u0000 is refused by the subject index (22P05, ADR 0028 §7) and returned as
// ErrInvalid. internal/event never writes one: it stores U+FFFD instead, on
// both engines (TestEncodePayloadReplacesNUL), so a check cannot fail on it.
func TestEventPayloadEscapedNULIsInvalid(t *testing.T) {
	s := openStore(t)
	seedStore(t, s)
	ctx := t.Context()
	err := s.AppendEvent(ctx, storeEvent("e1", `{"subject":"pypi:x@1.0","summary":"a\u0000b"}`), nil)
	if !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("AppendEvent with \\u0000 = %v, want ErrInvalid", err)
	}
	must(t, s.AppendEvent(ctx, storeEvent("e1", `{"subject":"pypi:x@1.0","summary":"a�b"}`), nil))
	ev, err := s.GetLatestEventBySubject(ctx, "w1", "pypi:x@1.0", "release.published")
	must(t, err)
	if string(ev.Payload) != `{"subject":"pypi:x@1.0","summary":"a�b"}` {
		t.Fatalf("payload = %s", ev.Payload)
	}
}

// TestYankLookupUsesSubjectIndex: the generated query of
// GetLatestEventBySubject repeats the index expression, collation included,
// so it walks event_watch_subject (ADR 0018, ADR 0028 §7).
func TestYankLookupUsesSubjectIndex(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	conn, err := s.db.Conn(ctx)
	must(t, err)
	defer func() { _ = conn.Close() }()
	for _, q := range []string{`SET enable_seqscan = off`, `SET enable_bitmapscan = off`} {
		_, err := conn.ExecContext(ctx, q)
		must(t, err)
	}
	rows, err := conn.QueryContext(ctx, "EXPLAIN "+getLatestEventBySubject, "w1", "pypi:x@1.0", `["release.published"]`)
	must(t, err)
	var plan strings.Builder
	for rows.Next() {
		var line string
		must(t, rows.Scan(&line))
		plan.WriteString(line + "\n")
	}
	must(t, rows.Close())
	if !strings.Contains(plan.String(), "event_watch_subject") {
		t.Fatalf("the yank lookup does not use event_watch_subject:\n%s", plan.String())
	}
}

// TestAppendEventTakesTheEventLock: an event transaction holds the event lock
// (schema key, 2) until it commits, and a transaction that appends nothing
// takes none (ADR 0028 §1).
func TestAppendEventTakesTheEventLock(t *testing.T) {
	s := openStore(t)
	seedStore(t, s)
	ctx := t.Context()
	held := func() int64 {
		t.Helper()
		var n int64
		must(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory' AND classid::bigint = ($1::bigint & 4294967295) AND objid = 2 AND objsubid = 2`, s.key).Scan(&n))
		return n
	}
	must(t, s.InTx(ctx, func(tx store.Tx) error {
		if _, err := tx.ListVersions(ctx, "t1"); err != nil {
			return err
		}
		if n := held(); n != 0 {
			t.Errorf("event lock held by a transaction without events: %d", n)
		}
		if err := tx.AppendEvent(ctx, storeEvent("e1", `{}`), []store.Delivery{{ID: "d1", SubscriptionID: "s1", Status: store.DeliveryPending}}); err != nil {
			return err
		}
		if n := held(); n != 1 {
			t.Errorf("event lock held %d times after AppendEvent, want 1", n)
		}
		return nil
	}))
	if n := held(); n != 0 {
		t.Fatalf("event lock still held after commit: %d", n)
	}
	ds, err := s.ListDeliveries(ctx, "e1")
	must(t, err)
	if len(ds) != 1 || ds[0].ID != "d1" {
		t.Fatalf("deliveries = %+v", ds)
	}
}

// TestAppendEventBatchesDeliveries: many deliveries of one event go in one
// statement and come back with every field (ADR 0028 §1).
func TestAppendEventBatchesDeliveries(t *testing.T) {
	s := openStore(t)
	seedStore(t, s)
	ctx := t.Context()
	const n = 2000
	ds := make([]store.Delivery, n)
	for i := range ds {
		ds[i] = store.Delivery{ID: fmt.Sprintf("d%05d", i), SubscriptionID: fmt.Sprintf("s%05d", i), Status: store.DeliveryPending, NextAttemptAt: t0}
	}
	ds[0].LastError, ds[0].TraceParent, ds[0].Attempts = `quote " and \ backslash`, "00-abc-def-01", 3
	ds[1].Hold = &store.Hold{
		Until: t0.Add(time.Hour), StartedAt: t0, WatchCooldown: time.Minute, Prerelease: true, WatchID: "w1", Raw: "1.0",
		Input: json.RawMessage(`{"labels": {"a":"\u0000"} }`),
	}
	must(t, s.AppendEvent(ctx, storeEvent("e1", `{}`), ds))
	got, err := s.ListDeliveries(ctx, "e1")
	must(t, err)
	if len(got) != n {
		t.Fatalf("%d deliveries, want %d", len(got), n)
	}
	if d := got[0]; d.LastError != ds[0].LastError || d.TraceParent != ds[0].TraceParent || d.Attempts != 3 || !d.NextAttemptAt.Equal(t0) || d.Hold != nil {
		t.Errorf("delivery 0 = %+v", d)
	}
	h := got[1].Hold
	if h == nil || !h.Until.Equal(t0.Add(time.Hour)) || !got[1].NextAttemptAt.Equal(h.Until) || !h.StartedAt.Equal(t0) ||
		h.WatchCooldown != time.Minute || !h.Prerelease || h.WatchID != "w1" || h.Raw != "1.0" || string(h.Input) != string(ds[1].Hold.Input) {
		t.Errorf("held delivery = %+v, hold %+v", got[1], h)
	}
	// A duplicate inside the batch is a conflict and writes nothing.
	dup := []store.Delivery{{ID: "x1", SubscriptionID: "s1", Status: store.DeliveryPending}, {ID: "x2", SubscriptionID: "s1", Status: store.DeliveryPending}}
	if err := s.AppendEvent(ctx, storeEvent("e2", `{}`), dup); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate deliveries = %v, want ErrConflict", err)
	}
	if _, err := s.GetEvent(ctx, "e2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("event of the refused batch: %v", err)
	}
}

// TestLeaseDueTargetsSkipsATargetDeletedMeanwhile: DeleteTargetIfUnused takes
// no lease lock, so a target can go between the select of due targets and
// its lease; it is skipped and its lease dropped (ADR 0028 §3).
func TestLeaseDueTargetsSkipsATargetDeletedMeanwhile(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	for _, id := range []string{"t1", "t2"} {
		must(t, s.CreateTarget(ctx, store.Target{ID: id, Kind: "pypi", Coordinates: map[string]string{"name": id}, NextCheckAt: t0}))
	}
	s.afterLeaseSelect = func() {
		must(t, s.InConfigTx(ctx, func(tx store.ConfigTx) error {
			ok, err := tx.DeleteTargetIfUnused(ctx, "t1")
			if err == nil && !ok {
				err = errors.New("t1 not deleted")
			}
			return err
		}))
	}
	got, err := s.LeaseDueTargets(ctx, "h", t0, t0.Add(time.Minute), 10)
	must(t, err)
	if len(got) != 1 || got[0].ID != "t2" {
		t.Fatalf("leased %+v, want t2 only", got)
	}
	if n := count(t, s.db, `SELECT count(*) FROM lease WHERE name = $1`, store.TargetLeaseName("t1")); n != 0 {
		t.Fatalf("the lease of the deleted target was kept")
	}
}

// TestLeaseDueDeliveriesSkipsRowsChangedMeanwhile: writers without the lease
// lock can fail or delete a selected delivery before it is set in flight. The
// failed one stays dead (not revived in flight), the deleted one does not
// abort the batch, and neither keeps a lease (ADR 0028 §3).
func TestLeaseDueDeliveriesSkipsRowsChangedMeanwhile(t *testing.T) {
	s := openStore(t)
	seedStore(t, s)
	ctx := t.Context()
	must(t, s.CreateSubscription(ctx, store.Subscription{ID: "s2", SinkID: "k1"}))
	must(t, s.CreateSubscription(ctx, store.Subscription{ID: "s3", SinkID: "k1"}))
	var ds []store.Delivery
	for _, sub := range []string{"s1", "s2", "s3"} {
		ds = append(ds, store.Delivery{ID: "d-" + sub, SubscriptionID: sub, Status: store.DeliveryPending, NextAttemptAt: t0})
	}
	must(t, s.AppendEvent(ctx, storeEvent("e1", `{}`), ds))
	s.afterLeaseSelect = func() {
		must(t, s.InConfigTx(ctx, func(tx store.ConfigTx) error {
			_, err := tx.FailPendingDeliveries(ctx, "s1", "subscription removed")
			return err
		}))
		_, err := s.db.ExecContext(ctx, `DELETE FROM delivery WHERE id = 'd-s2'`)
		must(t, err)
	}
	got, err := s.LeaseDueDeliveries(ctx, "h", t0, t0.Add(time.Minute), 10)
	must(t, err)
	if len(got) != 1 || got[0].Delivery.ID != "d-s3" || got[0].Delivery.Status != store.DeliveryInFlight {
		t.Fatalf("leased %+v, want d-s3 only", got)
	}
	d, err := s.GetDelivery(ctx, "d-s1")
	must(t, err)
	if d.Status != store.DeliveryDead || d.Attempts != 0 {
		t.Fatalf("failed delivery = %+v, want dead with no attempt", d)
	}
	if n := count(t, s.db, `SELECT count(*) FROM lease WHERE name IN ('delivery:d-s1', 'delivery:d-s2')`); n != 0 {
		t.Fatalf("%d leases kept for skipped deliveries", n)
	}
}

// TestOpenNeverShowsThePassword: a DSN that cannot connect, or cannot be
// parsed, fails without its password in the error or the log (#85).
func TestOpenNeverShowsThePassword(t *testing.T) {
	const password = "s3cret-Pa55word"
	for _, dsn := range []string{
		"postgres://angelia:" + password + "@127.0.0.1:1/angelia?sslmode=disable&connect_timeout=5",
		"host=127.0.0.1 port=1 user=angelia password=" + password + " dbname=angelia sslmode=disable connect_timeout=5",
		"postgres://angelia:" + password + "@127.0.0.1:notaport/angelia",
		"host=127.0.0.1 password='" + password,
	} {
		var logs bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		s, err := Open(ctx, dsn, logger)
		cancel()
		if err == nil {
			_ = s.Close()
			t.Fatalf("Open succeeded on an unreachable server")
		}
		if strings.Contains(err.Error(), password) || strings.Contains(logs.String(), password) {
			t.Errorf("the password leaked:\nerror: %v\nlog: %s", err, logs.String())
		}
	}
}

func TestRedactedError(t *testing.T) {
	inner := errors.New("auth for pw123 failed")
	err := redact(inner, "pw123")
	if err.Error() != "auth for xxxxx failed" || !errors.Is(err, inner) {
		t.Fatalf("redact = %q", err)
	}
	if got := redact(inner, ""); got.Error() != inner.Error() || redact(nil, "pw") != nil {
		t.Fatal("redact without password or error changed it")
	}
}

// TestPoolMaxConns: pool_max_conns sets the pool size and is not sent to the
// server, which would refuse it (ADR 0028 §8), in both DSN forms.
func TestPoolMaxConns(t *testing.T) {
	dsn := pgtest.DSN(t)
	withParam := dsn + " pool_max_conns=3"
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		withParam = dsn + "&pool_max_conns=3"
	}
	s := openStoreDSN(t, withParam)
	if n := s.db.Stats().MaxOpenConnections; n != 3 {
		t.Fatalf("pool size %d, want 3", n)
	}
	if def := openStoreDSN(t, dsn); def.db.Stats().MaxOpenConnections != max(4, runtime.NumCPU()) {
		t.Fatalf("default pool size %d", def.db.Stats().MaxOpenConnections)
	}
	for _, v := range []string{"0", "x"} {
		if _, _, err := parseDSN("host=localhost pool_max_conns=" + v); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("pool_max_conns=%s: %v, want ErrInvalid", v, err)
		}
	}
	cfg, n, err := parseDSN("postgres://u@localhost/db?pool_max_conns=7&application_name=x")
	must(t, err)
	if _, ok := cfg.RuntimeParams[poolMaxConnsParam]; ok || n != 7 || cfg.RuntimeParams["application_name"] != "x" {
		t.Fatalf("parseDSN: %d, runtime params %v", n, cfg.RuntimeParams)
	}
}

// TestOpenRefusesAMissingSchema: a search_path naming no existing schema has
// nowhere to put the tables (ADR 0028 §4).
func TestOpenRefusesAMissingSchema(t *testing.T) {
	dsn, err := pgtest.WithSearchPath(pgtest.ServerDSN(t), "angelia_test_does_not_exist")
	must(t, err)
	if _, err := Open(t.Context(), dsn, discardLogger()); !errors.Is(err, ErrNoSchema) {
		t.Fatalf("Open = %v, want ErrNoSchema", err)
	}
}
