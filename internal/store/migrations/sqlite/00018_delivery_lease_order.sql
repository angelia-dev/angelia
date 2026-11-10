-- The dispatcher's lease query (ListLeasableDueDeliveries, story #232) reads
-- the oldest due deliveries in (next_attempt_at, id) order. With an index on
-- (status, next_attempt_at) only, the rows of one event's fan-out share a
-- next_attempt_at and SQLite had to sort the whole due backlog on every
-- lease. This index holds the full order key per status, so each branch of
-- the query walks it in order and stops at its LIMIT.
--
-- Purely additive: one index. No row is read for rewrite, changed or dropped.
-- delivery_status_next_attempt_at is a prefix of it and is kept here, so
-- that this migration only adds; dropping it is left to a later story.
--
-- TODO(postgres, #85): the Postgres twin runs the same CREATE INDEX (a btree
-- on (status, next_attempt_at, id); Postgres sorts NULLs last by default, so
-- its lease query orders with NULLS FIRST to match SQLite, or the index is
-- created with next_attempt_at NULLS FIRST).

-- +goose Up
CREATE INDEX delivery_status_next_attempt_at_id ON delivery (status, next_attempt_at, id);

-- +goose Down
DROP INDEX delivery_status_next_attempt_at_id;
