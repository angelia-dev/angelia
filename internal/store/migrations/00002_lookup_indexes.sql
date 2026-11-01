-- Indexes for the scheduler, dispatcher and foreign-key lookups.

-- +goose Up
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
