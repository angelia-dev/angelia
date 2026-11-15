-- API tokens (story #72, ADR 0030 T3): the twin of SQLite 00021, mapped as
-- in 00020 (TEXT COLLATE "C", BIGINT milliseconds, BYTEA). The table holds
-- only the SHA-256 hash of each token (ADR 0030 T2). Additive only: the
-- migration creates a table and rewrites no row.
--
-- Revocation is soft and names are unique among active tokens only (partial
-- index). No foreign keys (ADR 0008).

-- +goose Up
CREATE TABLE api_token (
    id           TEXT COLLATE "C" PRIMARY KEY,
    name         TEXT COLLATE "C" NOT NULL,
    scope        TEXT COLLATE "C" NOT NULL CHECK (scope IN ('read', 'write', 'admin')),
    hash         BYTEA  NOT NULL CHECK (octet_length(hash) = 32),
    created_at   BIGINT NOT NULL,
    created_by   TEXT COLLATE "C" NOT NULL,
    expires_at   BIGINT,
    last_used_at BIGINT,
    revoked_at   BIGINT,
    revoked_by   TEXT COLLATE "C"
);

CREATE UNIQUE INDEX api_token_active_name ON api_token (name) WHERE revoked_at IS NULL;

-- +goose Down
-- Destroys every API token: for development only, like the other
-- migrations' Down.
DROP TABLE api_token;
