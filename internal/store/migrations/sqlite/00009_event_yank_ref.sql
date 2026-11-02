-- Yank events (story #112, ADR 0018). Additive only: existing rows get NULL
-- and no row is rewritten, so the event_immutable trigger (00007) is not
-- touched.
--
-- event.ref_event_id is set on a release.yanked event to the id of the event
-- the watch emitted for that version; NULL on every other event. It has no
-- foreign key, like the rest of the history rows (ADR 0008): retention may
-- delete the referenced event first.
--
-- event_watch_subject finds the event a watch emitted for a version (its
-- CloudEvents subject, <kind>:<package>@<raw version>) without scanning the
-- watch's events. The query must use the same expression to use the index.
--
-- TODO(postgres, #85): the Postgres engine needs the jsonb equivalent,
-- CREATE INDEX event_watch_subject ON event (watch_id, (payload_json->>'subject')),
-- and the lookup query written with the same expression.

-- +goose Up
ALTER TABLE event ADD COLUMN ref_event_id TEXT;
CREATE INDEX event_watch_subject ON event (watch_id, json_extract(payload_json, '$.subject'));

-- +goose Down
DROP INDEX event_watch_subject;
ALTER TABLE event DROP COLUMN ref_event_id;
