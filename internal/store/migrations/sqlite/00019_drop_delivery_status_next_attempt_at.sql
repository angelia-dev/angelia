-- delivery_status_next_attempt_at (status, next_attempt_at), created by 00002,
-- is a prefix of delivery_status_next_attempt_at_id (status, next_attempt_at,
-- id), which 00018 added for the lease query (story #232). Every query that
-- could use the old index can use the new one at no worse a cost (checked
-- with EXPLAIN QUERY PLAN, story #234), so the old one only adds an index
-- write to each delivery insert and update. Dropping an index reads and
-- rewrites no row.
--
-- The down migration recreates the index exactly as 00002 defined it.
--
-- TODO(postgres, #85): the Postgres twin has no counterpart: it never creates
-- the prefix index, only the (status, next_attempt_at, id) btree.

-- +goose Up
DROP INDEX delivery_status_next_attempt_at;

-- +goose Down
CREATE INDEX delivery_status_next_attempt_at ON delivery (status, next_attempt_at);
