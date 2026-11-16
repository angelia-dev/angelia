-- Browser sessions of the admin web UI and the identity pins of its access
-- list (story #262, ADR 0033 §3 and §5). Additive only: the migration creates
-- two tables and rewrites no row.
--
-- ui_session holds one row per sign-in, with only the SHA-256 hash of the
-- session cookie (domain-separated as the API token hash, ADR 0030 T2), never
-- the cookie. id is the session's ULID, the visible id inside the cookie.
-- Revocation is soft, like api_token: a revoked row stays so the audit trail
-- can name it. name, avatar_url and user_agent are '' when unknown.
-- last_seen_at is coalesced by the authenticator (about 5 minutes); expires_at
-- is the absolute end (sign-in plus ui.session.max_lifetime).
--
-- ui_identity_pin binds a github_user entry of ui.access (a login, stored in
-- lower case) to the provider's numeric id seen at its first sign-in: a
-- later sign-in with that login and another id is refused. A pin is written
-- once and never changed by the server.
--
-- No foreign keys (ADR 0008). Timestamps are milliseconds (ADR 0008).

-- +goose Up
CREATE TABLE ui_session (
    id           TEXT    PRIMARY KEY,
    hash         BLOB    NOT NULL CHECK (length(hash) = 32),
    provider     TEXT    NOT NULL,
    subject      TEXT    NOT NULL,
    login        TEXT    NOT NULL,
    name         TEXT    NOT NULL,
    avatar_url   TEXT    NOT NULL,
    user_agent   TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    revoked_at   INTEGER,
    revoked_by   TEXT
) STRICT;

CREATE TABLE ui_identity_pin (
    provider  TEXT    NOT NULL,
    login     TEXT    NOT NULL,
    subject   TEXT    NOT NULL,
    pinned_at INTEGER NOT NULL,
    PRIMARY KEY (provider, login)
) STRICT;

-- +goose Down
-- Destroys every UI session and identity pin: for development only, like the
-- other migrations' Down.
DROP TABLE ui_identity_pin;
DROP TABLE ui_session;
