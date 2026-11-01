-- Subscription policy overrides (story #69, ADR 0014). Additive only: existing
-- rows get '{}' and NULL.
--
-- subscription.policy_json holds the narrowing keys of the subscription
-- (prerelease, cooldown), defaults filled.
--
-- A delivery held by a subscription cooldown stays an ordinary pending row
-- whose next_attempt_at is held_until, so a restart loses nothing and the
-- dispatcher needs no change. The other hold_* columns are what is needed to
-- decide again when the subscription is edited, or to cancel on a yank:
-- the instant the cooldown counts from, the watch cooldown it was measured
-- with, whether the version is a pre-release, the filter input the delivery
-- was matched with, and the (watch, raw version) it belongs to. They carry no foreign key, like the rest of delivery (ADR 0008).

-- +goose Up
ALTER TABLE subscription ADD COLUMN policy_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(policy_json));
ALTER TABLE delivery ADD COLUMN held_until             INTEGER;
ALTER TABLE delivery ADD COLUMN hold_started_at        INTEGER;
ALTER TABLE delivery ADD COLUMN hold_watch_cooldown_ms INTEGER;
ALTER TABLE delivery ADD COLUMN hold_prerelease        INTEGER;
ALTER TABLE delivery ADD COLUMN hold_watch_id          TEXT;
ALTER TABLE delivery ADD COLUMN hold_raw               TEXT;
ALTER TABLE delivery ADD COLUMN hold_input             TEXT;     -- the filter's input (JSON), to match again after an edit

CREATE INDEX delivery_held ON delivery (subscription_id) WHERE held_until IS NOT NULL;
CREATE INDEX delivery_hold_version ON delivery (hold_watch_id, hold_raw) WHERE held_until IS NOT NULL;

-- +goose Down
DROP INDEX delivery_hold_version;
DROP INDEX delivery_held;
ALTER TABLE delivery DROP COLUMN hold_input;
ALTER TABLE delivery DROP COLUMN hold_raw;
ALTER TABLE delivery DROP COLUMN hold_watch_id;
ALTER TABLE delivery DROP COLUMN hold_prerelease;
ALTER TABLE delivery DROP COLUMN hold_watch_cooldown_ms;
ALTER TABLE delivery DROP COLUMN hold_started_at;
ALTER TABLE delivery DROP COLUMN held_until;
ALTER TABLE subscription DROP COLUMN policy_json;
