-- The dispatcher's lease query (ListLeasableDueDeliveries, story #232) reads
-- the oldest due deliveries in (next_attempt_at, id) order. With an index on
-- (status, next_attempt_at) only, the rows of one event's fan-out share a
-- next_attempt_at and SQLite had to sort the whole due backlog on every
-- lease. This index holds the full order key per status, so each branch of
-- the query walks it in order and stops at its LIMIT.
--
-- Purely additive: one index. No row is read for rewrite, changed or dropped.
-- delivery_status_next_attempt_at is a prefix of it and is kept here, so
-- that this migration only adds; 00019 (story #234) drops it.

-- +goose Up
CREATE INDEX delivery_status_next_attempt_at_id ON delivery (status, next_attempt_at, id);

-- +goose Down
DROP INDEX delivery_status_next_attempt_at_id;
