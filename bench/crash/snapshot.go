package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite" // the database/sql driver angelia's store uses
)

// Snapshot is what angelia's database holds about the run, read after
// angelia stopped.
type Snapshot struct {
	// Packages maps a watch id to its package name (one watch per target).
	Packages map[string]string
	// Versions maps a package name to its recorded versions.
	Versions map[string][]StoredVersion
	Events   []StoredEvent
	// Subscriptions maps a subscription id to the name of its sink.
	Subscriptions map[string]string
	Deliveries    []StoredDelivery
	// Integrity holds what PRAGMA integrity_check and foreign_key_check
	// reported; empty when the database is sound.
	Integrity []string
}

// StoredVersion is a row of the version table.
type StoredVersion struct {
	Raw       string
	FirstSeen int64
}

// StoredEvent is a row of the event table with the fields of its payload
// the checks need.
type StoredEvent struct {
	ID, WatchID, Type, Package, Version string
}

// StoredDelivery is a row of the delivery table.
type StoredDelivery struct {
	ID, EventID, SubscriptionID, Status, LastError string
	Attempts                                       int64
}

// openDB opens angelia's SQLite database read-only.
func openDB(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro&_pragma=busy_timeout(10000)")
}

// readSnapshot reads the whole run from the database at path.
func readSnapshot(ctx context.Context, path string) (*Snapshot, error) {
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	s := &Snapshot{Packages: map[string]string{}, Versions: map[string][]StoredVersion{}, Subscriptions: map[string]string{}}
	targets := map[string]string{} // target id -> package
	s.Integrity, err = checkIntegrity(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("check %s: %w", path, err)
	}
	err = query(ctx, db, `SELECT w.id, t.id, t.coordinates_json FROM watch w JOIN target t ON t.id = w.target_id`, func(r *sql.Rows) error {
		var wid, tid, coords string
		if err := r.Scan(&wid, &tid, &coords); err != nil {
			return err
		}
		var c struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(coords), &c); err != nil {
			return fmt.Errorf("target %s coordinates: %w", tid, err)
		}
		s.Packages[wid], targets[tid] = c.Name, c.Name
		return nil
	})
	if err == nil {
		err = query(ctx, db, `SELECT target_id, raw, first_seen_at FROM version`, func(r *sql.Rows) error {
			var tid string
			var v StoredVersion
			if err := r.Scan(&tid, &v.Raw, &v.FirstSeen); err != nil {
				return err
			}
			s.Versions[targets[tid]] = append(s.Versions[targets[tid]], v)
			return nil
		})
	}
	if err == nil {
		err = query(ctx, db, `SELECT id, watch_id, type, payload_json FROM event ORDER BY seq`, func(r *sql.Rows) error {
			var e StoredEvent
			var payload string
			if err := r.Scan(&e.ID, &e.WatchID, &e.Type, &payload); err != nil {
				return err
			}
			var p struct {
				Data struct {
					Package string `json:"package"`
					Version string `json:"version"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(payload), &p); err != nil {
				return fmt.Errorf("event %s payload: %w", e.ID, err)
			}
			e.Package, e.Version = p.Data.Package, p.Data.Version
			s.Events = append(s.Events, e)
			return nil
		})
	}
	if err == nil {
		err = query(ctx, db, `SELECT s.id, COALESCE(k.name, '') FROM subscription s JOIN sink k ON k.id = s.sink_id`, func(r *sql.Rows) error {
			var id, name string
			if err := r.Scan(&id, &name); err != nil {
				return err
			}
			s.Subscriptions[id] = name
			return nil
		})
	}
	if err == nil {
		err = query(ctx, db, `SELECT id, event_id, subscription_id, status, attempts, COALESCE(last_error, '') FROM delivery`, func(r *sql.Rows) error {
			var d StoredDelivery
			if err := r.Scan(&d.ID, &d.EventID, &d.SubscriptionID, &d.Status, &d.Attempts, &d.LastError); err != nil {
				return err
			}
			s.Deliveries = append(s.Deliveries, d)
			return nil
		})
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return s, nil
}

// Backlog is the number of deliveries not finished yet.
type Backlog struct {
	Pending  int `json:"pending"`
	InFlight int `json:"in_flight"`
}

// State is what the harness reads from the database during a run.
type State struct {
	Backlog
	// MaxSeq is the newest event's sequence number.
	MaxSeq int64
	// Checks is the number of targets last checked after the since time
	// readState was given.
	Checks int
	// OldestCheck is the oldest last check among all targets (zero if a
	// target was never checked).
	OldestCheck time.Time
}

// readState reads the backlog, the event sequence and the checks committed
// after since. It works while angelia runs (WAL mode) and after it was
// killed.
func readState(ctx context.Context, path string, since time.Time) (State, error) {
	db, err := openDB(path)
	if err != nil {
		return State{}, err
	}
	defer func() { _ = db.Close() }()
	var st State
	err = query(ctx, db, `SELECT status, COUNT(*) FROM delivery WHERE status IN ('pending', 'in_flight') GROUP BY status`, func(r *sql.Rows) error {
		var status string
		var n int
		if err := r.Scan(&status, &n); err != nil {
			return err
		}
		if status == "pending" {
			st.Pending = n
		} else {
			st.InFlight = n
		}
		return nil
	})
	if err != nil {
		return st, err
	}
	var oldest int64
	err = db.QueryRowContext(ctx, `SELECT COALESCE((SELECT MAX(seq) FROM event), 0),
		(SELECT COUNT(*) FROM target WHERE last_checked_at > ?),
		COALESCE((SELECT MIN(COALESCE(last_checked_at, 0)) FROM target), 0)`, since.UnixMilli()).Scan(&st.MaxSeq, &st.Checks, &oldest)
	if oldest > 0 {
		st.OldestCheck = time.UnixMilli(oldest)
	}
	return st, err
}

// checkIntegrity runs SQLite's integrity and foreign key checks and
// returns every problem they report.
func checkIntegrity(ctx context.Context, db *sql.DB) ([]string, error) {
	var problems []string
	err := query(ctx, db, `PRAGMA integrity_check`, func(r *sql.Rows) error {
		var line string
		if err := r.Scan(&line); err != nil {
			return err
		}
		if line != "ok" {
			problems = append(problems, "integrity_check: "+line)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = query(ctx, db, `PRAGMA foreign_key_check`, func(r *sql.Rows) error {
		var table, parent string
		var rowid sql.NullInt64
		var fk int
		if err := r.Scan(&table, &rowid, &parent, &fk); err != nil {
			return err
		}
		problems = append(problems, fmt.Sprintf("foreign_key_check: %s row %d refers to a missing %s", table, rowid.Int64, parent))
		return nil
	})
	return problems, err
}

func query(ctx context.Context, db *sql.DB, q string, row func(*sql.Rows) error) error {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := row(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
