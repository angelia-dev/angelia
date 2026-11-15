package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// OpenExisting connects to the database named by dsn (same syntax and
// parameters as [Open]) for reading and writing, but never creates a schema
// and never migrates: the store-only opener of `angelia token create
// --local` (ADR 0030 T7). The caller checks [Store.SchemaVersion] before it
// writes. [ErrNoDatabase] when the database does not exist; any other
// failure is an error that never holds the password.
func OpenExisting(ctx context.Context, dsn string) (*Store, error) {
	cfg, maxConns, err := parseDSN(dsn)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(min(maxConns, 2))
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == invalidCatalogName {
			return nil, ErrNoDatabase
		}
		return nil, redact(fmt.Errorf("store: postgres: connect: %w", err), cfg.Password)
	}
	key, err := SchemaKey(ctx, db)
	if err != nil {
		return nil, errors.Join(redact(err, cfg.Password), db.Close())
	}
	return &Store{db: db, q: New(db), key: key}, nil
}

// SchemaVersion returns the highest applied migration version of the current
// schema, 0 when it holds no migration record (Angelia never migrated it).
func (s *Store) SchemaVersion(ctx context.Context) (int64, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, migrationsTable).Scan(&exists); err != nil {
		return 0, fmt.Errorf("store: postgres: read schema version: %w", err)
	}
	if !exists {
		return 0, nil
	}
	var v sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(version_id) FROM `+migrationsTable+` WHERE is_applied`).Scan(&v); err != nil {
		return 0, fmt.Errorf("store: postgres: read schema version: %w", err)
	}
	return v.Int64, nil
}
