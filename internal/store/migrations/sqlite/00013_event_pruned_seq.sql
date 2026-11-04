-- Highest pruned event seq (story #185, spec docs/api.md decision 12). A pull
-- cursor below it points into pruned events, and the API answers it with 410
-- cursor-expired. Additive only: no existing row changes.
--
-- event_prune holds one row. A trigger raises highest_pruned_seq on every
-- event delete, so the value is right whichever path deletes (today only
-- PruneHistory does). Event rows are never updated (00007), so seq values do
-- not move.
--
-- The initial value is recovered from the gaps that earlier prunes left: seq
-- is AUTOINCREMENT and never reused, and only pruning deletes events, so the
-- highest seq that was ever assigned (sqlite_sequence) and no longer exists
-- is the highest pruned one. That is the newest assigned seq when it is gone,
-- else the seq just below the highest kept event that has a gap below it.
-- 0 when nothing was pruned.
--
-- TODO(postgres, #85): the Postgres engine needs the same table and an
-- AFTER DELETE trigger.

-- +goose Up
CREATE TABLE event_prune (
    id                 INTEGER NOT NULL PRIMARY KEY CHECK (id = 1),
    highest_pruned_seq INTEGER NOT NULL
) STRICT;

INSERT INTO event_prune (id, highest_pruned_seq)
SELECT 1, coalesce(max(s), 0) FROM (
    SELECT q.seq AS s FROM sqlite_sequence q
    WHERE q.name = 'event' AND NOT EXISTS (SELECT 1 FROM event e WHERE e.seq = q.seq)
    UNION ALL
    SELECT e.seq - 1 FROM event e
    WHERE e.seq > 1 AND NOT EXISTS (SELECT 1 FROM event p WHERE p.seq = e.seq - 1)
);

-- +goose StatementBegin
CREATE TRIGGER event_prune_seq AFTER DELETE ON event
BEGIN
    UPDATE event_prune SET highest_pruned_seq = max(highest_pruned_seq, OLD.seq) WHERE id = 1;
END;
-- +goose StatementEnd

-- +goose Down
-- Discards the recorded highest pruned seq; up again recovers it from the
-- gaps in the event log.
DROP TRIGGER event_prune_seq;
DROP TABLE event_prune;
