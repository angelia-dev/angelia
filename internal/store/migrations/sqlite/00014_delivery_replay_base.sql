-- Retry budget of a replayed delivery (story #71, spec docs/api.md decision
-- 28, ADR 0017 amendment). Additive only: existing rows get 0 and no row is
-- rewritten.
--
-- A replay keeps attempts (the count stays the delivery's history) and sets
-- replay_base_attempts to it, so that the dispatcher counts the attempts of
-- the replay, attempts - replay_base_attempts, against max_attempts. 0 means
-- never replayed: the budget counts from the first attempt.
--
-- TODO(postgres, #85): the Postgres engine needs the same column (BIGINT).

-- +goose Up
ALTER TABLE delivery ADD COLUMN replay_base_attempts INTEGER NOT NULL DEFAULT 0;

-- +goose Down
-- A delivery replayed after its budget ran out is dead again at its next
-- lease: the dispatcher counts every attempt once more.
ALTER TABLE delivery DROP COLUMN replay_base_attempts;
