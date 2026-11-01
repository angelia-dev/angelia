-- Stable names, ownership and check interval for the watch file (story #48,
-- docs/watch-file.md decision D5). Additive: nothing is dropped or rewritten.
--
-- name       the key a declarative file uses; NULL for rows that have none.
-- managed_by who owns the row: 'api' (default, never touched by a file apply)
--            or 'file'. A file apply only reads and changes rows it owns.
-- interval_ms the target's check interval (the smallest interval among the
--            watches that use it); NULL until a watch file sets it.
-- SQLite treats NULLs as distinct in a unique index, so unnamed rows never
-- collide.

-- +goose Up
ALTER TABLE watch ADD COLUMN name TEXT;
ALTER TABLE watch ADD COLUMN managed_by TEXT NOT NULL DEFAULT 'api';
ALTER TABLE sink ADD COLUMN name TEXT;
ALTER TABLE sink ADD COLUMN managed_by TEXT NOT NULL DEFAULT 'api';
ALTER TABLE subscription ADD COLUMN name TEXT;
ALTER TABLE subscription ADD COLUMN managed_by TEXT NOT NULL DEFAULT 'api';
ALTER TABLE target ADD COLUMN interval_ms INTEGER;
CREATE UNIQUE INDEX watch_managed_by_name ON watch (managed_by, name);
CREATE UNIQUE INDEX sink_managed_by_name ON sink (managed_by, name);
CREATE UNIQUE INDEX subscription_managed_by_name ON subscription (managed_by, name);

-- +goose Down
DROP INDEX subscription_managed_by_name;
DROP INDEX sink_managed_by_name;
DROP INDEX watch_managed_by_name;
ALTER TABLE target DROP COLUMN interval_ms;
ALTER TABLE subscription DROP COLUMN managed_by;
ALTER TABLE subscription DROP COLUMN name;
ALTER TABLE sink DROP COLUMN managed_by;
ALTER TABLE sink DROP COLUMN name;
ALTER TABLE watch DROP COLUMN managed_by;
ALTER TABLE watch DROP COLUMN name;
