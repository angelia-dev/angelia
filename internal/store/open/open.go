// Package open is the engine-neutral way to reach the store: it chooses the
// SQLite or the Postgres engine from database.dsn and gives the commands one
// set of functions, the read-only helpers included (story #85, ADR 0028).
//
// The rule is the scheme of the DSN: sqlite is SQLite, postgres and
// postgresql are Postgres. There is no other form: a keyword/value Postgres
// DSN, a bare path or a file: URI is refused, as the configuration check
// already refuses it. Nothing here adds a configuration key: the engine,
// the pool size (pool_max_conns), TLS (sslmode) and the schema (search_path)
// all come from the DSN.
package open

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/postgres"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
)

// Engine names a store engine.
type Engine string

// The engines.
const (
	SQLite   Engine = "sqlite"
	Postgres Engine = "postgres"
)

// ErrNoDatabase is returned by the read-only helpers and by [Exists] when the
// database does not exist yet: a SQLite file that is missing, a Postgres
// database that is missing or that Angelia never migrated. Nothing here
// creates one.
var ErrNoDatabase = errors.New("store: database does not exist")

// Store is what the roles and commands need of an engine: the full
// [store.APIStore] (which includes [store.Store] and [store.SecretStore]) and
// the two queries both engines add for the start-up checks and the scheduler
// (neither is part of a store interface).
type Store interface {
	store.APIStore
	TargetSources(ctx context.Context) ([]store.TargetSource, error)
	TargetDemand(ctx context.Context) ([]store.TargetDemand, error)
}

var (
	_ Store = (*sqlite.Store)(nil)
	_ Store = (*postgres.Store)(nil)
)

// EngineOf returns the engine a DSN names, from its scheme (case-insensitive).
// The error never shows the DSN, which may hold a password.
func EngineOf(dsn string) (Engine, error) {
	u, err := url.Parse(dsn)
	// An opaque URL (postgres:foo) has no "//" authority: not a DSN of ours.
	if err != nil || u.Scheme == "" || u.Opaque != "" || !strings.Contains(dsn, "://") {
		return "", errors.New("database.dsn must be a URL: sqlite:///path or postgres://host/db")
	}
	switch strings.ToLower(u.Scheme) {
	case "sqlite":
		return SQLite, nil
	case "postgres", "postgresql":
		return Postgres, nil
	}
	return "", fmt.Errorf("database.dsn: unsupported scheme %q (want sqlite or postgres)", u.Scheme)
}

// sqlitePath returns the file of a sqlite DSN.
func sqlitePath(dsn string) (string, error) {
	return config.Database{DSN: dsn}.SQLitePath() //nolint:wrapcheck // its messages name database.dsn
}

// postgresDSN returns dsn for pgx, which reads only the lower-case scheme.
// The DSN is rebuilt from the parsed URL; callers have passed it to [EngineOf].
func postgresDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn // not reached after EngineOf; pgx then refuses it without echoing it
	}
	u.Scheme = strings.ToLower(u.Scheme)
	return u.String()
}

// Open opens (creating and migrating if needed) the store named by dsn. A nil
// logger means [slog.Default].
func Open(ctx context.Context, dsn string, log *slog.Logger) (Store, error) {
	e, err := EngineOf(dsn)
	if err != nil {
		return nil, err
	}
	if e == Postgres {
		st, err := postgres.Open(ctx, postgresDSN(dsn), log)
		if err != nil {
			return nil, err //nolint:wrapcheck // the engine's errors are already prefixed and redacted
		}
		return st, nil
	}
	path, err := sqlitePath(dsn)
	if err != nil {
		return nil, err
	}
	st, err := sqlite.Open(ctx, path, log)
	if err != nil {
		return nil, err //nolint:wrapcheck // the engine's errors are already prefixed
	}
	return st, nil
}

// Exists returns nil when the database holds an Angelia store, and an error
// wrapping [ErrNoDatabase] when it does not. The instance-side commands call
// it before they open the store, so that a mistyped DSN is an error and not a
// new empty database.
func Exists(ctx context.Context, dsn string) error {
	e, err := EngineOf(dsn)
	if err != nil {
		return err
	}
	if e == SQLite {
		path, err := sqlitePath(dsn)
		if err != nil {
			return err
		}
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s (check database.dsn)", ErrNoDatabase, path)
		}
		return nil
	}
	r, err := postgres.OpenReader(ctx, postgresDSN(dsn))
	if err != nil {
		return noDB(err)
	}
	defer func() { _ = r.Close() }()
	ok, err := r.Initialized(ctx)
	if err != nil {
		return err //nolint:wrapcheck // prefixed by the engine
	}
	if !ok {
		return fmt.Errorf("%w: no Angelia tables in the schema of database.dsn (check database.dsn)", ErrNoDatabase)
	}
	return nil
}

// noDB maps an engine's missing-database error onto [ErrNoDatabase].
func noDB(err error) error {
	if errors.Is(err, sqlite.ErrNoDatabase) || errors.Is(err, postgres.ErrNoDatabase) {
		return fmt.Errorf("%w: the database named by database.dsn does not exist", ErrNoDatabase)
	}
	return err
}

// SecretLookup reads stored secrets read-only. Close it after use.
type SecretLookup interface {
	SecretByName(ctx context.Context, name string) (store.Secret, error)
	Close() error
}

// SecretReader opens the database for reading secrets by name, without
// creating or migrating it. [ErrNoDatabase] when it does not exist.
func SecretReader(ctx context.Context, dsn string) (SecretLookup, error) {
	e, err := EngineOf(dsn)
	if err != nil {
		return nil, err
	}
	if e == SQLite {
		path, err := sqlitePath(dsn)
		if err != nil {
			return nil, err
		}
		r, err := sqlite.OpenSecretReader(ctx, path)
		if err != nil {
			return nil, noDB(err)
		}
		return r, nil
	}
	r, err := postgres.OpenReader(ctx, postgresDSN(dsn))
	if err != nil {
		return nil, noDB(err)
	}
	return r, nil
}

// SecretKeyCounts counts the stored secrets per key id, read-only. A database
// the secret migration has not reached has none. [ErrNoDatabase] when it does
// not exist.
func SecretKeyCounts(ctx context.Context, dsn string) (map[string]int64, error) {
	e, err := EngineOf(dsn)
	if err != nil {
		return nil, err
	}
	if e == SQLite {
		path, err := sqlitePath(dsn)
		if err != nil {
			return nil, err
		}
		c, err := sqlite.SecretKeyCounts(ctx, path)
		return c, noDB(err)
	}
	return withReader(ctx, dsn, func(r *postgres.Reader) (map[string]int64, error) { return r.CountSecretsByKeyID(ctx) })
}

// SecretHosts reads the name and hosts of every stored secret, read-only and
// without a ciphertext. A database the secret migration has not reached has
// none. [ErrNoDatabase] when it does not exist.
func SecretHosts(ctx context.Context, dsn string) (map[string][]string, error) {
	e, err := EngineOf(dsn)
	if err != nil {
		return nil, err
	}
	if e == SQLite {
		path, err := sqlitePath(dsn)
		if err != nil {
			return nil, err
		}
		h, err := sqlite.SecretHosts(ctx, path)
		return h, noDB(err)
	}
	return withReader(ctx, dsn, func(r *postgres.Reader) (map[string][]string, error) {
		rows, err := r.ListSecrets(ctx)
		out := make(map[string][]string, len(rows))
		for _, s := range rows {
			out[s.Name] = s.Hosts
		}
		return out, err
	})
}

// StoredTargetSources reads the sources the stored targets use, read-only.
// [ErrNoDatabase] when the database does not exist.
func StoredTargetSources(ctx context.Context, dsn string) ([]store.TargetSource, error) {
	e, err := EngineOf(dsn)
	if err != nil {
		return nil, err
	}
	if e == SQLite {
		path, err := sqlitePath(dsn)
		if err != nil {
			return nil, err
		}
		s, err := sqlite.StoredTargetSources(ctx, path)
		return s, noDB(err)
	}
	return withReader(ctx, dsn, func(r *postgres.Reader) ([]store.TargetSource, error) { return r.TargetSources(ctx) })
}

func withReader[T any](ctx context.Context, dsn string, fn func(*postgres.Reader) (T, error)) (T, error) {
	var zero T
	r, err := postgres.OpenReader(ctx, postgresDSN(dsn))
	if err != nil {
		return zero, noDB(err)
	}
	defer func() { _ = r.Close() }()
	return fn(r)
}
