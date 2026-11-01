package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path"

	"github.com/pressly/goose/v3"

	"github.com/angelia-dev/angelia/internal/store/migrations"
)

// migrationsTable records the applied schema versions.
const migrationsTable = "schema_migrations"

// migrate applies pending migrations up to version (0: the latest) and
// returns how many it applied. Running it on an up-to-date database is a no-op.
func migrate(ctx context.Context, db *sql.DB, logger *slog.Logger, version int64) (int, error) {
	p, err := goose.NewProvider(goose.DialectSQLite3, db, migrations.FS,
		goose.WithTableName(migrationsTable),
		goose.WithDisableGlobalRegistry(true),
	)
	if err != nil {
		return 0, fmt.Errorf("store: load migrations: %w", err)
	}
	var res []*goose.MigrationResult
	if version == 0 {
		res, err = p.Up(ctx)
	} else {
		res, err = p.UpTo(ctx, version)
	}
	if err != nil {
		return 0, fmt.Errorf("store: migrate: %w", err)
	}
	for _, r := range res {
		logger.InfoContext(ctx, "applied migration", "version", r.Source.Version, "file", path.Base(r.Source.Path), "duration", r.Duration)
	}
	return len(res), nil
}
