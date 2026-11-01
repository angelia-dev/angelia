-- name: CreateTarget :exec
INSERT INTO target (id, kind, coordinates_json, fetch_state_json, last_checked_at, next_check_at, error_count, interval_ms)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetTargetByCoordinates :one
SELECT * FROM target WHERE kind = ? AND coordinates_json = ?;

-- name: SetTargetInterval :execrows
UPDATE target SET interval_ms = ?, next_check_at = ? WHERE id = ?;

-- name: DeleteTargetVersions :exec
DELETE FROM version WHERE target_id = ?;

-- name: DeleteTargetIfUnused :execrows
DELETE FROM target WHERE target.id = ? AND NOT EXISTS (SELECT 1 FROM watch w WHERE w.target_id = target.id);

-- name: GetTarget :one
SELECT * FROM target WHERE id = ?;

-- name: UpdateTargetState :execrows
UPDATE target
SET fetch_state_json = ?, last_checked_at = ?, next_check_at = ?, error_count = ?
WHERE id = ?;

-- name: CreateWatch :exec
INSERT INTO watch (id, target_id, policy_json, labels_json, baseline_done, name, managed_by)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: UpdateWatchConfig :execrows
UPDATE watch SET target_id = ?, policy_json = ?, labels_json = ? WHERE id = ?;

-- name: ResetWatchBaseline :execrows
UPDATE watch SET baseline_done = 0 WHERE id = ?;

-- name: ListWatchesManagedBy :many
SELECT * FROM watch WHERE managed_by = ? ORDER BY name;

-- name: GetWatch :one
SELECT * FROM watch WHERE id = ?;

-- name: ListWatchesByTarget :many
SELECT * FROM watch WHERE target_id = ? ORDER BY id;

-- name: MarkBaselineDone :execrows
UPDATE watch SET baseline_done = 1 WHERE id = ?;

-- name: DeleteWatch :execrows
DELETE FROM watch WHERE id = ?;

-- name: AddVersion :execrows
INSERT INTO version (target_id, raw, normalized, published_at, prerelease, yanked, first_seen_at, removed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (target_id, raw) DO NOTHING;

-- name: UpdateVersionState :execrows
UPDATE version SET yanked = ?, removed_at = ? WHERE target_id = ? AND raw = ?;

-- name: ListVersions :many
SELECT * FROM version WHERE target_id = ? ORDER BY first_seen_at, raw;

-- name: CreateSink :exec
INSERT INTO sink (id, kind, config_json_encrypted, name, managed_by) VALUES (?, ?, ?, ?, ?);

-- name: UpdateSink :execrows
UPDATE sink SET kind = ?, config_json_encrypted = ? WHERE id = ?;

-- name: ListSinksManagedBy :many
SELECT * FROM sink WHERE managed_by = ? ORDER BY name;

-- name: DeleteSink :execrows
DELETE FROM sink WHERE id = ?;

-- name: CreateSubscription :exec
INSERT INTO subscription (id, filter_json, sink_id, template, name, managed_by, policy_json) VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: UpdateSubscription :execrows
UPDATE subscription SET filter_json = ?, sink_id = ?, template = ?, policy_json = ? WHERE id = ?;

-- name: ListSubscriptionsManagedBy :many
SELECT * FROM subscription WHERE managed_by = ? ORDER BY name;

-- name: ListSubscriptions :many
SELECT * FROM subscription ORDER BY id;

-- name: FailPendingDeliveries :execrows
UPDATE delivery SET status = 'dead', last_error = ?, next_attempt_at = NULL
WHERE subscription_id = ? AND status = 'pending';

-- A held delivery is pending and held_until is still ahead.
-- name: ListHeldDeliveries :many
SELECT * FROM delivery
WHERE subscription_id = ? AND status = 'pending' AND held_until > ?
ORDER BY id;

-- The held queries only touch a delivery that is still held at now.
-- name: ReleaseHeldDelivery :execrows
UPDATE delivery SET held_until = NULL, next_attempt_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND status = 'pending' AND held_until > sqlc.arg(now);

-- name: RescheduleHeldDelivery :execrows
UPDATE delivery SET held_until = sqlc.arg(until), next_attempt_at = sqlc.arg(until)
WHERE id = sqlc.arg(id) AND status = 'pending' AND held_until > sqlc.arg(now);

-- name: FailHeldDelivery :execrows
UPDATE delivery SET status = 'dead', last_error = sqlc.arg(reason), next_attempt_at = NULL, held_until = NULL
WHERE id = sqlc.arg(id) AND status = 'pending' AND held_until > sqlc.arg(now);

-- name: DeleteSubscription :execrows
DELETE FROM subscription WHERE id = ?;

-- name: CreateEvent :exec
INSERT INTO event (id, watch_id, type, payload_json, created_at) VALUES (?, ?, ?, ?, ?);

-- name: ListEventsAfter :many
SELECT * FROM event WHERE seq > ? ORDER BY seq LIMIT ?;

-- name: CreateDelivery :exec
INSERT INTO delivery (id, event_id, subscription_id, status, attempts, next_attempt_at, last_error,
                      held_until, hold_started_at, hold_watch_cooldown_ms, hold_prerelease, hold_watch_id, hold_raw, hold_input)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListDeliveries :many
SELECT * FROM delivery WHERE event_id = ? ORDER BY id;

-- name: PruneBatchEnd :many
-- There is no index on event.created_at, so this scans by seq from after_seq
-- and stops at batch matches. That is fine for a periodic job, not for a hot path.
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

-- Due targets that no unexpired lease covers ('target:' || id, see
-- store.TargetLeaseName), oldest due first. Postgres will use
-- FOR UPDATE SKIP LOCKED here instead.
-- name: ListLeasableDueTargets :many
SELECT * FROM target
WHERE next_check_at <= sqlc.arg(now)
  AND NOT EXISTS (
    SELECT 1 FROM lease
    WHERE lease.name = 'target:' || target.id AND lease.expires_at > sqlc.arg(now)
  )
ORDER BY next_check_at, id
LIMIT sqlc.arg(max_rows);

-- name: CountLeasableDueTargets :one
SELECT count(*) FROM target
WHERE next_check_at <= sqlc.arg(now)
  AND NOT EXISTS (
    SELECT 1 FROM lease
    WHERE lease.name = 'target:' || target.id AND lease.expires_at > sqlc.arg(now)
  );

-- name: HasLeasableDueTargets :one
SELECT EXISTS (
  SELECT 1 FROM target
  WHERE next_check_at <= sqlc.arg(now)
    AND NOT EXISTS (
      SELECT 1 FROM lease
      WHERE lease.name = 'target:' || target.id AND lease.expires_at > sqlc.arg(now)
    )
);

-- The target write of a check, fenced by its lease: it only applies while
-- holder still owns an unexpired lease named lease_name.
-- name: UpdateTargetStateLeased :execrows
UPDATE target
SET fetch_state_json = sqlc.arg(fetch_state_json), last_checked_at = sqlc.arg(last_checked_at),
    next_check_at = sqlc.arg(next_check_at), error_count = sqlc.arg(error_count)
WHERE id = sqlc.arg(id)
  AND EXISTS (
    SELECT 1 FROM lease
    WHERE lease.name = sqlc.arg(lease_name) AND lease.holder = sqlc.arg(holder)
      AND lease.expires_at > sqlc.arg(now)
  );

-- name: TargetExists :one
SELECT EXISTS (SELECT 1 FROM target WHERE id = ?);

-- name: DeleteExpiredTargetLeases :execrows
DELETE FROM lease WHERE name LIKE 'target:%' AND expires_at <= ?;

-- name: AddPendingRelease :execrows
INSERT INTO pending_release (watch_id, target_id, raw, held_at, release_json)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (watch_id, target_id, raw) DO NOTHING;

-- name: ListPendingReleases :many
-- Rows of watches that moved to another target are left out.
SELECT p.watch_id, p.target_id, p.raw, p.held_at, p.release_json, v.yanked, v.removed_at
FROM pending_release p
JOIN watch w ON w.id = p.watch_id AND w.target_id = p.target_id
JOIN version v ON v.target_id = p.target_id AND v.raw = p.raw
WHERE p.target_id = ?
ORDER BY p.watch_id, p.held_at, p.raw;

-- name: DeletePendingRelease :execrows
DELETE FROM pending_release WHERE watch_id = ? AND target_id = ? AND raw = ?;

-- name: DeletePendingReleasesOfWatch :execrows
DELETE FROM pending_release WHERE watch_id = ?;

-- Dispatcher (story #107, ADR 0017). A delivery is due when it is pending
-- and its next_attempt_at has passed (NULL counts as due), or when it is in
-- flight under no unexpired lease: its holder crashed or stalled, so it is
-- sent again (at-least-once). Oldest due first. Postgres will use
-- FOR UPDATE SKIP LOCKED here instead. The three OR terms are written apart
-- so that SQLite can use delivery_status_next_attempt_at for each.
-- name: ListLeasableDueDeliveries :many
SELECT d.id FROM delivery d
WHERE ((d.status = 'pending' AND d.next_attempt_at <= sqlc.arg(now))
       OR (d.status = 'pending' AND d.next_attempt_at IS NULL)
       OR d.status = 'in_flight')
  AND NOT EXISTS (
    SELECT 1 FROM lease
    WHERE lease.name = 'delivery:' || d.id AND lease.expires_at > sqlc.arg(now)
  )
ORDER BY d.next_attempt_at, d.id
LIMIT sqlc.arg(max_rows);

-- name: HasLeasableDueDeliveries :one
SELECT EXISTS (
  SELECT 1 FROM delivery d
  WHERE ((d.status = 'pending' AND d.next_attempt_at <= sqlc.arg(now))
         OR (d.status = 'pending' AND d.next_attempt_at IS NULL)
         OR d.status = 'in_flight')
    AND NOT EXISTS (
      SELECT 1 FROM lease
      WHERE lease.name = 'delivery:' || d.id AND lease.expires_at > sqlc.arg(now)
    )
);

-- Every lease of a delivery counts as an attempt, so an attempt cut short
-- by a crash counts too.
-- name: StartDeliveryAttempt :one
UPDATE delivery SET status = 'in_flight', attempts = attempts + 1
WHERE id = ?
RETURNING *;

-- name: GetEvent :one
SELECT * FROM event WHERE id = ?;

-- name: GetSubscription :one
SELECT * FROM subscription WHERE id = ?;

-- name: GetSink :one
SELECT * FROM sink WHERE id = ?;

-- The end of an attempt, fenced by its lease: it applies only while holder
-- still owns the lease row, expired or not, and the row is still at this
-- attempt. Leasing replaces the row and counts a new attempt, so a holder
-- whose delivery was taken over (by another holder or by itself, after its
-- lease expired) writes nothing.
-- name: FinishLeasedDelivery :execrows
UPDATE delivery
SET status = sqlc.arg(status), next_attempt_at = sqlc.arg(next_attempt_at), last_error = sqlc.arg(last_error)
WHERE id = sqlc.arg(id) AND status = 'in_flight' AND attempts = sqlc.arg(attempt)
  AND EXISTS (
    SELECT 1 FROM lease WHERE lease.name = sqlc.arg(lease_name) AND lease.holder = sqlc.arg(holder)
  );
