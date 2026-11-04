-- Requested check interval per watch (story #185, spec docs/api.md decision
-- 10). Additive only: existing rows get NULL and no row is rewritten.
--
-- watch.interval_ms is the interval the watch asked for, so the API returns
-- the value that was PUT. target.interval_ms stays the effective interval: the
-- smallest interval among the watches on the target. NULL on rows written
-- before this migration until the next apply sets it.
--
-- TODO(postgres, #85): the Postgres engine needs the same column (BIGINT).

-- +goose Up
ALTER TABLE watch ADD COLUMN interval_ms INTEGER;

-- +goose Down
-- Discards the requested interval of every watch; target.interval_ms (the
-- effective one) is kept.
ALTER TABLE watch DROP COLUMN interval_ms;
