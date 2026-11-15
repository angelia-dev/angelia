package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
)

// OpenExisting opens the database file at path for reading and writing, like
// [Open], but never creates the file and never migrates it: the store-only
// opener of `angelia token create --local` (ADR 0030 T7). The caller checks
// [Store.SchemaVersion] before it writes. ErrNoDatabase when the file does
// not exist.
func OpenExisting(ctx context.Context, path string) (*Store, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoDatabase
	}
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("store: open %s: %w", path, err), db.Close())
	}
	return &Store{db: db, q: New(db)}, nil
}

// SchemaVersion returns the highest applied migration version, 0 when the
// database holds no migration record (Angelia never migrated it).
func (s *Store) SchemaVersion(ctx context.Context) (int64, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = '`+migrationsTable+`')`).Scan(&exists)
	if err != nil {
		return 0, fmt.Errorf("store: read schema version: %w", err)
	}
	if !exists {
		return 0, nil
	}
	var v sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(version_id) FROM `+migrationsTable+` WHERE is_applied`).Scan(&v); err != nil {
		return 0, fmt.Errorf("store: read schema version: %w", err)
	}
	return v.Int64, nil
}
