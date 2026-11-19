-- Names of watch-file resources an admin handed over to the API with
-- POST /v1/adoptions (story #287, spec docs/api.md item 55). Additive only:
-- the migration creates one table and rewrites no row.
--
-- One row per adopted kind ('sink', 'watch', 'subscription') and name. While
-- the watch file still declares that kind and name, the startup apply skips
-- the entry with a warning instead of failing on the taken name; once the
-- file no longer declares it, the apply deletes the row. adopted_at is in
-- milliseconds (ADR 0008). No foreign keys (ADR 0008).

-- +goose Up
CREATE TABLE adoption (
    kind       TEXT    NOT NULL,
    name       TEXT    NOT NULL,
    adopted_at INTEGER NOT NULL,
    PRIMARY KEY (kind, name)
) STRICT;

-- +goose Down
DROP TABLE adoption;
