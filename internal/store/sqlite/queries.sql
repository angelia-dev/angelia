-- name: CreateTarget :exec
INSERT INTO target (id, kind, coordinates_json, fetch_state_json, last_checked_at, next_check_at, error_count)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetTarget :one
SELECT * FROM target WHERE id = ?;

-- name: UpdateTargetState :execrows
UPDATE target
SET fetch_state_json = ?, last_checked_at = ?, next_check_at = ?, error_count = ?
WHERE id = ?;

-- name: CreateWatch :exec
INSERT INTO watch (id, target_id, policy_json, labels_json, baseline_done)
VALUES (?, ?, ?, ?, ?);

-- name: GetWatch :one
SELECT * FROM watch WHERE id = ?;

-- name: DeleteWatch :execrows
DELETE FROM watch WHERE id = ?;

-- name: AddVersion :execrows
INSERT INTO version (target_id, raw, normalized, published_at, prerelease, yanked, first_seen_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (target_id, raw) DO NOTHING;

-- name: ListVersions :many
SELECT * FROM version WHERE target_id = ? ORDER BY first_seen_at, raw;

-- name: CreateSink :exec
INSERT INTO sink (id, kind, config_json_encrypted) VALUES (?, ?, ?);

-- name: DeleteSink :execrows
DELETE FROM sink WHERE id = ?;

-- name: CreateSubscription :exec
INSERT INTO subscription (id, filter_json, sink_id, template) VALUES (?, ?, ?, ?);

-- name: DeleteSubscription :execrows
DELETE FROM subscription WHERE id = ?;

-- name: CreateEvent :exec
INSERT INTO event (id, watch_id, type, payload_json, created_at) VALUES (?, ?, ?, ?, ?);

-- name: ListEventsAfter :many
SELECT * FROM event WHERE seq > ? ORDER BY seq LIMIT ?;

-- name: CreateDelivery :exec
INSERT INTO delivery (id, event_id, subscription_id, status, attempts, next_attempt_at, last_error)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: ListDeliveries :many
SELECT * FROM delivery WHERE event_id = ? ORDER BY id;

-- name: PruneBatchEnd :many
SELECT seq FROM event WHERE seq > sqlc.arg(after_seq) AND created_at < sqlc.arg(before) ORDER BY seq LIMIT sqlc.arg(batch);

-- name: PruneDeliveries :execrows
DELETE FROM delivery
WHERE status IN ('delivered', 'dead')
  AND event_id IN (
    SELECT e.id FROM event e
    WHERE e.seq > sqlc.arg(after_seq) AND e.seq <= sqlc.arg(up_to_seq) AND e.created_at < sqlc.arg(before)
      AND NOT EXISTS (
        SELECT 1 FROM delivery p WHERE p.event_id = e.id AND p.status IN ('pending', 'in_flight')
      )
  );

-- name: PruneEvents :execrows
DELETE FROM event
WHERE seq > sqlc.arg(after_seq) AND seq <= sqlc.arg(up_to_seq) AND created_at < sqlc.arg(before)
  AND NOT EXISTS (SELECT 1 FROM delivery d WHERE d.event_id = event.id);

-- name: DeleteExpiredLease :exec
DELETE FROM lease WHERE name = ? AND expires_at <= ?;

-- name: AcquireLease :execrows
INSERT INTO lease (name, holder, expires_at) VALUES (?, ?, ?)
ON CONFLICT (name) DO UPDATE SET expires_at = excluded.expires_at
WHERE lease.holder = excluded.holder;

-- name: ReleaseLease :exec
DELETE FROM lease WHERE name = ? AND holder = ?;
