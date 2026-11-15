package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
)

// OpenExisting opens the database file at path for reading and writing, like
// [Open], but never creates the file and never migrates it: the store-only
// opener of `angelia token create --local` (ADR 0030 T7). The caller checks
// [SchemaVersionOf] first, since this open sets the pragmas of [Open] (WAL),
// which write to the file. ErrNoDatabase when the file does not exist.
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

// SchemaVersionOf reads the schema version of the database file at path
// through a read-only connection with no pragmas, so the file is left
// byte-for-byte as it was, even an empty one: see [Store.SchemaVersion].
// ErrNoDatabase when the file does not exist.
func SchemaVersionOf(ctx context.Context, path string) (int64, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return 0, ErrNoDatabase
	}
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro")
	if err != nil {
		return 0, fmt.Errorf("store: open %s: %w", path, err)
	}
	defer func() { _ = db.Close() }()
	return (&Store{db: db}).SchemaVersion(ctx)
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
