-- Names are unique per resource type across owners (story #185, spec
-- docs/api.md decision 2). The REST API addresses watches, sinks and
-- subscriptions by name, so a file row and an API row can no longer share one.
-- 00004 made (managed_by, name) unique; this makes name alone unique.
--
-- No row is changed. When duplicate names already exist the migration fails
-- with a message that lists them, and the database stays at 00010: rename or
-- delete one row of each duplicate, then start again. SQLite has no RAISE
-- outside a trigger, so a temporary trigger raises the message.
-- NULL names (rows without a name) never collide.
--
-- TODO(postgres, #85): the Postgres engine needs the same unique indexes and
-- the same refusal (a DO block that raises the list).

-- +goose Up
CREATE TEMP TABLE migration_00011_check (x INTEGER);

-- +goose StatementBegin
CREATE TEMP TRIGGER migration_00011_check BEFORE INSERT ON migration_00011_check
BEGIN
    SELECT RAISE(ABORT, 'migration 00011: names must be unique per resource type across owners; '
        || 'rename or delete one row of each, then start again. Duplicate names: '
        || (SELECT group_concat(kind || ' "' || name || '" (managed by ' || owners || ')', '; ')
            FROM (
                SELECT 'watch' AS kind, name, group_concat(managed_by, ', ') AS owners
                FROM (SELECT name, managed_by FROM watch WHERE name IS NOT NULL ORDER BY name, managed_by) GROUP BY name HAVING count(*) > 1
                UNION ALL
                SELECT 'sink', name, group_concat(managed_by, ', ')
                FROM (SELECT name, managed_by FROM sink WHERE name IS NOT NULL ORDER BY name, managed_by) GROUP BY name HAVING count(*) > 1
                UNION ALL
                SELECT 'subscription', name, group_concat(managed_by, ', ')
                FROM (SELECT name, managed_by FROM subscription WHERE name IS NOT NULL ORDER BY name, managed_by) GROUP BY name HAVING count(*) > 1
                ORDER BY 1, 2
            )))
    WHERE EXISTS (SELECT 1 FROM watch WHERE name IS NOT NULL GROUP BY name HAVING count(*) > 1)
       OR EXISTS (SELECT 1 FROM sink WHERE name IS NOT NULL GROUP BY name HAVING count(*) > 1)
       OR EXISTS (SELECT 1 FROM subscription WHERE name IS NOT NULL GROUP BY name HAVING count(*) > 1);
END;
-- +goose StatementEnd

INSERT INTO migration_00011_check (x) VALUES (1);
DROP TRIGGER migration_00011_check;
DROP TABLE migration_00011_check;

DROP INDEX watch_managed_by_name;
DROP INDEX sink_managed_by_name;
DROP INDEX subscription_managed_by_name;
CREATE UNIQUE INDEX watch_name ON watch (name);
CREATE UNIQUE INDEX sink_name ON sink (name);
CREATE UNIQUE INDEX subscription_name ON subscription (name);

-- +goose Down
-- Always possible: a unique name is also unique per (managed_by, name).
DROP INDEX subscription_name;
DROP INDEX sink_name;
DROP INDEX watch_name;
CREATE UNIQUE INDEX watch_managed_by_name ON watch (managed_by, name);
CREATE UNIQUE INDEX sink_managed_by_name ON sink (managed_by, name);
CREATE UNIQUE INDEX subscription_managed_by_name ON subscription (managed_by, name);
