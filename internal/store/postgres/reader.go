package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/angelia-dev/angelia/internal/store"
)

// ErrNoDatabase says that the database named by the DSN does not exist on the
// server. [Reader.Initialized] tells whether Angelia ever created its tables
// in it.
var ErrNoDatabase = errors.New("store: postgres: the database does not exist")

// invalidCatalogName is the SQLSTATE of a connection to a database that does
// not exist.
const invalidCatalogName = "3D000"

// TargetSources lists the distinct sources the stored targets use, with how
// many targets each has, for `config check` and the start-up check of the
// configured source names (story #205). Like the SQLite engine's method of the
// same name, it is not part of [store.Store].
func (s *Store) TargetSources(ctx context.Context) ([]store.TargetSource, error) {
	return s.stmt().targetSources(ctx)
}

// TargetDemand groups the stored targets by source name and check interval,
// for the per-host request demand estimate of the scheduler (story #220).
func (s *Store) TargetDemand(ctx context.Context) ([]store.TargetDemand, error) {
	rows, err := s.q.ListTargetDemand(ctx)
	if err != nil {
		return nil, wrap(err, "list target demand", "")
	}
	out := make([]store.TargetDemand, len(rows))
	for i, r := range rows {
		out[i] = store.TargetDemand{Name: r.Kind, Interval: time.Duration(r.IntervalMs.Int64) * time.Millisecond, Targets: r.Targets}
	}
	return out, nil
}

func (o txOps) targetSources(ctx context.Context) ([]store.TargetSource, error) {
	rows, err := o.q.ListTargetSources(ctx)
	if err != nil {
		return nil, wrap(err, "list target sources", "")
	}
	out := make([]store.TargetSource, len(rows))
	for i, r := range rows {
		out[i] = store.TargetSource{Name: r.Kind, Kind: r.SourceKind.String, Targets: r.Targets}
	}
	return out, nil
}

// Reader reads stored secrets and target sources over connections that are
// read-only (default_transaction_read_only). It never creates a schema, a
// table or a migration record, and never runs the migrations, so
// `angelia config check`, `secrets status` and the start-up checks leave a
// database as they found it, whatever the DSN. It implements
// [store.SecretReader] on the tables that exist: a database Angelia has not
// reached reads as empty. Close it after use.
type Reader struct {
	db *sql.DB
	q  *Queries
}

var _ store.SecretReader = (*Reader)(nil)

// OpenReader connects to the database named by dsn (same syntax and
// parameters as [Open]) for reading only. It returns [ErrNoDatabase] when the
// database does not exist; any other failure is an error that never holds the
// password.
func OpenReader(ctx context.Context, dsn string) (*Reader, error) {
	cfg, _, err := parseDSN(dsn)
	if err != nil {
		return nil, err
	}
	cfg.RuntimeParams["default_transaction_read_only"] = "on"
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(2)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == invalidCatalogName {
			return nil, ErrNoDatabase
		}
		return nil, redact(fmt.Errorf("store: postgres: connect: %w", err), cfg.Password)
	}
	return &Reader{db: db, q: New(db)}, nil
}

// Close closes the connections.
func (r *Reader) Close() error { return r.db.Close() }

// has reports whether the table exists in the first schema of the search_path
// that holds it.
func (r *Reader) has(ctx context.Context, table string) (bool, error) {
	var ok bool
	if err := r.db.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&ok); err != nil {
		return false, fmt.Errorf("store: postgres: read schema: %w", err)
	}
	return ok, nil
}

// Initialized reports whether Angelia has created its tables here, which is
// what the instance-side commands need before they open the store.
func (r *Reader) Initialized(ctx context.Context) (bool, error) {
	return r.has(ctx, migrationsTable)
}

// ListSecrets implements [store.SecretReader].
func (r *Reader) ListSecrets(ctx context.Context) ([]store.Secret, error) {
	if ok, err := r.has(ctx, "secret"); err != nil || !ok {
		return nil, err
	}
	return txOps{q: r.q}.ListSecrets(ctx)
}

// SecretByName implements [store.SecretReader]; store.ErrNotFound without
// the secret table.
func (r *Reader) SecretByName(ctx context.Context, name string) (store.Secret, error) {
	ok, err := r.has(ctx, "secret")
	switch {
	case err != nil:
		return store.Secret{}, err
	case !ok:
		return store.Secret{}, store.ErrNotFound
	}
	return txOps{q: r.q}.SecretByName(ctx, name)
}

// CountSecretsByKeyID implements [store.SecretReader].
func (r *Reader) CountSecretsByKeyID(ctx context.Context) (map[string]int64, error) {
	if ok, err := r.has(ctx, "secret"); err != nil {
		return nil, err
	} else if !ok {
		return map[string]int64{}, nil
	}
	return txOps{q: r.q}.CountSecretsByKeyID(ctx)
}

// TargetSources lists the sources the stored targets use; none without the
// target table.
func (r *Reader) TargetSources(ctx context.Context) ([]store.TargetSource, error) {
	if ok, err := r.has(ctx, "target"); err != nil || !ok {
		return nil, err
	}
	return txOps{q: r.q}.targetSources(ctx)
}
