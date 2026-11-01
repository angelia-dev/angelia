-- Data model of docs/architecture.md §5.
-- IDs are ULID strings. Timestamps are INTEGER unix milliseconds, UTC.
-- *_json columns hold JSON as TEXT.

-- +goose Up
CREATE TABLE target (
    id               TEXT    PRIMARY KEY,
    kind             TEXT    NOT NULL,
    coordinates_json TEXT    NOT NULL CHECK (json_valid(coordinates_json)),
    fetch_state_json TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(fetch_state_json)),
    last_checked_at  INTEGER,
    next_check_at    INTEGER NOT NULL,
    error_count      INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE TABLE watch (
    id            TEXT    PRIMARY KEY,
    target_id     TEXT    NOT NULL REFERENCES target (id),
    policy_json   TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(policy_json)),
    labels_json   TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(labels_json)),
    baseline_done INTEGER NOT NULL DEFAULT 0 CHECK (baseline_done IN (0, 1))
) STRICT;

CREATE TABLE version (
    target_id     TEXT    NOT NULL REFERENCES target (id),
    raw           TEXT    NOT NULL,
    normalized    TEXT    NOT NULL,
    published_at  INTEGER,
    prerelease    INTEGER NOT NULL DEFAULT 0 CHECK (prerelease IN (0, 1)),
    yanked        INTEGER NOT NULL DEFAULT 0 CHECK (yanked IN (0, 1)),
    first_seen_at INTEGER NOT NULL,
    PRIMARY KEY (target_id, raw)
) STRICT;

CREATE TABLE event (
    id           TEXT    PRIMARY KEY,
    watch_id     TEXT    NOT NULL REFERENCES watch (id),
    type         TEXT    NOT NULL,
    payload_json TEXT    NOT NULL CHECK (json_valid(payload_json)),
    created_at   INTEGER NOT NULL
) STRICT;

CREATE TABLE sink (
    id                    TEXT PRIMARY KEY,
    kind                  TEXT NOT NULL,
    config_json_encrypted BLOB NOT NULL
) STRICT;

CREATE TABLE subscription (
    id          TEXT PRIMARY KEY,
    filter_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(filter_json)),
    sink_id     TEXT NOT NULL REFERENCES sink (id),
    template    TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE TABLE delivery (
    id              TEXT    PRIMARY KEY,
    event_id        TEXT    NOT NULL REFERENCES event (id),
    subscription_id TEXT    NOT NULL REFERENCES subscription (id),
    status          TEXT    NOT NULL CHECK (status IN ('pending', 'in_flight', 'delivered', 'dead')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER,
    last_error      TEXT,
    UNIQUE (event_id, subscription_id)
) STRICT;

-- Named single-holder lease used by the scheduler on SQLite (ADR 0003).
CREATE TABLE lease (
    name       TEXT    PRIMARY KEY,
    holder     TEXT    NOT NULL,
    expires_at INTEGER NOT NULL
) STRICT;

-- +goose Down
DROP TABLE lease;
DROP TABLE delivery;
DROP TABLE subscription;
DROP TABLE sink;
DROP TABLE event;
DROP TABLE version;
DROP TABLE watch;
DROP TABLE target;
