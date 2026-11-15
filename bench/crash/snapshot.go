package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // the database/sql driver of angelia's Postgres store
	_ "modernc.org/sqlite"             // the database/sql driver angelia's SQLite store uses
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
	// Integrity holds what the integrity checks reported (PRAGMA
	// integrity_check and foreign_key_check on SQLite, foreign key and
	// constraint queries on Postgres); empty when the database is sound.
	Integrity []string
	// Postgres is true when the snapshot came from Postgres.
	Postgres bool
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

// isPostgres reports whether ref, the "path" argument of readSnapshot and
// readState, is a Postgres DSN (an URL) and not the path of a SQLite file.
// This is the engine switch: every read below takes either.
func isPostgres(ref string) bool {
	return strings.HasPrefix(ref, "postgres://") || strings.HasPrefix(ref, "postgresql://")
}

// openDB opens angelia's database read-only: the SQLite file at ref, or the
// Postgres database ref names. The harness only reads; on Postgres the
// session is set read-only, so a bug here cannot change what is verified.
func openDB(ref string) (*sql.DB, error) {
	if isPostgres(ref) {
		u, err := url.Parse(ref)
		if err != nil {
			return nil, errors.New("the Postgres DSN is not a valid URL") // never echo it: it holds a password
		}
		q := u.Query()
		q.Set("default_transaction_read_only", "on")
		u.RawQuery = q.Encode()
		db, err := sql.Open("pgx", u.String())
		if err != nil {
			return nil, errors.New("open the Postgres database") // see above
		}
		db.SetMaxOpenConns(1)
		return db, nil
	}
	return sql.Open("sqlite", "file:"+(&url.URL{Path: ref}).EscapedPath()+"?mode=ro&_pragma=busy_timeout(10000)")
}

// readSnapshot reads the whole run from the database at path.
func readSnapshot(ctx context.Context, path string) (*Snapshot, error) {
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	s := &Snapshot{Packages: map[string]string{}, Versions: map[string][]StoredVersion{}, Subscriptions: map[string]string{}, Postgres: isPostgres(path)}
	targets := map[string]string{} // target id -> package
	if s.Postgres {
		s.Integrity, err = checkIntegrityPostgres(ctx, db)
	} else {
		s.Integrity, err = checkIntegrity(ctx, db)
	}
	if err != nil {
		return nil, fmt.Errorf("check the database: %w", err)
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
		return nil, fmt.Errorf("read the database: %w", err)
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
	arg := "?" // the placeholder of SQLite; Postgres numbers them
	if isPostgres(path) {
		arg = "$1"
	}
	err = db.QueryRowContext(ctx, `SELECT COALESCE((SELECT MAX(seq) FROM event), 0),
		(SELECT COUNT(*) FROM target WHERE last_checked_at > `+arg+`),
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

// foreignKeys are the foreign keys of angelia's schema (the migrations in
// internal/store/migrations/postgres): child table and columns, parent table
// and columns. Postgres enforces them on every write, so a violation after
// kills means a constraint was dropped, or a row was written around it;
// checkIntegrityPostgres reads them from the catalogue and cross-checks
// this list, so a new foreign key fails the benchmark's tests until it is
// listed here.
var foreignKeys = []struct {
	child, parent    string
	cols, parentCols []string
}{
	{"watch", "target", []string{"target_id"}, []string{"id"}},
	{"version", "target", []string{"target_id"}, []string{"id"}},
	{"subscription", "sink", []string{"sink_id"}, []string{"id"}},
	{"delivery", "event", []string{"event_id"}, []string{"id"}},
	{"pending_release", "watch", []string{"watch_id"}, []string{"id"}},
	{"pending_release", "version", []string{"target_id", "raw"}, []string{"target_id", "raw"}},
}

// checkIntegrityPostgres is the Postgres counterpart of checkIntegrity:
// there is no PRAGMA, so it looks for rows whose foreign key points at
// nothing (the same finding as foreign_key_check), constraints that are not
// validated and indexes that are not valid (what a crashed CREATE INDEX or
// ALTER leaves behind), and foreign keys in the catalogue that foreignKeys
// does not list.
func checkIntegrityPostgres(ctx context.Context, db *sql.DB) ([]string, error) {
	var problems []string
	for _, fk := range foreignKeys {
		var on, nulls []string
		for i, c := range fk.cols {
			on = append(on, "p."+fk.parentCols[i]+" = c."+c)
			nulls = append(nulls, "c."+c+" IS NOT NULL")
		}
		q := "SELECT count(*) FROM " + fk.child + " c WHERE " + strings.Join(nulls, " AND ") +
			" AND NOT EXISTS (SELECT 1 FROM " + fk.parent + " p WHERE " + strings.Join(on, " AND ") + ")"
		var n int64
		if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			return nil, fmt.Errorf("foreign key %s(%s) to %s: %w", fk.child, strings.Join(fk.cols, ", "), fk.parent, err)
		}
		if n > 0 {
			problems = append(problems, fmt.Sprintf("foreign_key_check: %d rows of %s refer to a missing %s", n, fk.child, fk.parent))
		}
	}
	err := query(ctx, db, `SELECT conrelid::regclass::text, conname, convalidated
		FROM pg_constraint WHERE connamespace = current_schema()::regnamespace AND contype IN ('f', 'c', 'u', 'p')`, func(r *sql.Rows) error {
		var rel, name string
		var validated bool
		if err := r.Scan(&rel, &name, &validated); err != nil {
			return err
		}
		if !validated {
			problems = append(problems, fmt.Sprintf("constraint %s on %s is not validated", name, rel))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = query(ctx, db, `SELECT i.indexrelid::regclass::text FROM pg_index i
		JOIN pg_class c ON c.oid = i.indrelid
		WHERE c.relnamespace = current_schema()::regnamespace AND NOT (i.indisvalid AND i.indisready)`, func(r *sql.Rows) error {
		var name string
		if err := r.Scan(&name); err != nil {
			return err
		}
		problems = append(problems, "index "+name+" is not valid")
		return nil
	})
	if err != nil {
		return nil, err
	}
	// A foreign key the list above lacks would escape the check.
	listed := map[string]bool{}
	for _, fk := range foreignKeys {
		listed[fk.child+">"+fk.parent] = true
	}
	err = query(ctx, db, `SELECT conrelid::regclass::text, confrelid::regclass::text FROM pg_constraint
		WHERE connamespace = current_schema()::regnamespace AND contype = 'f'`, func(r *sql.Rows) error {
		var child, parent string
		if err := r.Scan(&child, &parent); err != nil {
			return err
		}
		if !listed[child+">"+parent] {
			problems = append(problems, fmt.Sprintf("foreign key from %s to %s is not checked by the benchmark (add it to foreignKeys)", child, parent))
		}
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
