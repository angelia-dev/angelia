-- Initial Postgres schema (story #85, ADR 0028 §7, decision D19). It creates
-- directly the schema that the SQLite migrations 00001-00020 build step by
-- step, so "version 20" is the same schema on both engines. Every later
-- SQLite migration gets a Postgres twin with the same number. The parity test
-- (internal/store/postgres/parity_test.go) compares the two catalogues.
--
-- Mapping (ADR 0028 §7): INTEGER millisecond timestamps and counters are
-- BIGINT (ADR 0008 §3); TEXT stays TEXT; BLOB is BYTEA; INTEGER 0/1 flags are
-- BOOLEAN, without the SQLite CHECK. JSON columns stay TEXT with a ::json CHECK
-- (the twin of json_valid; an invalid value fails the cast with 22P02), never
-- JSONB: event.payload_json is returned byte for byte (ADR 0015).
-- Steps of the SQLite history that only make sense on an existing SQLite
-- database (00011's duplicate-name refusal, 00013's sqlite_sequence recovery,
-- 00016's rename, 00019's index drop) have no counterpart: there is no older
-- Postgres database to upgrade.

-- +goose Up
CREATE TABLE target (
    id               TEXT   PRIMARY KEY,
    kind             TEXT   NOT NULL,
    coordinates_json TEXT   NOT NULL CHECK (coordinates_json::json IS NOT NULL),
    fetch_state_json TEXT   NOT NULL DEFAULT '{}' CHECK (fetch_state_json::json IS NOT NULL),
    last_checked_at  BIGINT,
    next_check_at    BIGINT NOT NULL,
    error_count      BIGINT NOT NULL DEFAULT 0,
    interval_ms      BIGINT,
    source_kind      TEXT
);

CREATE TABLE watch (
    id            TEXT    PRIMARY KEY,
    target_id     TEXT    NOT NULL REFERENCES target (id),
    policy_json   TEXT    NOT NULL DEFAULT '{}' CHECK (policy_json::json IS NOT NULL),
    labels_json   TEXT    NOT NULL DEFAULT '{}' CHECK (labels_json::json IS NOT NULL),
    baseline_done BOOLEAN NOT NULL DEFAULT false,
    name          TEXT,
    managed_by    TEXT    NOT NULL DEFAULT 'api',
    interval_ms   BIGINT
);

CREATE TABLE version (
    target_id     TEXT    NOT NULL REFERENCES target (id),
    raw           TEXT    NOT NULL,
    normalized    TEXT    NOT NULL,
    published_at  BIGINT,
    prerelease    BOOLEAN NOT NULL DEFAULT false,
    yanked        BOOLEAN NOT NULL DEFAULT false,
    first_seen_at BIGINT  NOT NULL,
    removed_at    BIGINT,
    PRIMARY KEY (target_id, raw)
);

-- seq is the pull-API cursor. CACHE 1 makes nextval strictly increasing
-- across sessions, which the commit-order argument of ADR 0028 §1 needs, and
-- an identity never hands a value out twice, so seq is not reused after
-- retention deletes the newest rows. GENERATED ALWAYS rejects an explicit seq.
CREATE TABLE event (
    seq          BIGINT GENERATED ALWAYS AS IDENTITY (CACHE 1) PRIMARY KEY,
    id           TEXT   NOT NULL UNIQUE,
    watch_id     TEXT   NOT NULL, -- no FK: the event log is immutable
    type         TEXT   NOT NULL,
    payload_json TEXT   NOT NULL CHECK (payload_json::json IS NOT NULL),
    created_at   BIGINT NOT NULL,
    ref_event_id TEXT
);

CREATE TABLE sink (
    id          TEXT  PRIMARY KEY,
    kind        TEXT  NOT NULL,
    config_json BYTEA NOT NULL,
    name        TEXT,
    managed_by  TEXT  NOT NULL DEFAULT 'api'
);

CREATE TABLE subscription (
    id          TEXT PRIMARY KEY,
    filter_json TEXT NOT NULL DEFAULT '{}' CHECK (filter_json::json IS NOT NULL),
    sink_id     TEXT NOT NULL REFERENCES sink (id),
    template    TEXT NOT NULL DEFAULT '',
    name        TEXT,
    managed_by  TEXT NOT NULL DEFAULT 'api',
    policy_json TEXT NOT NULL DEFAULT '{}' CHECK (policy_json::json IS NOT NULL)
);

CREATE TABLE delivery (
    id                     TEXT    PRIMARY KEY,
    event_id               TEXT    NOT NULL REFERENCES event (id),
    subscription_id        TEXT    NOT NULL, -- no FK: history outlives the subscription
    status                 TEXT    NOT NULL CHECK (status IN ('pending', 'in_flight', 'delivered', 'dead')),
    attempts               BIGINT  NOT NULL DEFAULT 0,
    next_attempt_at        BIGINT,
    last_error             TEXT,
    held_until             BIGINT,
    hold_started_at        BIGINT,
    hold_watch_cooldown_ms BIGINT,
    hold_prerelease        BOOLEAN,
    hold_watch_id          TEXT,
    hold_raw               TEXT,
    hold_input             TEXT,
    traceparent            TEXT,
    replay_base_attempts   BIGINT  NOT NULL DEFAULT 0,
    UNIQUE (event_id, subscription_id)
);

-- Named single-holder lease (ADR 0003, ADR 0028 §3).
CREATE TABLE lease (
    name       TEXT   PRIMARY KEY,
    holder     TEXT   NOT NULL,
    expires_at BIGINT NOT NULL
);

CREATE TABLE pending_release (
    watch_id     TEXT   NOT NULL REFERENCES watch (id) ON DELETE CASCADE,
    target_id    TEXT   NOT NULL,
    raw          TEXT   NOT NULL,
    held_at      BIGINT NOT NULL,
    release_json TEXT   NOT NULL CHECK (release_json::json IS NOT NULL),
    PRIMARY KEY (watch_id, target_id, raw),
    FOREIGN KEY (target_id, raw) REFERENCES version (target_id, raw) ON DELETE CASCADE
);

CREATE TABLE subscription_generation (
    id         BIGINT NOT NULL PRIMARY KEY CHECK (id = 1),
    generation BIGINT NOT NULL
);
INSERT INTO subscription_generation (id, generation) VALUES (1, 0);

CREATE TABLE event_prune (
    id                 BIGINT NOT NULL PRIMARY KEY CHECK (id = 1),
    highest_pruned_seq BIGINT NOT NULL
);
INSERT INTO event_prune (id, highest_pruned_seq) VALUES (1, 0);

CREATE TABLE secret (
    id         TEXT   PRIMARY KEY,
    name       TEXT   NOT NULL UNIQUE,
    hosts_json TEXT   NOT NULL DEFAULT '[]' CHECK (json_typeof(hosts_json::json) = 'array'),
    key_id     TEXT   NOT NULL,
    ciphertext BYTEA  NOT NULL,
    revision   BIGINT NOT NULL DEFAULT 1,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL
);

CREATE TABLE consumer (
    id          TEXT   PRIMARY KEY,
    name        TEXT   NOT NULL UNIQUE,
    filter_json TEXT   NOT NULL DEFAULT '{}' CHECK (filter_json::json IS NOT NULL),
    acked_seq   BIGINT NOT NULL DEFAULT 0 CHECK (acked_seq >= 0),
    acked_at    BIGINT,
    created_at  BIGINT NOT NULL,
    updated_at  BIGINT NOT NULL
);

-- Indexes, with the names the SQLite migrations give them.
CREATE UNIQUE INDEX target_kind_coordinates ON target (kind, coordinates_json);
CREATE INDEX target_next_check_at ON target (next_check_at);
CREATE INDEX watch_target_id ON watch (target_id);
CREATE UNIQUE INDEX watch_name ON watch (name);
CREATE INDEX event_watch_id ON event (watch_id);
-- The yank lookup (ADR 0018) must use this same expression to use the index.
CREATE INDEX event_watch_subject ON event (watch_id, ((payload_json::json) ->> 'subject'));
CREATE UNIQUE INDEX sink_name ON sink (name);
CREATE INDEX subscription_sink_id ON subscription (sink_id);
CREATE UNIQUE INDEX subscription_name ON subscription (name);
CREATE INDEX delivery_subscription_id ON delivery (subscription_id);
CREATE INDEX delivery_held ON delivery (subscription_id) WHERE held_until IS NOT NULL;
CREATE INDEX delivery_hold_version ON delivery (hold_watch_id, hold_raw) WHERE held_until IS NOT NULL;
-- The lease query orders by next_attempt_at NULLS FIRST, id (SQLite sorts
-- NULLs first, Postgres last by default), so the index is built in that order.
CREATE INDEX delivery_status_next_attempt_at_id ON delivery (status, next_attempt_at NULLS FIRST, id);
CREATE INDEX pending_release_target_id ON pending_release (target_id);
CREATE INDEX secret_key_id ON secret (key_id);

-- Release events are immutable once written (SQLite 00007). Retention still
-- deletes old rows.
-- +goose StatementBegin
CREATE FUNCTION event_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'event rows are immutable';
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER event_immutable BEFORE UPDATE ON event
    FOR EACH ROW EXECUTE FUNCTION event_immutable();

-- The highest pruned seq rises on every event delete (SQLite 00013).
-- +goose StatementBegin
CREATE FUNCTION event_prune_seq() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE event_prune SET highest_pruned_seq = greatest(highest_pruned_seq, OLD.seq) WHERE id = 1;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER event_prune_seq AFTER DELETE ON event
    FOR EACH ROW EXECUTE FUNCTION event_prune_seq();

-- The subscription generation (SQLite 00008) counts every insert and delete
-- of a subscription, and every update that changes id, filter_json or
-- policy_json. Row-level, like SQLite's triggers.
-- +goose StatementBegin
CREATE FUNCTION subscription_generation_bump() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE subscription_generation SET generation = generation + 1;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER subscription_generation_insert AFTER INSERT ON subscription
    FOR EACH ROW EXECUTE FUNCTION subscription_generation_bump();
CREATE TRIGGER subscription_generation_update AFTER UPDATE ON subscription
    FOR EACH ROW
    WHEN (OLD.id IS DISTINCT FROM NEW.id
       OR OLD.filter_json IS DISTINCT FROM NEW.filter_json
       OR OLD.policy_json IS DISTINCT FROM NEW.policy_json)
    EXECUTE FUNCTION subscription_generation_bump();
CREATE TRIGGER subscription_generation_delete AFTER DELETE ON subscription
    FOR EACH ROW EXECUTE FUNCTION subscription_generation_bump();

-- +goose Down
-- Destroys the whole schema and its data: for development only, like the
-- SQLite migrations' Down.
DROP TABLE consumer;
DROP TABLE secret;
DROP TABLE event_prune;
DROP TABLE subscription_generation;
DROP TABLE pending_release;
DROP TABLE lease;
DROP TABLE delivery;
DROP TABLE subscription;
DROP TABLE sink;
DROP TABLE event;
DROP TABLE version;
DROP TABLE watch;
DROP TABLE target;
DROP FUNCTION subscription_generation_bump();
DROP FUNCTION event_prune_seq();
DROP FUNCTION event_immutable();
