-- Browser sessions of the admin web UI and the identity pins of its access
-- list (story #262, ADR 0033 §3 and §5): the twin of SQLite 00022, mapped as
-- in 00021 (TEXT COLLATE "C", BIGINT milliseconds, BYTEA). ui_session holds
-- only the SHA-256 hash of each session cookie. Additive only: the migration
-- creates two tables and rewrites no row.
--
-- Revocation is soft. name, avatar_url and user_agent are '' when unknown.
-- ui_identity_pin's login is stored in lower case. No foreign keys (ADR 0008).

-- +goose Up
CREATE TABLE ui_session (
    id           TEXT COLLATE "C" PRIMARY KEY,
    hash         BYTEA  NOT NULL CHECK (octet_length(hash) = 32),
    provider     TEXT COLLATE "C" NOT NULL,
    subject      TEXT COLLATE "C" NOT NULL,
    login        TEXT COLLATE "C" NOT NULL,
    name         TEXT COLLATE "C" NOT NULL,
    avatar_url   TEXT COLLATE "C" NOT NULL,
    user_agent   TEXT COLLATE "C" NOT NULL,
    created_at   BIGINT NOT NULL,
    last_seen_at BIGINT NOT NULL,
    expires_at   BIGINT NOT NULL,
    revoked_at   BIGINT,
    revoked_by   TEXT COLLATE "C"
);

CREATE TABLE ui_identity_pin (
    provider  TEXT COLLATE "C" NOT NULL,
    login     TEXT COLLATE "C" NOT NULL,
    subject   TEXT COLLATE "C" NOT NULL,
    pinned_at BIGINT NOT NULL,
    PRIMARY KEY (provider, login)
);

-- +goose Down
-- Destroys every UI session and identity pin: for development only, like the
-- other migrations' Down.
DROP TABLE ui_identity_pin;
DROP TABLE ui_session;
