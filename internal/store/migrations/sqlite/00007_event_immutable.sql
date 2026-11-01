-- Release events are immutable once written (story #106, architecture.md §2
-- decision 4): every delivery, retry and replay carries the same payload.
-- The store API has no update for events; this trigger also rejects an
-- UPDATE issued by any other path. Retention still deletes old rows.
-- Additive only: no existing row changes.

-- +goose Up
-- +goose StatementBegin
CREATE TRIGGER event_immutable BEFORE UPDATE ON event
BEGIN
    SELECT RAISE(ABORT, 'event rows are immutable');
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER event_immutable;
