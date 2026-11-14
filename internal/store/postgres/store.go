package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/angelia-dev/angelia/internal/store"
)

// Advisory-lock kinds (ADR 0028 §4), the second key of
// pg_advisory_xact_lock(schema key, kind).
const (
	lockNone   int32 = 0
	lockConfig int32 = 1 // config, API and secret writes (§2)
	lockEvent  int32 = 2 // before every event insert (§1)
	lockLease  int32 = 3 // lease transactions, until #86 (§3)
)

// poolMaxConnsParam is the DSN parameter of the pool size (ADR 0028 §8).
const poolMaxConnsParam = "pool_max_conns"

// poolOnlyParams are the other pgxpool DSN parameters. database/sql has no
// equivalent the engine applies, and the server would refuse them as
// settings, so Open refuses them with a clear error.
var poolOnlyParams = []string{
	"pool_min_conns", "pool_min_idle_conns", "pool_max_conn_lifetime", "pool_max_conn_lifetime_jitter",
	"pool_max_conn_idle_time", "pool_health_check_period",
}

// Store is the Postgres engine. It is safe for concurrent use.
//
// Transactions run at READ COMMITTED. Config, API and secret transactions
// (and the Store methods that write config rows) take the config lock as
// their first statement; lease transactions take the lease lock as theirs;
// a check transaction takes the event lock before each event insert, so
// event.seq is in commit order (ADR 0028 §1-§4).
type Store struct {
	db  *sql.DB
	q   *Queries
	key int32 // schema key, hashtext(current_schema())

	// afterLeaseSelect, when set by a test, runs inside a lease transaction
	// between the select of due rows and the writes, so a test can change
	// those rows from another connection.
	afterLeaseSelect func()
}

var _ store.APIStore = (*Store)(nil)

// Open connects to the Postgres database named by dsn (a postgres:// URL or
// keyword/value pairs; the schema is the first existing one of its
// search_path) and migrates it to the latest schema version. The pool size is
// the DSN parameter pool_max_conns, which is removed before the DSN reaches
// the server (default: max(4, number of CPUs), like pgxpool). Neither the
// logs nor the errors ever hold the DSN. A nil logger means [slog.Default].
func Open(ctx context.Context, dsn string, logger *slog.Logger) (*Store, error) {
	if logger == nil {
		logger = slog.Default()
	}
	cfg, maxConns, err := parseDSN(dsn)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	fail := func(err error) (*Store, error) {
		return nil, errors.Join(redact(err, cfg.Password), db.Close())
	}
	key, err := SchemaKey(ctx, db)
	if err != nil {
		return fail(err)
	}
	if _, err := Migrate(ctx, db, logger); err != nil {
		return fail(err)
	}
	return &Store{db: db, q: New(db), key: key}, nil
}

// parseDSN parses dsn with pgx and takes pool_max_conns out of it. pgx does
// not know that parameter and would send it to the server as a setting, which
// refuses it (ADR 0028 §8).
func parseDSN(dsn string) (*pgx.ConnConfig, int, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		// pgx redacts the password it can find, but an unparsable string
		// may hide one where it does not look, and the wrapped error holds
		// the original text: say nothing about the DSN.
		return nil, 0, errors.New("store: postgres: the DSN cannot be parsed (its text is not shown: it may hold a password)")
	}
	// Autocommit statements run at READ COMMITTED too.
	cfg.RuntimeParams["default_transaction_isolation"] = "read committed"
	for _, p := range poolOnlyParams {
		if _, ok := cfg.RuntimeParams[p]; ok {
			return nil, 0, fmt.Errorf("store: postgres: the DSN parameter %s is not supported (only %s sets the pool): %w",
				p, poolMaxConnsParam, store.ErrInvalid)
		}
	}
	maxConns := max(4, runtime.NumCPU())
	if v, ok := cfg.RuntimeParams[poolMaxConnsParam]; ok {
		delete(cfg.RuntimeParams, poolMaxConnsParam)
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, 0, fmt.Errorf("store: postgres: %s must be a positive integer: %w", poolMaxConnsParam, store.ErrInvalid)
		}
		maxConns = n
	}
	return cfg, maxConns, nil
}

// redactedError hides a password in the text of an error, keeping the error
// chain for errors.Is and errors.As.
type redactedError struct {
	err      error
	password string
}

func (e redactedError) Error() string {
	if len(e.password) < minRedactLen {
		// Masking a very short password would mangle the text and could
		// still leave it recognisable: show none of it.
		return "store: postgres: cannot connect (the error text is not shown: it may hold the password)"
	}
	return strings.ReplaceAll(e.err.Error(), e.password, "xxxxx")
}

// minRedactLen is the shortest password redactedError masks in place.
const minRedactLen = 4

func (e redactedError) Unwrap() error { return e.err }

// redact returns err with password masked in its text.
func redact(err error, password string) error {
	if err == nil || password == "" {
		return err
	}
	return redactedError{err: err, password: password}
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// InTx implements [store.Store]. A check transaction takes no lock at BEGIN:
// AppendEvent takes the event lock before each insert.
func (s *Store) InTx(ctx context.Context, fn func(tx store.Tx) error) error {
	return s.withTx(ctx, lockNone, func(q *Queries) error { return fn(s.ops(q, kindCheck)) })
}

// withTx runs fn on queries bound to one transaction, committing on nil. A
// lock other than lockNone is taken as the transaction's first statement.
func (s *Store) withTx(ctx context.Context, lock int32, fn func(q *Queries) error) (err error) {
	// READ COMMITTED explicitly: the locking design of ADR 0028 assumes it,
	// whatever the server or role default (REPEATABLE READ would fail
	// writers with 40001 instead of re-checking their WHERE).
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("store: rollback: %w", rbErr))
		}
	}()
	q := s.q.WithTx(tx)
	if lock != lockNone {
		if err := q.AdvisoryXactLock(ctx, AdvisoryXactLockParams{SchemaKey: s.key, Kind: lock}); err != nil {
			return wrap(err, "advisory lock", strconv.Itoa(int(lock)))
		}
	}
	if err := fn(q); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return wrap(err, "commit", "")
	}
	return nil
}

// txKind tells the operations which unit of work they run in.
type txKind int

const (
	kindStatement txKind = iota // a Store method, one statement in autocommit
	kindCheck                   // InTx
	kindConfig                  // InConfigTx, InAPITx, InSecretTx
)

// txOps runs the operations of the transaction interfaces on q.
type txOps struct {
	q    *Queries
	key  int32
	kind txKind
}

func (s *Store) ops(q *Queries, kind txKind) txOps { return txOps{q: q, key: s.key, kind: kind} }

// stmt runs a single operation as one autocommit statement.
func (s *Store) stmt() txOps { return s.ops(s.q, kindStatement) }

// AddVersion implements [store.Tx] in its own statement.
func (s *Store) AddVersion(ctx context.Context, v store.Version) (bool, error) {
	return s.stmt().AddVersion(ctx, v)
}

// UpdateVersion implements [store.Tx] in its own statement.
func (s *Store) UpdateVersion(ctx context.Context, v store.Version) error {
	return s.stmt().UpdateVersion(ctx, v)
}

// MarkBaselineDone implements [store.Tx] in its own statement.
func (s *Store) MarkBaselineDone(ctx context.Context, watchID string) error {
	return s.stmt().MarkBaselineDone(ctx, watchID)
}

// ListVersions implements [store.Tx] outside a transaction.
func (s *Store) ListVersions(ctx context.Context, targetID string) ([]store.Version, error) {
	return s.stmt().ListVersions(ctx, targetID)
}

// ListWatches implements [store.Tx] outside a transaction (its share locks
// end with the statement).
func (s *Store) ListWatches(ctx context.Context, targetID string) ([]store.Watch, error) {
	return s.stmt().ListWatches(ctx, targetID)
}

// UpdateTarget implements [store.Tx] in its own statement.
func (s *Store) UpdateTarget(ctx context.Context, t store.Target) error {
	return s.stmt().UpdateTarget(ctx, t)
}

// AppendEvent implements [store.Tx] in its own transaction, under the event
// lock.
func (s *Store) AppendEvent(ctx context.Context, e store.Event, ds []store.Delivery) error {
	return s.InTx(ctx, func(tx store.Tx) error { return tx.AppendEvent(ctx, e, ds) })
}

// ListSubscriptions implements [store.Tx] outside a transaction.
func (s *Store) ListSubscriptions(ctx context.Context) ([]store.Subscription, error) {
	return s.stmt().ListSubscriptions(ctx)
}

// SubscriptionGeneration implements [store.Tx] outside a transaction.
func (s *Store) SubscriptionGeneration(ctx context.Context) (int64, error) {
	return s.stmt().SubscriptionGeneration(ctx)
}

// GetLatestEventBySubject implements [store.Tx] outside a transaction.
func (s *Store) GetLatestEventBySubject(ctx context.Context, watchID, subject string, types ...string) (store.Event, error) {
	return s.stmt().GetLatestEventBySubject(ctx, watchID, subject, types...)
}

// CancelHeldDeliveries implements [store.Tx] in its own statement.
func (s *Store) CancelHeldDeliveries(ctx context.Context, eventID string, now time.Time) (int64, error) {
	return s.stmt().CancelHeldDeliveries(ctx, eventID, now)
}

// ListPendingReleases implements [store.Tx] outside a transaction.
func (s *Store) ListPendingReleases(ctx context.Context, targetID string) ([]store.PendingRelease, error) {
	return s.stmt().ListPendingReleases(ctx, targetID)
}

// AddPendingRelease implements [store.Tx] in its own statement.
func (s *Store) AddPendingRelease(ctx context.Context, p store.PendingRelease) error {
	return s.stmt().AddPendingRelease(ctx, p)
}

// DeletePendingRelease implements [store.Tx] in its own statement.
func (s *Store) DeletePendingRelease(ctx context.Context, watchID, targetID, raw string) error {
	return s.stmt().DeletePendingRelease(ctx, watchID, targetID, raw)
}

// DeletePendingReleasesOfWatch implements [store.Tx] in its own statement.
func (s *Store) DeletePendingReleasesOfWatch(ctx context.Context, watchID string) (int64, error) {
	return s.stmt().DeletePendingReleasesOfWatch(ctx, watchID)
}

func (o txOps) AddVersion(ctx context.Context, v store.Version) (bool, error) {
	if v.FirstSeenAt.IsZero() {
		return false, fmt.Errorf("store: version %s@%s: zero first_seen_at: %w", v.TargetID, v.Raw, store.ErrInvalid)
	}
	n, err := o.q.AddVersion(ctx, AddVersionParams{
		TargetID: v.TargetID, Raw: v.Raw, Normalized: v.Normalized, PublishedAt: nullMillis(v.PublishedAt),
		Prerelease: v.Prerelease, Yanked: v.Yanked, FirstSeenAt: millis(v.FirstSeenAt), RemovedAt: nullMillis(v.RemovedAt),
	})
	return n == 1, wrap(err, "add version", v.TargetID+"@"+v.Raw)
}

func (o txOps) UpdateVersion(ctx context.Context, v store.Version) error {
	n, err := o.q.UpdateVersionState(ctx, UpdateVersionStateParams{
		Yanked: v.Yanked, RemovedAt: nullMillis(v.RemovedAt), TargetID: v.TargetID, Raw: v.Raw,
	})
	return deleted(n, err, "update version", v.TargetID+"@"+v.Raw)
}

func (o txOps) MarkBaselineDone(ctx context.Context, watchID string) error {
	n, err := o.q.MarkBaselineDone(ctx, watchID)
	return deleted(n, err, "mark baseline done", watchID)
}

func (o txOps) ListVersions(ctx context.Context, targetID string) ([]store.Version, error) {
	rows, err := o.q.ListVersions(ctx, targetID)
	if err != nil {
		return nil, wrap(err, "list versions", targetID)
	}
	out := make([]store.Version, len(rows))
	for i, r := range rows {
		out[i] = store.Version{
			TargetID: r.TargetID, Raw: r.Raw, Normalized: r.Normalized, PublishedAt: fromNullMillis(r.PublishedAt),
			Prerelease: r.Prerelease, Yanked: r.Yanked, FirstSeenAt: fromMillis(r.FirstSeenAt),
			RemovedAt: fromNullMillis(r.RemovedAt),
		}
	}
	return out, nil
}

func (o txOps) ListWatches(ctx context.Context, targetID string) ([]store.Watch, error) {
	rows, err := o.q.ListWatchesByTarget(ctx, targetID)
	if err != nil {
		return nil, wrap(err, "list watches", targetID)
	}
	return watchesFromRows(rows)
}

func (o txOps) ListPendingReleases(ctx context.Context, targetID string) ([]store.PendingRelease, error) {
	rows, err := o.q.ListPendingReleases(ctx, targetID)
	if err != nil {
		return nil, wrap(err, "list pending releases", targetID)
	}
	out := make([]store.PendingRelease, len(rows))
	for i, r := range rows {
		out[i] = store.PendingRelease{
			WatchID: r.WatchID, TargetID: r.TargetID, Raw: r.Raw, HeldAt: fromMillis(r.HeldAt),
			Release: json.RawMessage(r.ReleaseJson), Yanked: r.Yanked, Removed: r.RemovedAt.Valid,
		}
	}
	return out, nil
}

func (o txOps) AddPendingRelease(ctx context.Context, p store.PendingRelease) error {
	id := p.WatchID + "@" + p.Raw
	rel, err := rawJSON(p.Release, true)
	if err != nil {
		return fmt.Errorf("store: pending release %s: %w", id, err)
	}
	if p.HeldAt.IsZero() {
		return fmt.Errorf("store: pending release %s: zero held_at: %w", id, store.ErrInvalid)
	}
	n, err := o.q.AddPendingRelease(ctx, AddPendingReleaseParams{
		WatchID: p.WatchID, TargetID: p.TargetID, Raw: p.Raw, HeldAt: millis(p.HeldAt), ReleaseJson: rel,
	})
	if err == nil && n == 0 {
		return fmt.Errorf("store: add pending release %s: already held: %w", id, store.ErrConflict)
	}
	return wrap(err, "add pending release", id)
}

func (o txOps) DeletePendingRelease(ctx context.Context, watchID, targetID, raw string) error {
	n, err := o.q.DeletePendingRelease(ctx, DeletePendingReleaseParams{WatchID: watchID, TargetID: targetID, Raw: raw})
	return deleted(n, err, "delete pending release", watchID+"@"+targetID+"@"+raw)
}

func (o txOps) DeletePendingReleasesOfWatch(ctx context.Context, watchID string) (int64, error) {
	n, err := o.q.DeletePendingReleasesOfWatch(ctx, watchID)
	return n, wrap(err, "delete pending releases of watch", watchID)
}

func watchFromRow(r Watch) (store.Watch, error) {
	w := store.Watch{
		ID: r.ID, TargetID: r.TargetID, Policy: json.RawMessage(r.PolicyJson), BaselineDone: r.BaselineDone,
		Name: r.Name.String, ManagedBy: r.ManagedBy, Interval: fromNullInterval(r.IntervalMs),
	}
	if err := json.Unmarshal([]byte(r.LabelsJson), &w.Labels); err != nil {
		return store.Watch{}, fmt.Errorf("store: watch %s labels: %w", r.ID, err)
	}
	return w, nil
}

func watchesFromRows(rows []Watch) ([]store.Watch, error) {
	out := make([]store.Watch, len(rows))
	for i, r := range rows {
		var err error
		if out[i], err = watchFromRow(r); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// targetStateArgs validates t and encodes its fetch state.
func targetStateArgs(t store.Target) (string, error) {
	fs, err := rawJSON(t.FetchState, false)
	if err != nil {
		return "", fmt.Errorf("store: target %s fetch state: %w", t.ID, err)
	}
	if t.NextCheckAt.IsZero() {
		return "", fmt.Errorf("store: target %s: zero next_check_at: %w", t.ID, store.ErrInvalid)
	}
	return fs, nil
}

func (o txOps) UpdateTarget(ctx context.Context, t store.Target) error {
	fs, err := targetStateArgs(t)
	if err != nil {
		return err
	}
	n, err := o.q.UpdateTargetState(ctx, UpdateTargetStateParams{
		FetchStateJson: fs, LastCheckedAt: nullMillis(t.LastCheckedAt), NextCheckAt: millis(t.NextCheckAt),
		ErrorCount: t.ErrorCount, ID: t.ID,
	})
	return deleted(n, err, "update target", t.ID)
}

// deliveryRow is one element of the deliveries_json of CreateDeliveries,
// keyed by column name (json_populate_recordset).
type deliveryRow struct {
	ID                  string  `json:"id"`
	EventID             string  `json:"event_id"`
	SubscriptionID      string  `json:"subscription_id"`
	Status              string  `json:"status"`
	Attempts            int64   `json:"attempts"`
	NextAttemptAt       *int64  `json:"next_attempt_at"`
	LastError           *string `json:"last_error"`
	HeldUntil           *int64  `json:"held_until"`
	HoldStartedAt       *int64  `json:"hold_started_at"`
	HoldWatchCooldownMs *int64  `json:"hold_watch_cooldown_ms"`
	HoldPrerelease      *bool   `json:"hold_prerelease"`
	HoldWatchID         *string `json:"hold_watch_id"`
	HoldRaw             *string `json:"hold_raw"`
	HoldInput           *string `json:"hold_input"`
	Traceparent         *string `json:"traceparent"`
}

// AppendEvent takes the event lock before the insert, on every call (ADR
// 0028 §1: re-entrant, and safe after a rolled-back savepoint), then writes
// the event and all its deliveries in two statements. Config and API
// transactions append no event (agent-decided item 1).
func (o txOps) AppendEvent(ctx context.Context, e store.Event, ds []store.Delivery) error {
	if o.kind == kindConfig {
		return fmt.Errorf("store: event %s: AppendEvent in a config or API transaction: %w", e.ID, store.ErrInvalid)
	}
	payload, err := rawJSON(e.Payload, true)
	if err != nil {
		return fmt.Errorf("store: event %s payload: %w", e.ID, err)
	}
	if e.CreatedAt.IsZero() {
		return fmt.Errorf("store: event %s: zero created_at: %w", e.ID, store.ErrInvalid)
	}
	rows := make([]deliveryRow, len(ds))
	for i, d := range ds {
		if d.EventID != "" && d.EventID != e.ID {
			return fmt.Errorf("store: delivery %s belongs to event %s, not %s: %w", d.ID, d.EventID, e.ID, store.ErrInvalid)
		}
		r := deliveryRow{
			ID: d.ID, EventID: e.ID, SubscriptionID: d.SubscriptionID, Status: d.Status, Attempts: d.Attempts,
			NextAttemptAt: ptrMillis(d.NextAttemptAt), LastError: ptrString(d.LastError), Traceparent: ptrString(d.TraceParent),
		}
		if h := d.Hold; h != nil {
			if h.Until.IsZero() || h.StartedAt.IsZero() || d.Status != store.DeliveryPending {
				return fmt.Errorf("store: delivery %s: a hold needs Until, StartedAt and status pending: %w", d.ID, store.ErrInvalid)
			}
			cooldown, pre := h.WatchCooldown.Milliseconds(), h.Prerelease
			r.NextAttemptAt, r.HeldUntil, r.HoldStartedAt = ptrMillis(h.Until), ptrMillis(h.Until), ptrMillis(h.StartedAt)
			r.HoldWatchCooldownMs, r.HoldPrerelease = &cooldown, &pre
			r.HoldWatchID, r.HoldRaw, r.HoldInput = ptrString(h.WatchID), ptrString(h.Raw), ptrString(string(h.Input))
		}
		rows[i] = r
	}
	if err := o.q.AdvisoryXactLock(ctx, AdvisoryXactLockParams{SchemaKey: o.key, Kind: lockEvent}); err != nil {
		return wrap(err, "event lock", e.ID)
	}
	if err := o.q.CreateEvent(ctx, CreateEventParams{
		ID: e.ID, WatchID: e.WatchID, Type: e.Type, PayloadJson: payload, CreatedAt: millis(e.CreatedAt),
		RefEventID: nullString(e.RefEventID),
	}); err != nil {
		return wrap(err, "create event", e.ID)
	}
	if len(rows) == 0 {
		return nil
	}
	b, err := json.Marshal(rows)
	if err != nil {
		return fmt.Errorf("store: event %s deliveries: %w", e.ID, err)
	}
	return wrap(o.q.CreateDeliveries(ctx, b), "create deliveries of event", e.ID)
}

// LeaseDueTargets implements [store.Store]. It runs under the lease lock, so
// lease transactions of every process exclude each other (ADR 0028 §3). Each
// target is read back after its lease is written: one deleted since the
// select (DeleteTargetIfUnused takes no lease lock) is skipped and its new
// lease dropped.
func (s *Store) LeaseDueTargets(ctx context.Context, holder string, now, until time.Time, limit int) ([]store.Target, error) {
	if limit <= 0 {
		return nil, nil
	}
	if holder == "" || !until.After(now) {
		return nil, fmt.Errorf("store: lease due targets: empty holder or until not after now: %w", store.ErrInvalid)
	}
	var out []store.Target
	err := s.withTx(ctx, lockLease, func(q *Queries) error {
		ids, err := q.ListLeasableDueTargets(ctx, ListLeasableDueTargetsParams{Now: millis(now), MaxRows: maxRows(limit)})
		if err != nil {
			return wrap(err, "list due targets", holder)
		}
		s.leaseSelected()
		out = make([]store.Target, 0, len(ids))
		for _, id := range ids {
			name := store.TargetLeaseName(id)
			if err := acquire(ctx, q, name, holder, now, until); err != nil {
				return err
			}
			r, err := q.GetTarget(ctx, id)
			if errors.Is(err, sql.ErrNoRows) {
				if err := q.ReleaseLease(ctx, ReleaseLeaseParams{Name: name, Holder: holder}); err != nil {
					return wrap(err, "release lease", name)
				}
				continue
			}
			if err != nil {
				return wrap(err, "get target", id)
			}
			t, err := targetFromRow(r)
			if err != nil {
				return err
			}
			out = append(out, t)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// acquire replaces an expired lease and takes it for holder; the select
// excluded live leases, so a refusal is a bug.
func acquire(ctx context.Context, q *Queries, name, holder string, now, until time.Time) error {
	if err := q.DeleteExpiredLease(ctx, DeleteExpiredLeaseParams{Name: name, ExpiresAt: millis(now)}); err != nil {
		return wrap(err, "expire lease", name)
	}
	n, err := q.AcquireLease(ctx, AcquireLeaseParams{Name: name, Holder: holder, ExpiresAt: millis(until)})
	if err != nil {
		return wrap(err, "acquire lease", name)
	}
	if n != 1 {
		return fmt.Errorf("store: lease %s held by another holder: %w", name, store.ErrConflict)
	}
	return nil
}

func (s *Store) leaseSelected() {
	if s.afterLeaseSelect != nil {
		s.afterLeaseSelect()
	}
}

// HasDueTargets implements [store.Store] with a read only.
func (s *Store) HasDueTargets(ctx context.Context, now time.Time) (bool, error) {
	ok, err := s.q.HasLeasableDueTargets(ctx, millis(now))
	return ok, wrap(err, "has due targets", "")
}

// UpdateTargetLeased implements [store.Store], under the lease lock.
func (s *Store) UpdateTargetLeased(ctx context.Context, t store.Target, leaseName, holder string, now time.Time) (bool, error) {
	fs, err := targetStateArgs(t)
	if err != nil {
		return false, err
	}
	var held bool
	err = s.withTx(ctx, lockLease, func(q *Queries) error {
		n, err := q.UpdateTargetStateLeased(ctx, UpdateTargetStateLeasedParams{
			FetchStateJson: fs, LastCheckedAt: nullMillis(t.LastCheckedAt), NextCheckAt: millis(t.NextCheckAt),
			ErrorCount: t.ErrorCount, ID: t.ID, LeaseName: leaseName, Holder: holder, Now: millis(now),
		})
		if err != nil {
			return wrap(err, "update target", t.ID)
		}
		if n == 1 {
			held = true
			return nil
		}
		exists, err := q.TargetExists(ctx, t.ID)
		if err == nil && !exists {
			err = sql.ErrNoRows
		}
		return wrap(err, "update target", t.ID)
	})
	return held, err
}

// DeleteExpiredTargetLeases implements [store.Store], under the lease lock.
func (s *Store) DeleteExpiredTargetLeases(ctx context.Context, now time.Time) (int64, error) {
	var n int64
	err := s.withTx(ctx, lockLease, func(q *Queries) error {
		var err error
		n, err = q.DeleteExpiredTargetLeases(ctx, millis(now))
		return wrap(err, "delete expired target leases", "")
	})
	return n, err
}

// CountDueTargets implements [store.Store].
func (s *Store) CountDueTargets(ctx context.Context, now time.Time) (int64, error) {
	n, err := s.q.CountLeasableDueTargets(ctx, millis(now))
	return n, wrap(err, "count due targets", "")
}

// ListEventsAfter implements [store.Store]: one statement, so one snapshot.
// Event inserts hold the event lock until they commit, so the events it sees
// above the cursor are a prefix of what will ever commit there (ADR 0028 §1).
func (s *Store) ListEventsAfter(ctx context.Context, afterSeq int64, limit int) ([]store.Event, error) {
	if limit <= 0 {
		limit = store.DefaultEventLimit
	}
	rows, err := s.q.ListEventsAfter(ctx, ListEventsAfterParams{Seq: afterSeq, Limit: maxRows(limit)})
	if err != nil {
		return nil, wrap(err, "list events after", strconv.FormatInt(afterSeq, 10))
	}
	out := make([]store.Event, len(rows))
	for i, r := range rows {
		out[i] = eventFromRow(r)
	}
	return out, nil
}

func eventFromRow(r Event) store.Event {
	return store.Event{
		Seq: r.Seq, ID: r.ID, WatchID: r.WatchID, Type: r.Type, Payload: json.RawMessage(r.PayloadJson),
		CreatedAt: fromMillis(r.CreatedAt), RefEventID: r.RefEventID.String,
	}
}

func (o txOps) GetLatestEventBySubject(ctx context.Context, watchID, subject string, types ...string) (store.Event, error) {
	if len(types) == 0 {
		return store.Event{}, fmt.Errorf("store: latest event of watch %s: no event type: %w", watchID, store.ErrNotFound)
	}
	r, err := o.q.GetLatestEventBySubject(ctx, GetLatestEventBySubjectParams{WatchID: watchID, Subject: subject, TypesJson: jsonList(types)})
	if err != nil {
		return store.Event{}, wrap(err, "latest event of watch", watchID)
	}
	return eventFromRow(r), nil
}

func (o txOps) CancelHeldDeliveries(ctx context.Context, eventID string, now time.Time) (int64, error) {
	n, err := o.q.DeleteHeldDeliveriesOfEvent(ctx, DeleteHeldDeliveriesOfEventParams{EventID: eventID, Now: nullMillis(now)})
	return n, wrap(err, "cancel held deliveries of event", eventID)
}

// ListDeliveries implements [store.Store].
func (s *Store) ListDeliveries(ctx context.Context, eventID string) ([]store.Delivery, error) {
	rows, err := s.q.ListDeliveries(ctx, eventID)
	if err != nil {
		return nil, wrap(err, "list deliveries", eventID)
	}
	return deliveriesFromRows(rows), nil
}

func deliveryFromRow(r Delivery) store.Delivery {
	d := store.Delivery{
		ID: r.ID, EventID: r.EventID, SubscriptionID: r.SubscriptionID, Status: r.Status, Attempts: r.Attempts,
		ReplayBaseAttempts: r.ReplayBaseAttempts, NextAttemptAt: fromNullMillis(r.NextAttemptAt),
		LastError: r.LastError.String, TraceParent: r.Traceparent.String,
	}
	if r.HoldStartedAt.Valid {
		d.Hold = &store.Hold{
			Until: fromNullMillis(r.HeldUntil), StartedAt: fromNullMillis(r.HoldStartedAt),
			WatchCooldown: time.Duration(r.HoldWatchCooldownMs.Int64) * time.Millisecond,
			Prerelease:    r.HoldPrerelease.Bool, WatchID: r.HoldWatchID.String, Raw: r.HoldRaw.String,
		}
		if r.HoldInput.Valid {
			d.Hold.Input = json.RawMessage(r.HoldInput.String)
		}
	}
	return d
}

func deliveriesFromRows(rows []Delivery) []store.Delivery {
	out := make([]store.Delivery, len(rows))
	for i, r := range rows {
		out[i] = deliveryFromRow(r)
	}
	return out
}

// AcquireLease implements [store.Store], under the lease lock.
func (s *Store) AcquireLease(ctx context.Context, name, holder string, now, until time.Time) (bool, error) {
	var ok bool
	err := s.withTx(ctx, lockLease, func(q *Queries) error {
		if err := q.DeleteExpiredLease(ctx, DeleteExpiredLeaseParams{Name: name, ExpiresAt: millis(now)}); err != nil {
			return wrap(err, "expire lease", name)
		}
		n, err := q.AcquireLease(ctx, AcquireLeaseParams{Name: name, Holder: holder, ExpiresAt: millis(until)})
		ok = n == 1
		return wrap(err, "acquire lease", name)
	})
	return ok, err
}

// ReleaseLease implements [store.Store], under the lease lock.
func (s *Store) ReleaseLease(ctx context.Context, name, holder string) error {
	return s.withTx(ctx, lockLease, func(q *Queries) error {
		return wrap(q.ReleaseLease(ctx, ReleaseLeaseParams{Name: name, Holder: holder}), "release lease", name)
	})
}

// SQLSTATEs mapped onto store sentinels.
const (
	codeUniqueViolation     = "23505"
	codeForeignKeyViolation = "23503"
	codeCheckViolation      = "23514"
	codeInvalidText         = "22P02" // includes an invalid ::json cast
	codeUnsupportedEscape   = "22P05" // \u0000 in a JSON document
	codeNotInRepertoire     = "22021" // a NUL byte in a TEXT value
)

// wrap maps driver errors to store sentinels and adds context. nil stays nil.
// Other errors, such as a raised exception of the event_immutable trigger
// (P0001), a deadlock (40P01) or a lock timeout, are wrapped without a
// sentinel, like their SQLite counterparts.
func wrap(err error, op, id string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: %s %s: %w", op, id, store.ErrNotFound)
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case codeUniqueViolation:
			return fmt.Errorf("store: %s %s: %w: %w", op, id, store.ErrConflict, err)
		case codeForeignKeyViolation:
			return fmt.Errorf("store: %s %s: %w: %w", op, id, store.ErrInvalidRef, err)
		case codeCheckViolation, codeInvalidText, codeUnsupportedEscape, codeNotInRepertoire:
			return fmt.Errorf("store: %s %s: %w: %w", op, id, store.ErrInvalid, err)
		}
	}
	return fmt.Errorf("store: %s %s: %w", op, id, err)
}

// deleted turns the row count of a write by key into an error: ErrNotFound
// when no row matched.
func deleted(n int64, err error, op, id string) error {
	if err == nil && n == 0 {
		err = sql.ErrNoRows
	}
	return wrap(err, op, id)
}

// rawJSON validates m. Empty or null is "{}" when optional, ErrInvalid when
// required.
func rawJSON(m json.RawMessage, required bool) (string, error) {
	if t := strings.TrimSpace(string(m)); t == "" || t == "null" {
		if required {
			return "", fmt.Errorf("empty or null JSON: %w", store.ErrInvalid)
		}
		return "{}", nil
	}
	if !json.Valid(m) {
		return "", fmt.Errorf("invalid JSON: %w", store.ErrInvalid)
	}
	return string(m), nil
}

// jsonList encodes a list for json_array_elements_text; nil encodes as null.
func jsonList(v []string) json.RawMessage {
	b, _ := json.Marshal(v) // a []string always encodes
	return b
}

// maxRows converts a positive row limit to the int4 of a LIMIT parameter.
func maxRows(limit int) int32 {
	if limit > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(limit) //nolint:gosec // bounded above
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func millis(t time.Time) int64 { return t.UnixMilli() }

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func nullMillis(t time.Time) sql.NullInt64 {
	return sql.NullInt64{Int64: t.UnixMilli(), Valid: !t.IsZero()}
}

func fromNullMillis(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return fromMillis(n.Int64)
}

func ptrMillis(t time.Time) *int64 {
	if t.IsZero() {
		return nil
	}
	ms := t.UnixMilli()
	return &ms
}

func ptrString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func nullInterval(d time.Duration) sql.NullInt64 {
	return sql.NullInt64{Int64: d.Milliseconds(), Valid: d > 0}
}

func fromNullInterval(n sql.NullInt64) time.Duration {
	if !n.Valid {
		return 0
	}
	return time.Duration(n.Int64) * time.Millisecond
}
