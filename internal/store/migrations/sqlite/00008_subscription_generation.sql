-- Subscription generation (story #166). event.Emit caches the compiled
-- subscription matcher outside the write transaction; this counter says
-- whether the cache is still current. Triggers bump it on every insert,
-- update and delete of a subscription, so a write by the watch-file apply,
-- the API or another process is seen by the next Emit without any writer
-- having to remember. Additive only: no existing row changes.

-- +goose Up
CREATE TABLE subscription_generation (
    id         INTEGER NOT NULL PRIMARY KEY CHECK (id = 1),
    generation INTEGER NOT NULL
);
INSERT INTO subscription_generation (id, generation) VALUES (1, 0);

-- +goose StatementBegin
CREATE TRIGGER subscription_generation_insert AFTER INSERT ON subscription
BEGIN
    UPDATE subscription_generation SET generation = generation + 1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER subscription_generation_update AFTER UPDATE ON subscription
BEGIN
    UPDATE subscription_generation SET generation = generation + 1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER subscription_generation_delete AFTER DELETE ON subscription
BEGIN
    UPDATE subscription_generation SET generation = generation + 1;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER subscription_generation_delete;
DROP TRIGGER subscription_generation_update;
DROP TRIGGER subscription_generation_insert;
DROP TABLE subscription_generation;
