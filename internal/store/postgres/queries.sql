-- Postgres twin of sqlite/queries.sql (story #85, ADR 0028). The queries keep
-- the SQLite names and semantics; the differences are Postgres syntax ($n,
-- greatest, NULLS FIRST), booleans for the 0/1 flags, and lists passed as
-- JSON text (json_array_elements_text) instead of sqlc.slice (ADR 0028 §5).

-- Advisory locks (ADR 0028 §4): the first argument is the schema key, the
-- second the kind (1 config, 2 event, 3 lease).
-- name: AdvisoryXactLock :exec
SELECT pg_advisory_xact_lock(sqlc.arg(schema_key)::integer, sqlc.arg(kind)::integer);

-- name: CreateTarget :exec
INSERT INTO target (id, kind, coordinates_json, fetch_state_json, last_checked_at, next_check_at, error_count, interval_ms, source_kind)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: GetTargetByCoordinates :one
SELECT * FROM target WHERE kind = $1 AND coordinates_json = $2;

-- name: SetTargetInterval :execrows
UPDATE target SET interval_ms = $1, next_check_at = $2 WHERE id = $3;

-- name: DeleteTargetVersions :exec
DELETE FROM version WHERE target_id = $1;

-- name: DeleteTargetIfUnused :execrows
DELETE FROM target WHERE target.id = $1 AND NOT EXISTS (SELECT 1 FROM watch w WHERE w.target_id = target.id);

-- name: GetTarget :one
SELECT * FROM target WHERE id = $1;

-- name: UpdateTargetState :execrows
UPDATE target
SET fetch_state_json = $1, last_checked_at = $2, next_check_at = $3, error_count = $4
WHERE id = $5;

-- name: CreateWatch :exec
INSERT INTO watch (id, target_id, policy_json, labels_json, baseline_done, name, managed_by, interval_ms)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: UpdateWatchConfig :execrows
UPDATE watch SET target_id = $1, policy_json = $2, labels_json = $3, interval_ms = $4 WHERE id = $5;

-- name: ResetWatchBaseline :execrows
UPDATE watch SET baseline_done = false WHERE id = $1;

-- NULLS FIRST: SQLite sorts NULL names first, Postgres last by default.
-- name: ListWatchesManagedBy :many
SELECT * FROM watch WHERE managed_by = $1 ORDER BY name NULLS FIRST;

-- name: GetWatch :one
SELECT * FROM watch WHERE id = $1;

-- A check reads its watches FOR SHARE (ADR 0028 §2): a config transaction
-- that updates or deletes one of them waits until the check commits. Config
-- transactions must update a watch before deleting its held versions.
-- name: ListWatchesByTarget :many
SELECT * FROM watch WHERE target_id = $1 ORDER BY id FOR SHARE;

-- name: MarkBaselineDone :execrows
UPDATE watch SET baseline_done = true WHERE id = $1;

-- name: DeleteWatch :execrows
DELETE FROM watch WHERE id = $1;

-- name: AddVersion :execrows
INSERT INTO version (target_id, raw, normalized, published_at, prerelease, yanked, first_seen_at, removed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (target_id, raw) DO NOTHING;

-- name: UpdateVersionState :execrows
UPDATE version SET yanked = $1, removed_at = $2 WHERE target_id = $3 AND raw = $4;

-- name: ListVersions :many
SELECT * FROM version WHERE target_id = $1 ORDER BY first_seen_at, raw;

-- name: CreateSink :exec
INSERT INTO sink (id, kind, config_json, name, managed_by) VALUES ($1, $2, $3, $4, $5);

-- name: UpdateSink :execrows
UPDATE sink SET kind = $1, config_json = $2 WHERE id = $3;

-- name: ListSinksManagedBy :many
SELECT * FROM sink WHERE managed_by = $1 ORDER BY name NULLS FIRST;

-- name: DeleteSink :execrows
DELETE FROM sink WHERE id = $1;

-- name: CreateSubscription :exec
INSERT INTO subscription (id, filter_json, sink_id, template, name, managed_by, policy_json) VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: UpdateSubscription :execrows
UPDATE subscription SET filter_json = $1, sink_id = $2, template = $3, policy_json = $4 WHERE id = $5;

-- name: ListSubscriptionsManagedBy :many
SELECT * FROM subscription WHERE managed_by = $1 ORDER BY name NULLS FIRST;

-- name: ListSubscriptions :many
SELECT * FROM subscription ORDER BY id;

-- name: FailPendingDeliveries :execrows
UPDATE delivery SET status = 'dead', last_error = $1, next_attempt_at = NULL
WHERE subscription_id = $2 AND status = 'pending';

-- A held delivery is pending and held_until is still ahead.
-- name: ListHeldDeliveries :many
SELECT * FROM delivery
WHERE subscription_id = sqlc.arg(subscription_id) AND status = 'pending' AND held_until > sqlc.arg(now)::bigint
ORDER BY id;

-- The held queries only touch a delivery that is still held at now.
-- name: ReleaseHeldDelivery :execrows
UPDATE delivery SET held_until = NULL, next_attempt_at = sqlc.narg(now)::bigint
WHERE id = sqlc.arg(id) AND status = 'pending' AND held_until > sqlc.narg(now)::bigint;

-- name: RescheduleHeldDelivery :execrows
UPDATE delivery SET held_until = sqlc.narg(until)::bigint, next_attempt_at = sqlc.narg(until)::bigint
WHERE id = sqlc.arg(id) AND status = 'pending' AND held_until > sqlc.narg(now)::bigint;

-- name: FailHeldDelivery :execrows
UPDATE delivery SET status = 'dead', last_error = sqlc.arg(reason)::text, next_attempt_at = NULL, held_until = NULL
WHERE id = sqlc.arg(id) AND status = 'pending' AND held_until > sqlc.narg(now)::bigint;

-- A yank cancels the deliveries of the original event that are still held
-- (ADR 0014 section 4, ADR 0018).
-- name: DeleteHeldDeliveriesOfEvent :execrows
DELETE FROM delivery
WHERE event_id = sqlc.arg(event_id) AND status = 'pending' AND held_until > sqlc.narg(now)::bigint;

-- name: DeleteSubscription :execrows
DELETE FROM subscription WHERE id = $1;

-- name: CreateEvent :exec
INSERT INTO event (id, watch_id, type, payload_json, created_at, ref_event_id) VALUES ($1, $2, $3, $4, $5, $6);

-- The expression, collation included, is the one of the event_watch_subject
-- index (ADR 0018, ADR 0028 §7), so the lookup walks it.
-- name: GetLatestEventBySubject :one
SELECT * FROM event
WHERE watch_id = sqlc.arg(watch_id)
  AND ((payload_json::json) ->> 'subject') COLLATE "C" = sqlc.arg(subject)::text
  AND type IN (SELECT json_array_elements_text(sqlc.arg(types_json)::json))
ORDER BY seq DESC LIMIT 1;

-- name: ListEventsAfter :many
SELECT * FROM event WHERE seq > $1 ORDER BY seq LIMIT $2;

-- All deliveries of an event in one statement (ADR 0028 §1: the event lock is
-- held for one round trip per event, whatever the number of subscriptions).
-- deliveries_json is an array of objects keyed by column name.
-- name: CreateDeliveries :exec
INSERT INTO delivery (id, event_id, subscription_id, status, attempts, next_attempt_at, last_error,
                      held_until, hold_started_at, hold_watch_cooldown_ms, hold_prerelease, hold_watch_id, hold_raw, hold_input,
                      traceparent)
SELECT d.id, d.event_id, d.subscription_id, d.status, d.attempts, d.next_attempt_at, d.last_error,
       d.held_until, d.hold_started_at, d.hold_watch_cooldown_ms, d.hold_prerelease, d.hold_watch_id, d.hold_raw, d.hold_input,
       d.traceparent
FROM json_populate_recordset(NULL::delivery, sqlc.arg(deliveries_json)::json) AS d;

-- name: ListDeliveries :many
SELECT * FROM delivery WHERE event_id = $1 ORDER BY id;

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

-- Kept current by the event_prune_seq trigger.
-- name: HighestPrunedSeq :one
SELECT highest_pruned_seq FROM event_prune WHERE id = 1;

-- name: DeleteExpiredLease :exec
DELETE FROM lease WHERE name = $1 AND expires_at <= $2;

-- name: AcquireLease :execrows
INSERT INTO lease (name, holder, expires_at) VALUES ($1, $2, $3)
ON CONFLICT (name) DO UPDATE SET expires_at = excluded.expires_at
WHERE lease.holder = excluded.holder;

-- name: ReleaseLease :exec
DELETE FROM lease WHERE name = $1 AND holder = $2;

-- Due targets that no unexpired lease covers ('target:' || id, see
-- store.TargetLeaseName), oldest due first. #86 adds FOR UPDATE SKIP LOCKED;
-- until then the lease lock serializes lease transactions (ADR 0028 §3).
-- name: ListLeasableDueTargets :many
SELECT id FROM target
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
SELECT EXISTS (SELECT 1 FROM target WHERE id = $1);

-- name: DeleteExpiredTargetLeases :execrows
DELETE FROM lease WHERE name LIKE 'target:%' AND expires_at <= $1;

-- name: AddPendingRelease :execrows
INSERT INTO pending_release (watch_id, target_id, raw, held_at, release_json)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (watch_id, target_id, raw) DO NOTHING;

-- name: ListPendingReleases :many
-- Rows of watches that moved to another target are left out.
SELECT p.watch_id, p.target_id, p.raw, p.held_at, p.release_json, v.yanked, v.removed_at
FROM pending_release p
JOIN watch w ON w.id = p.watch_id AND w.target_id = p.target_id
JOIN version v ON v.target_id = p.target_id AND v.raw = p.raw
WHERE p.target_id = $1
ORDER BY p.watch_id, p.held_at, p.raw;

-- name: DeletePendingRelease :execrows
DELETE FROM pending_release WHERE watch_id = $1 AND target_id = $2 AND raw = $3;

-- name: DeletePendingReleasesOfWatch :execrows
DELETE FROM pending_release WHERE watch_id = $1;

-- Dispatcher (story #107, ADR 0017). The same three disjoint branches as on
-- SQLite (story #232), each read in order from
-- delivery_status_next_attempt_at_id, which is built NULLS FIRST.
-- name: ListLeasableDueDeliveries :many
SELECT u.id FROM (
  (SELECT d.id, d.next_attempt_at FROM delivery d
   WHERE d.status = 'pending' AND d.next_attempt_at IS NULL
     AND NOT EXISTS (
       SELECT 1 FROM lease
       WHERE lease.name = 'delivery:' || d.id AND lease.expires_at > sqlc.arg(now)::bigint
     )
   ORDER BY d.next_attempt_at NULLS FIRST, d.id
   LIMIT sqlc.arg(max_rows))
  UNION ALL
  (SELECT d.id, d.next_attempt_at FROM delivery d
   WHERE d.status = 'pending' AND d.next_attempt_at <= sqlc.arg(now)::bigint
     AND NOT EXISTS (
       SELECT 1 FROM lease
       WHERE lease.name = 'delivery:' || d.id AND lease.expires_at > sqlc.arg(now)::bigint
     )
   ORDER BY d.next_attempt_at NULLS FIRST, d.id
   LIMIT sqlc.arg(max_rows))
  UNION ALL
  (SELECT d.id, d.next_attempt_at FROM delivery d
   WHERE d.status = 'in_flight'
     AND NOT EXISTS (
       SELECT 1 FROM lease
       WHERE lease.name = 'delivery:' || d.id AND lease.expires_at > sqlc.arg(now)::bigint
     )
   ORDER BY d.next_attempt_at NULLS FIRST, d.id
   LIMIT sqlc.arg(max_rows))
) AS u
ORDER BY u.next_attempt_at NULLS FIRST, u.id
LIMIT sqlc.arg(max_rows);

-- name: HasLeasableDueDeliveries :one
SELECT EXISTS (
  SELECT 1 FROM delivery d
  WHERE ((d.status = 'pending' AND d.next_attempt_at <= sqlc.arg(now)::bigint)
         OR (d.status = 'pending' AND d.next_attempt_at IS NULL)
         OR d.status = 'in_flight')
    AND NOT EXISTS (
      SELECT 1 FROM lease
      WHERE lease.name = 'delivery:' || d.id AND lease.expires_at > sqlc.arg(now)::bigint
    )
);

-- Every lease of a delivery counts as an attempt, so an attempt cut short by
-- a crash counts too. Unlike SQLite's, the update repeats the due predicate
-- of ListLeasableDueDeliveries (ADR 0028 §3): writers that take no lease lock
-- (FailPendingDeliveries, FailHeldDelivery, CancelHeldDeliveries) may have
-- changed or deleted the row since it was selected; then no row comes back
-- and the caller skips it. The holder's own new lease does not count.
-- name: StartDeliveryAttempt :one
UPDATE delivery SET status = 'in_flight', attempts = attempts + 1
WHERE delivery.id = sqlc.arg(id)
  AND ((delivery.status = 'pending' AND (delivery.next_attempt_at IS NULL OR delivery.next_attempt_at <= sqlc.arg(now)::bigint))
       OR delivery.status = 'in_flight')
  AND NOT EXISTS (
    SELECT 1 FROM lease
    WHERE lease.name = 'delivery:' || delivery.id AND lease.expires_at > sqlc.arg(now)::bigint
      AND lease.holder <> sqlc.arg(holder)::text
  )
RETURNING *;

-- name: GetEvent :one
SELECT * FROM event WHERE id = $1;

-- name: GetSubscription :one
SELECT * FROM subscription WHERE id = $1;

-- name: GetSink :one
SELECT * FROM sink WHERE id = $1;

-- The end of an attempt, fenced by its lease: it applies only while holder
-- still owns the lease row, expired or not, and the row is still at this
-- attempt.
-- name: FinishLeasedDelivery :execrows
UPDATE delivery
SET status = sqlc.arg(status), next_attempt_at = sqlc.arg(next_attempt_at), last_error = sqlc.arg(last_error)
WHERE id = sqlc.arg(id) AND status = 'in_flight' AND attempts = sqlc.arg(attempt)
  AND EXISTS (
    SELECT 1 FROM lease WHERE lease.name = sqlc.arg(lease_name) AND lease.holder = sqlc.arg(holder)
  );

-- name: CountDeliveriesByStatus :many
SELECT status, count(*) AS n FROM delivery GROUP BY status;

-- name: SubscriptionGeneration :one
SELECT generation FROM subscription_generation WHERE id = 1;

-- REST API (story #71).

-- name: ListNamedWatchesPage :many
SELECT * FROM watch
WHERE name IS NOT NULL
  AND (name > sqlc.arg(after_name)::text OR (name = sqlc.arg(after_name)::text AND id > sqlc.arg(after_id)::text))
ORDER BY name, id
LIMIT sqlc.arg(max_rows);

-- name: GetWatchByName :one
SELECT * FROM watch WHERE name = $1;

-- name: ListNamedSinksPage :many
SELECT * FROM sink
WHERE name IS NOT NULL
  AND (name > sqlc.arg(after_name)::text OR (name = sqlc.arg(after_name)::text AND id > sqlc.arg(after_id)::text))
ORDER BY name, id
LIMIT sqlc.arg(max_rows);

-- name: GetSinkByName :one
SELECT * FROM sink WHERE name = $1;

-- name: ListNamedSubscriptionsPage :many
SELECT * FROM subscription
WHERE name IS NOT NULL
  AND (name > sqlc.arg(after_name)::text OR (name = sqlc.arg(after_name)::text AND id > sqlc.arg(after_id)::text))
ORDER BY name, id
LIMIT sqlc.arg(max_rows);

-- name: GetSubscriptionByName :one
SELECT * FROM subscription WHERE name = $1;

-- name: ListSubscriptionsBySink :many
SELECT * FROM subscription WHERE sink_id = $1 ORDER BY name NULLS FIRST, id;

-- name: GetDelivery :one
SELECT * FROM delivery WHERE id = $1;

-- name: HasEventsAfter :one
SELECT EXISTS (SELECT 1 FROM event WHERE seq > $1);

-- Newest event first, then by delivery id. A subscription_ids_json of 'null'
-- keeps every subscription.
-- name: ListDeliveriesPage :many
SELECT sqlc.embed(d), e.seq AS event_seq
FROM delivery d JOIN event e ON e.id = d.event_id
WHERE d.status IN (SELECT json_array_elements_text(sqlc.arg(statuses_json)::json))
  AND (sqlc.arg(subscription_ids_json)::text = 'null'
       OR d.subscription_id IN (SELECT json_array_elements_text(sqlc.arg(subscription_ids_json)::json)))
  AND (sqlc.arg(event_id)::text = '' OR d.event_id = sqlc.arg(event_id)::text)
  AND (e.seq < sqlc.arg(before_seq)::bigint OR (e.seq = sqlc.arg(before_seq)::bigint AND d.id > sqlc.arg(before_id)::text))
ORDER BY e.seq DESC, d.id
LIMIT sqlc.arg(max_rows);

-- name: ListReplayCandidates :many
SELECT sqlc.embed(d), e.seq AS event_seq, e.payload_json
FROM delivery d JOIN event e ON e.id = d.event_id
WHERE d.status IN (SELECT json_array_elements_text(sqlc.arg(statuses_json)::json))
  AND (sqlc.arg(event_id)::text = '' OR d.event_id = sqlc.arg(event_id)::text)
  AND (sqlc.arg(subscription_id)::text = '' OR d.subscription_id = sqlc.arg(subscription_id)::text)
ORDER BY e.seq, d.id;

-- name: ReplayDelivery :execrows
UPDATE delivery SET status = 'pending', next_attempt_at = sqlc.narg(now)::bigint, held_until = NULL,
    replay_base_attempts = attempts
WHERE id = sqlc.arg(id) AND status IN ('dead', 'delivered');

-- Stored secrets (story #75, ADR 0024 section 6).

-- name: CreateSecret :exec
INSERT INTO secret (id, name, hosts_json, key_id, ciphertext, revision, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, 1, $6, $7);

-- Fenced by the row's revision (ADR 0024 section 5). Under READ COMMITTED a
-- concurrent update of the row makes Postgres re-check the WHERE on the newest
-- version, so one of two writers at the same revision wins.
-- name: UpdateSecret :execrows
UPDATE secret SET hosts_json = $1, key_id = $2, ciphertext = $3, updated_at = $4, revision = revision + 1
WHERE id = $5 AND revision = $6;

-- name: DeleteSecret :execrows
DELETE FROM secret WHERE id = $1;

-- name: GetSecretByName :one
SELECT * FROM secret WHERE name = $1;

-- name: GetSecret :one
SELECT * FROM secret WHERE id = $1;

-- name: ListSecrets :many
SELECT * FROM secret ORDER BY name;

-- name: CountSecretsByKeyID :many
SELECT key_id, COUNT(*) AS n FROM secret GROUP BY key_id ORDER BY key_id;

-- Named consumers (story #79-B).

-- name: CreateConsumer :exec
INSERT INTO consumer (id, name, filter_json, acked_seq, acked_at, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: UpdateConsumerFilter :execrows
UPDATE consumer SET filter_json = $1, updated_at = $2 WHERE id = $3;

-- name: DeleteConsumer :execrows
DELETE FROM consumer WHERE id = $1;

-- name: GetConsumerByName :one
SELECT * FROM consumer WHERE name = $1;

-- name: ListConsumersPage :many
SELECT * FROM consumer
WHERE name > sqlc.arg(after_name)::text OR (name = sqlc.arg(after_name)::text AND id > sqlc.arg(after_id)::text)
ORDER BY name, id
LIMIT sqlc.arg(max_rows);

-- One statement, so concurrent acks never move the cursor back: the UPDATE
-- re-reads the newest row version under READ COMMITTED.
-- name: AckConsumer :one
UPDATE consumer SET
    acked_at   = CASE WHEN sqlc.arg(seq)::bigint > acked_seq THEN sqlc.arg(now)::bigint ELSE acked_at END,
    acked_seq  = greatest(acked_seq, sqlc.arg(seq)::bigint)
WHERE name = sqlc.arg(name)
RETURNING *;

-- name: MaxEventSeq :one
SELECT greatest(coalesce((SELECT max(seq) FROM event), 0),
                coalesce((SELECT highest_pruned_seq FROM event_prune WHERE id = 1), 0))::bigint AS seq;
