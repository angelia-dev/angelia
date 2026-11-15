package open

import (
	"context"
	"errors"
	"fmt"

	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/postgres"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
)

// TokenSchemaVersion is the first schema version with the api_token table
// (migration 00021, ADR 0030 T3).
const TokenSchemaVersion = 21

// ErrSchemaTooOld is returned by [TokenWriter] when the database has not been
// migrated to [TokenSchemaVersion] yet.
var ErrSchemaTooOld = errors.New("store: database schema too old")

// TokenStore writes API tokens. Close it after use.
type TokenStore interface {
	store.TokenStore
	Close() error
}

var (
	_ TokenStore = (*sqlite.Store)(nil)
	_ TokenStore = (*postgres.Store)(nil)
)

// checkTokenSchema refuses a schema version without the api_token table.
func checkTokenSchema(v int64) error {
	switch {
	case v == 0:
		return fmt.Errorf("%w: no Angelia tables in the database of database.dsn (check database.dsn, or start `angelia serve` once to create them)", ErrNoDatabase)
	case v < TokenSchemaVersion:
		return fmt.Errorf("%w: the database schema is at version %d and API tokens need %d or newer; upgrade the instance and start `angelia serve` once (it migrates the schema), then retry",
			ErrSchemaTooOld, v, TokenSchemaVersion)
	}
	return nil
}

// TokenWriter opens the store named by dsn for API token writes only, for
// `angelia token create --local` (ADR 0030 T7). Unlike [Open] it loads no
// secrets keyring, never creates the database and never migrates it, so a
// missing or rotated secrets key cannot block admin recovery and a newer CLI
// never migrates a database under a running older `serve`. It returns an
// error wrapping [ErrNoDatabase] when the database does not exist or Angelia
// never migrated it, and one wrapping [ErrSchemaTooOld] when its schema is
// older than [TokenSchemaVersion].
func TokenWriter(ctx context.Context, dsn string) (TokenStore, error) {
	e, err := EngineOf(dsn)
	if err != nil {
		return nil, err
	}
	if e == SQLite {
		path, err := sqlitePath(dsn)
		if err != nil {
			return nil, err
		}
		// The version is read read-only first: the read-write open sets
		// pragmas that write to the file, even to one it then refuses.
		v, err := sqlite.SchemaVersionOf(ctx, path)
		if errors.Is(err, sqlite.ErrNoDatabase) {
			return nil, fmt.Errorf("%w: %s (check database.dsn)", ErrNoDatabase, path)
		}
		if err != nil {
			return nil, err //nolint:wrapcheck // the engine's errors are already prefixed
		}
		if err := checkTokenSchema(v); err != nil {
			return nil, err
		}
		st, err := sqlite.OpenExisting(ctx, path)
		if err != nil {
			return nil, err //nolint:wrapcheck // the engine's errors are already prefixed
		}
		return st, nil
	}
	st, err := postgres.OpenExisting(ctx, postgresDSN(dsn))
	if errors.Is(err, postgres.ErrNoSchema) {
		return nil, fmt.Errorf("%w: the search_path of database.dsn names no existing schema (check database.dsn)", ErrNoDatabase)
	}
	if err != nil {
		return nil, noDB(err)
	}
	v, err := st.SchemaVersion(ctx)
	if err == nil {
		err = checkTokenSchema(v)
	}
	if err != nil {
		return nil, errors.Join(err, st.Close())
	}
	return st, nil
}
