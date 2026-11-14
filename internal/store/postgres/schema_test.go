package postgres

import (
	"bytes"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// sqlState returns the SQLSTATE of a Postgres error, or "".
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// seed writes one row per config table, a version and an event, for the
// tests that need parent rows.
func seed(t *testing.T, db *sql.DB) {
	t.Helper()
	exec(t, db, `INSERT INTO target (id, kind, coordinates_json, next_check_at) VALUES ('t1', 'pypi', '{"package":"x"}', 0)`)
	exec(t, db, `INSERT INTO watch (id, target_id) VALUES ('w1', 't1')`)
	exec(t, db, `INSERT INTO version (target_id, raw, normalized, first_seen_at) VALUES ('t1', '1.0', '1.0', 0)`)
	exec(t, db, `INSERT INTO sink (id, kind, config_json) VALUES ('s1', 'webhook', '\x7b7d')`)
	exec(t, db, `INSERT INTO subscription (id, sink_id) VALUES ('sub1', 's1')`)
	exec(t, db, `INSERT INTO event (id, watch_id, type, payload_json, created_at) VALUES ('e1', 'w1', 'release.published', '{}', 0)`)
}

// TestSchemaConstraints checks by behaviour what the parity test compares by
// name only: each CHECK, NOT NULL and foreign key rejects a bad row with the
// SQLSTATE the engine maps onto a store sentinel (ADR 0028 §7).
func TestSchemaConstraints(t *testing.T) {
	db := openSchema(t)
	seed(t, db)
	const (
		invalidJSON = "22P02"
		check       = "23514"
		notNull     = "23502"
		foreignKey  = "23503"
		unique      = "23505"
	)
	tests := []struct {
		name, q, want string
	}{
		{"target coordinates_json", `INSERT INTO target (id, kind, coordinates_json, next_check_at) VALUES ('t2', 'npm', 'nope', 0)`, invalidJSON},
		{"target fetch_state_json", `UPDATE target SET fetch_state_json = '{' WHERE id = 't1'`, invalidJSON},
		{"target duplicate coordinates", `INSERT INTO target (id, kind, coordinates_json, next_check_at) VALUES ('t2', 'pypi', '{"package":"x"}', 0)`, unique},
		{"watch policy_json", `UPDATE watch SET policy_json = 'x' WHERE id = 'w1'`, invalidJSON},
		{"watch labels_json", `UPDATE watch SET labels_json = '[' WHERE id = 'w1'`, invalidJSON},
		{"watch unknown target", `INSERT INTO watch (id, target_id) VALUES ('w2', 'nope')`, foreignKey},
		{"watch duplicate name", `INSERT INTO watch (id, target_id, name) VALUES ('w2', 't1', 'n'), ('w3', 't1', 'n')`, unique},
		{"version unknown target", `INSERT INTO version (target_id, raw, normalized, first_seen_at) VALUES ('nope', '1', '1', 0)`, foreignKey},
		{"event payload_json", `INSERT INTO event (id, watch_id, type, payload_json, created_at) VALUES ('e2', 'w1', 't', '{"a":', 0)`, invalidJSON},
		{"event duplicate id", `INSERT INTO event (id, watch_id, type, payload_json, created_at) VALUES ('e1', 'w1', 't', '{}', 0)`, unique},
		{"sink config_json not null", `INSERT INTO sink (id, kind, config_json) VALUES ('s2', 'webhook', NULL)`, notNull},
		{"subscription filter_json", `UPDATE subscription SET filter_json = 'x' WHERE id = 'sub1'`, invalidJSON},
		{"subscription policy_json", `UPDATE subscription SET policy_json = 'x' WHERE id = 'sub1'`, invalidJSON},
		{"subscription unknown sink", `INSERT INTO subscription (id, sink_id) VALUES ('sub2', 'nope')`, foreignKey},
		{"delivery status", `INSERT INTO delivery (id, event_id, subscription_id, status) VALUES ('d1', 'e1', 'sub1', 'lost')`, check},
		{"delivery unknown event", `INSERT INTO delivery (id, event_id, subscription_id, status) VALUES ('d1', 'nope', 'sub1', 'pending')`, foreignKey},
		{"delivery duplicate", `INSERT INTO delivery (id, event_id, subscription_id, status) VALUES ('d1', 'e1', 'sub1', 'pending'), ('d2', 'e1', 'sub1', 'pending')`, unique},
		{"pending_release release_json", `INSERT INTO pending_release (watch_id, target_id, raw, held_at, release_json) VALUES ('w1', 't1', '1.0', 0, 'x')`, invalidJSON},
		{"pending_release unknown version", `INSERT INTO pending_release (watch_id, target_id, raw, held_at, release_json) VALUES ('w1', 't1', '9', 0, '{}')`, foreignKey},
		{"subscription_generation singleton", `INSERT INTO subscription_generation (id, generation) VALUES (2, 0)`, check},
		{"event_prune singleton", `INSERT INTO event_prune (id, highest_pruned_seq) VALUES (2, 0)`, check},
		{"secret hosts_json not an array", `INSERT INTO secret (id, name, hosts_json, key_id, ciphertext, created_at, updated_at) VALUES ('x', 'n', '{}', 'k', '\x00', 0, 0)`, check},
		{"secret hosts_json invalid", `INSERT INTO secret (id, name, hosts_json, key_id, ciphertext, created_at, updated_at) VALUES ('x', 'n', 'x', 'k', '\x00', 0, 0)`, invalidJSON},
		{"consumer filter_json", `INSERT INTO consumer (id, name, filter_json, created_at, updated_at) VALUES ('c', 'n', 'x', 0, 0)`, invalidJSON},
		{"consumer acked_seq", `INSERT INTO consumer (id, name, acked_seq, created_at, updated_at) VALUES ('c', 'n', -1, 0, 0)`, check},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := db.ExecContext(t.Context(), tt.q)
			if got := sqlState(err); got != tt.want {
				t.Fatalf("SQLSTATE %q (%v), want %q", got, err, tt.want)
			}
		})
	}

	t.Run("defaults", func(t *testing.T) {
		var policy, labels, managedBy string
		var baseline bool
		if err := db.QueryRowContext(t.Context(), `SELECT policy_json, labels_json, baseline_done, managed_by FROM watch WHERE id = 'w1'`).
			Scan(&policy, &labels, &baseline, &managedBy); err != nil {
			t.Fatal(err)
		}
		if policy != "{}" || labels != "{}" || baseline || managedBy != "api" {
			t.Fatalf("watch defaults = %q %q %v %q", policy, labels, baseline, managedBy)
		}
	})

	t.Run("cascades", func(t *testing.T) {
		exec(t, db, `INSERT INTO version (target_id, raw, normalized, first_seen_at) VALUES ('t1', '2.0', '2.0', 0)`)
		exec(t, db, `INSERT INTO pending_release (watch_id, target_id, raw, held_at, release_json) VALUES ('w1', 't1', '1.0', 0, '{}'), ('w1', 't1', '2.0', 0, '{}')`)
		exec(t, db, `DELETE FROM version WHERE raw = '2.0'`)
		if n := count(t, db, `SELECT count(*) FROM pending_release`); n != 1 {
			t.Fatalf("after deleting a version: %d held rows, want 1", n)
		}
		exec(t, db, `DELETE FROM watch WHERE id = 'w1'`)
		if n := count(t, db, `SELECT count(*) FROM pending_release`); n != 0 {
			t.Fatalf("after deleting the watch: %d held rows, want 0", n)
		}
	})
}

func count(t *testing.T, db *sql.DB, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRowContext(t.Context(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestEventImmutable(t *testing.T) {
	db := openSchema(t)
	seed(t, db)
	_, err := db.ExecContext(t.Context(), `UPDATE event SET type = 'x' WHERE id = 'e1'`)
	if err == nil || !strings.Contains(err.Error(), "event rows are immutable") {
		t.Fatalf("UPDATE event: %v, want the immutable error", err)
	}
	if n := count(t, db, `SELECT count(*) FROM event WHERE type = 'release.published'`); n != 1 {
		t.Fatal("the event changed")
	}
	exec(t, db, `DELETE FROM event WHERE id = 'e1'`) // retention still deletes
}

func TestSubscriptionGenerationTriggers(t *testing.T) {
	db := openSchema(t)
	seed(t, db) // inserts sub1: generation 1
	gen := func() int64 { return count(t, db, `SELECT generation FROM subscription_generation WHERE id = 1`) }
	steps := []struct {
		name, q string
		want    int64
	}{
		{"after seed", `SELECT 1`, 1},
		{"insert", `INSERT INTO subscription (id, sink_id) VALUES ('sub2', 's1')`, 2},
		{"update of a column the matcher ignores", `UPDATE subscription SET template = 'x', name = 'n', managed_by = 'file', sink_id = 's1' WHERE id = 'sub2'`, 2},
		{"update to the same filter", `UPDATE subscription SET filter_json = filter_json WHERE id = 'sub2'`, 2},
		{"update filter_json", `UPDATE subscription SET filter_json = '{"a":1}' WHERE id = 'sub2'`, 3},
		{"update policy_json", `UPDATE subscription SET policy_json = '{"b":1}' WHERE id = 'sub2'`, 4},
		{"update id", `UPDATE subscription SET id = 'sub3' WHERE id = 'sub2'`, 5},
		{"delete counts each row", `DELETE FROM subscription`, 7},
	}
	for _, s := range steps {
		exec(t, db, s.q)
		if got := gen(); got != s.want {
			t.Fatalf("%s: generation %d, want %d", s.name, got, s.want)
		}
	}
}

func TestEventPruneSeqTrigger(t *testing.T) {
	db := openSchema(t)
	for _, id := range []string{"a", "b", "c"} {
		exec(t, db, `INSERT INTO event (id, watch_id, type, payload_json, created_at) VALUES ($1, 'w', 't', '{}', 0)`, id)
	}
	pruned := func() int64 { return count(t, db, `SELECT highest_pruned_seq FROM event_prune WHERE id = 1`) }
	if got := pruned(); got != 0 {
		t.Fatalf("initial highest_pruned_seq = %d", got)
	}
	exec(t, db, `DELETE FROM event WHERE id = 'b'`)
	if got := pruned(); got != 2 {
		t.Fatalf("after deleting seq 2: %d", got)
	}
	exec(t, db, `DELETE FROM event WHERE id = 'a'`)
	if got := pruned(); got != 2 {
		t.Fatalf("deleting a lower seq lowered it to %d", got)
	}
}

// TestEventSeqIdentity: seq is an identity the store cannot set, drawn with
// CACHE 1 (ADR 0028 §1), and never reused after the newest rows are deleted.
func TestEventSeqIdentity(t *testing.T) {
	db := openSchema(t)
	insert := func(id string) int64 {
		t.Helper()
		var seq int64
		if err := db.QueryRowContext(t.Context(),
			`INSERT INTO event (id, watch_id, type, payload_json, created_at) VALUES ($1, 'w', 't', '{}', 0) RETURNING seq`, id).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		return seq
	}
	for i, id := range []string{"a", "b", "c"} {
		if seq := insert(id); seq != int64(i+1) {
			t.Fatalf("seq of %s = %d", id, seq)
		}
	}
	exec(t, db, `DELETE FROM event WHERE id IN ('b', 'c')`)
	if seq := insert("d"); seq != 4 {
		t.Fatalf("seq after deleting the newest rows = %d, want 4 (never reused)", seq)
	}

	_, err := db.ExecContext(t.Context(), `INSERT INTO event (seq, id, watch_id, type, payload_json, created_at) VALUES (99, 'x', 'w', 't', '{}', 0)`)
	if got := sqlState(err); got != "428C9" {
		t.Fatalf("explicit seq: SQLSTATE %q (%v), want 428C9 (GENERATED ALWAYS)", got, err)
	}
	if n := count(t, db, `SELECT s.cache_size FROM pg_sequences s
		WHERE s.schemaname = current_schema() AND s.sequencename = split_part(pg_get_serial_sequence('event', 'seq'), '.', 2)`); n != 1 {
		t.Fatalf("event seq cache = %d, want 1", n)
	}
}

// TestJSONColumnsByteForByte: event.payload_json and sink.config_json come
// back exactly as written, key order, whitespace and escapes included, which
// JSONB would not keep (ADR 0015, ADR 0028 §7).
func TestJSONColumnsByteForByte(t *testing.T) {
	db := openSchema(t)
	payload := "{ \"subject\" :\"pypi:x@1.0\",\n\t\"b\":1,  \"a\":[1 ,2],\"esc\":\"\\u00e9\\u0041\\/\\\"\", \"utf8\":\"é\", \"dup\":1, \"dup\":2 }"
	exec(t, db, `INSERT INTO event (id, watch_id, type, payload_json, created_at) VALUES ('e', 'w', 't', $1, 0)`, payload)
	var got string
	if err := db.QueryRowContext(t.Context(), `SELECT payload_json FROM event WHERE id = 'e'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != payload {
		t.Fatalf("payload_json changed:\n got %q\nwant %q", got, payload)
	}

	config := []byte("{\"url\":  \"https://example.invalid\" ,\"a\":\"\\u0000\", \"z\":1}\x00\xff")
	exec(t, db, `INSERT INTO sink (id, kind, config_json) VALUES ('s', 'webhook', $1)`, config)
	var gotConfig []byte
	if err := db.QueryRowContext(t.Context(), `SELECT config_json FROM sink WHERE id = 's'`).Scan(&gotConfig); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotConfig, config) {
		t.Fatalf("config_json changed:\n got %q\nwant %q", gotConfig, config)
	}

	// The yank lookup's expression finds the event by its subject (ADR 0018).
	if n := count(t, db, `SELECT count(*) FROM event WHERE watch_id = 'w' AND (payload_json::json) ->> 'subject' = 'pypi:x@1.0'`); n != 1 {
		t.Fatalf("subject lookup found %d events", n)
	}
}

// TestEventPayloadEscapedNUL records the difference ADR 0028 lists as an open
// question: the subject expression index evaluates ->>, which refuses a
// document holding an escaped NUL anywhere, so Postgres rejects an event that
// SQLite stores. 85-D (or the event builder) decides how to avoid it; this
// test changes when it does.
func TestEventPayloadEscapedNUL(t *testing.T) {
	db := openSchema(t)
	_, err := db.ExecContext(t.Context(), `INSERT INTO event (id, watch_id, type, payload_json, created_at) VALUES ('e', 'w', 't', $1, 0)`,
		`{"subject":"pypi:x@1.0","summary":"a\u0000b"}`)
	if got := sqlState(err); got != "22P05" {
		t.Fatalf("SQLSTATE %q (%v), want 22P05 (unsupported Unicode escape)", got, err)
	}
}

// TestIndexesServeTheirQueries: with sequential scans and sorts discouraged,
// the yank lookup uses event_watch_subject and the lease order
// (next_attempt_at NULLS FIRST, id) walks delivery_status_next_attempt_at_id.
func TestIndexesServeTheirQueries(t *testing.T) {
	db := openSchema(t)
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	for _, q := range []string{`SET enable_seqscan = off`, `SET enable_sort = off`} {
		if _, err := conn.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	for q, index := range map[string]string{
		`SELECT seq FROM event WHERE watch_id = 'w' AND (payload_json::json) ->> 'subject' = 's'`:            "event_watch_subject",
		`SELECT id FROM delivery WHERE status = 'pending' ORDER BY next_attempt_at NULLS FIRST, id LIMIT 10`: "delivery_status_next_attempt_at_id",
	} {
		rows, err := conn.QueryContext(t.Context(), "EXPLAIN "+q)
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(line + "\n")
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan.String(), index) || strings.Contains(plan.String(), "Sort") {
			t.Errorf("%s\nplan does not walk %s without sorting:\n%s", q, index, plan.String())
		}
	}
}
