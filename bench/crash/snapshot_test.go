package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store/sqlite"
)

// TestReadSnapshot reads a database made by angelia's own migrations, so
// a schema change that breaks the queries fails here and not in a run.
func TestReadSnapshot(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "angelia.db")
	st, err := sqlite.Open(ctx, path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	db, err := sqlOpenRW(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, q := range []string{
		`INSERT INTO target (id, kind, coordinates_json, next_check_at, last_checked_at) VALUES ('t0', 'pypi', '{"name":"pkg-00000"}', 0, 5)`,
		`INSERT INTO watch (id, target_id) VALUES ('w0', 't0')`,
		`INSERT INTO version (target_id, raw, normalized, first_seen_at) VALUES ('t0', '1.0.0', '1.0.0', 1), ('t0', '1.1.0', '1.1.0', 2)`,
		`INSERT INTO event (id, watch_id, type, payload_json, created_at) VALUES ('e1', 'w0', 'dev.angelia.release.published.v1', '{"data":{"package":"pkg-00000","version":"1.1.0"}}', 2)`,
		`INSERT INTO sink (id, kind, config_json, name) VALUES ('k0', 'webhook', CAST('{}' AS BLOB), 'hook-0')`,
		`INSERT INTO subscription (id, sink_id, name) VALUES ('s0', 'k0', 'sub-0')`,
		`INSERT INTO sink (id, kind, config_json, name) VALUES ('k1', 'webhook', CAST('{}' AS BLOB), 'hook-1')`,
		`INSERT INTO subscription (id, sink_id, name) VALUES ('s1', 'k1', 'sub-1')`,
		`INSERT INTO delivery (id, event_id, subscription_id, status, attempts) VALUES ('d0', 'e1', 's0', 'delivered', 2)`,
		`INSERT INTO delivery (id, event_id, subscription_id, status, attempts, last_error) VALUES ('d1', 'e1', 's1', 'in_flight', 1, 'boom')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	s, err := readSnapshot(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Packages["w0"] != "pkg-00000" || len(s.Versions["pkg-00000"]) != 2 || len(s.Events) != 1 || len(s.Deliveries) != 2 {
		t.Fatalf("snapshot %+v", s)
	}
	if e := s.Events[0]; e.Package != "pkg-00000" || e.Version != "1.1.0" || e.WatchID != "w0" {
		t.Errorf("event %+v", e)
	}
	if s.Subscriptions["s0"] != "hook-0" || s.Subscriptions["s1"] != "hook-1" {
		t.Errorf("subscriptions %v", s.Subscriptions)
	}
	if len(s.Integrity) != 0 {
		t.Errorf("integrity %v", s.Integrity)
	}
	state, err := readState(ctx, path, time.UnixMilli(1))
	if err != nil || state.Backlog != (Backlog{InFlight: 1}) || state.MaxSeq != 1 || state.Checks != 1 || !state.OldestCheck.Equal(time.UnixMilli(5)) {
		t.Errorf("state %+v, %v", state, err)
	}
	if later, _ := readState(ctx, path, time.UnixMilli(5)); later.Checks != 0 {
		t.Errorf("checks after the last check: %d", later.Checks)
	}

	// A row that breaks a foreign key is reported, not skipped.
	if _, err := db.ExecContext(ctx, `INSERT INTO delivery (id, event_id, subscription_id, status) VALUES ('d2', 'ghost', 's0', 'pending')`); err != nil {
		t.Fatal(err)
	}
	s, err = readSnapshot(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Integrity) != 1 || !strings.Contains(s.Integrity[0], "foreign_key_check: delivery") {
		t.Errorf("integrity %v", s.Integrity)
	}
}
