-- Named consumers (story #79-B, spec docs/api.md consumers): a server-side
-- cursor over the event log that an agent resumes from and advances with an
-- explicit ack. Additive only: the migration creates a table and rewrites no
-- row.
--
-- acked_seq is a position in the event log (event.seq), not a reference to a
-- row: it has no foreign key, because events are pruned and the cursor then
-- points into the pruned range (the API answers 410 cursor-expired, ADR 0008
-- keeps history independent of configuration). It only moves up: the ack is
-- a single UPDATE with max(acked_seq, ?). acked_at is NULL until the first
-- ack that moves the cursor. filter_json holds the stored filter
-- ({watch, type, source}); deleting a watch does not touch a consumer.
-- Timestamps are milliseconds (ADR 0008).
--
-- TODO(postgres, #85): the Postgres twin uses BIGINT and JSONB.

-- +goose Up
CREATE TABLE consumer (
    id          TEXT    PRIMARY KEY,
    name        TEXT    NOT NULL UNIQUE,
    filter_json TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(filter_json)),
    acked_seq   INTEGER NOT NULL DEFAULT 0 CHECK (acked_seq >= 0),
    acked_at    INTEGER,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
) STRICT;

-- +goose Down
-- Destroys every consumer cursor: for development only, like the other
-- migrations' Down.
DROP TABLE consumer;
