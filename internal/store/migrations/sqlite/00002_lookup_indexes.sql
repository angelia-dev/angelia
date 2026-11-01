-- Indexes for the scheduler, dispatcher, lookups by parent row, and target
-- coalescing: coordinates_json is canonical JSON (keys sorted), so one
-- upstream object maps to one target row (architecture.md §3.2).

-- +goose Up
CREATE UNIQUE INDEX target_kind_coordinates ON target (kind, coordinates_json);
CREATE INDEX target_next_check_at ON target (next_check_at);
CREATE INDEX watch_target_id ON watch (target_id);
CREATE INDEX event_watch_id ON event (watch_id);
CREATE INDEX subscription_sink_id ON subscription (sink_id);
CREATE INDEX delivery_status_next_attempt_at ON delivery (status, next_attempt_at);
CREATE INDEX delivery_subscription_id ON delivery (subscription_id);

-- +goose Down
DROP INDEX delivery_subscription_id;
DROP INDEX delivery_status_next_attempt_at;
DROP INDEX subscription_sink_id;
DROP INDEX event_watch_id;
DROP INDEX watch_target_id;
DROP INDEX target_next_check_at;
DROP INDEX target_kind_coordinates;
