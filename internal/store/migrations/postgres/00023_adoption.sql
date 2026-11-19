-- Names of watch-file resources an admin handed over to the API with
-- POST /v1/adoptions (story #287): the twin of SQLite 00023, mapped as in
-- 00021 (TEXT COLLATE "C", BIGINT milliseconds). Additive only: the migration
-- creates one table and rewrites no row. No foreign keys (ADR 0008).

-- +goose Up
CREATE TABLE adoption (
    kind       TEXT COLLATE "C" NOT NULL,
    name       TEXT COLLATE "C" NOT NULL,
    adopted_at BIGINT NOT NULL,
    PRIMARY KEY (kind, name)
);

-- +goose Down
DROP TABLE adoption;
