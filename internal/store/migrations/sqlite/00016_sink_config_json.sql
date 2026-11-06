-- Rename sink.config_json_encrypted to sink.config_json (story #197, ADR 0024
-- decided question 4). Since ADR 0024 secret values live in the secret table
-- and the sink config holds only references, so the config is not encrypted
-- and the old name was wrong. Only the column is renamed: no row is read,
-- rewritten or dropped.
--
-- TODO(postgres, #85): the Postgres twin runs
-- ALTER TABLE sink RENAME COLUMN config_json_encrypted TO config_json;

-- +goose Up
ALTER TABLE sink RENAME COLUMN config_json_encrypted TO config_json;

-- +goose Down
ALTER TABLE sink RENAME COLUMN config_json TO config_json_encrypted;
