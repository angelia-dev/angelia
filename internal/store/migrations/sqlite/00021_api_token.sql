-- API tokens (story #72, ADR 0030 T3). One row per token, holding only the
-- SHA-256 hash of the token (ADR 0030 T2), never the token. Additive only:
-- the migration creates a table and rewrites no row.
--
-- id is the token's ULID, the visible id inside the token (ADR 0030 T1).
-- Revocation is soft: a revoked row stays, so the audit trail can name it and
-- an id is never reused; names are unique among active tokens only (partial
-- index). No foreign keys, like the other history-like tables (ADR 0008).
-- last_used_at is coalesced by the authenticator (about 5 minutes, ADR 0030
-- T6). Timestamps are milliseconds (ADR 0008).

-- +goose Up
CREATE TABLE api_token (
    id           TEXT    PRIMARY KEY,
    name         TEXT    NOT NULL,
    scope        TEXT    NOT NULL CHECK (scope IN ('read', 'write', 'admin')),
    hash         BLOB    NOT NULL CHECK (length(hash) = 32),
    created_at   INTEGER NOT NULL,
    created_by   TEXT    NOT NULL,
    expires_at   INTEGER,
    last_used_at INTEGER,
    revoked_at   INTEGER,
    revoked_by   TEXT
) STRICT;

CREATE UNIQUE INDEX api_token_active_name ON api_token (name) WHERE revoked_at IS NULL;

-- +goose Down
-- Destroys every API token: for development only, like the other
-- migrations' Down.
DROP TABLE api_token;
