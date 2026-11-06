-- Stored secrets, sealed with AES-256-GCM (story #75, ADR 0024 §6).
-- Additive only: the migration creates a table and rewrites no row (no
-- secret value is stored anywhere before it).
--
-- A secret is an opaque value of at most 64 KiB, referenced from
-- configuration by name with a *_ref key. ciphertext is the envelope of
-- ADR 0024 §3 (version, key id, nonce, GCM ciphertext and tag); its
-- associated data binds it to id and name, so a ciphertext copied into
-- another row, or a row renamed in SQL, no longer opens. key_id repeats
-- envelope bytes 1-8 in hex, so that startup and `secrets status` count
-- rows per key without decrypting. hosts_json is the host binding of
-- ADR 0024 §2 (not secret). revision is the optimistic lock of rotation on
-- Postgres (ADR 0024 §5).
--
-- TODO(postgres, #85): the Postgres twin uses BYTEA, BIGINT and JSONB.

-- +goose Up
CREATE TABLE secret (
    id         TEXT    PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE,
    hosts_json TEXT    NOT NULL DEFAULT '[]' CHECK (json_valid(hosts_json) AND json_type(hosts_json) = 'array'),
    key_id     TEXT    NOT NULL,
    ciphertext BLOB    NOT NULL,
    revision   INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;
CREATE INDEX secret_key_id ON secret (key_id);

-- +goose Down
-- Destroys every stored secret: for development only, like the other
-- migrations' Down.
DROP TABLE secret;
