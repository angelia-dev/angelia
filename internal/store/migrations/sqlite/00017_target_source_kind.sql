-- target.source_kind is the kind of the source a target was created for
-- (story #205, named source instances). target.kind holds the name the watch
-- gave: a source kind, or the name of a configured instance of one, so that
-- two instances of one kind never share a target. NULL means "the same as
-- kind", which is every row written before this migration and every target
-- of a built-in kind. A check refuses a target whose source_kind differs from
-- the kind of the source now registered under its name.
--
-- Purely additive: a nullable column. No row is read, rewritten or dropped.
--
-- TODO(postgres, #85): the Postgres twin runs
-- ALTER TABLE target ADD COLUMN source_kind TEXT;

-- +goose Up
ALTER TABLE target ADD COLUMN source_kind TEXT;

-- +goose Down
-- Drops the column; the targets themselves are kept (a target of an
-- instance then reads as a source of kind <instance name>).
ALTER TABLE target DROP COLUMN source_kind;
