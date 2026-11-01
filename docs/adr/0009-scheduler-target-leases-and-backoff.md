# ADR 0009: The scheduler leases targets through the generic lease table and backs off exponentially

- **Status:** Proposed
- **Date:** 2026-11-01
- **Story:** angelia-dev/angelia#47

## Context

[architecture.md §3.2](../architecture.md#32-scheduler-and-workers) asks for a continuous scheduler:
each target has `next_check_at`, workers lease due work from the database, jitter spreads the load,
errors back off, and one fetch serves every watch on a target. [ADR 0003](0003-sqlite-default-postgres-ha-no-external-queue.md)
rules out an external queue: SQLite by default, `FOR UPDATE SKIP LOCKED` on Postgres later. The
schema of #46 ([ADR 0008](0008-store-history-rows-and-event-cursor.md)) already has a generic
`lease(name, holder, expires_at)` table and `target.next_check_at`, `error_count` and
`fetch_state_json`. The diff engine (#56) and the sources (#51–53) are built in parallel and must
plug in later.

## Decision

1. **The unit of scheduling is the target.** Watches are never scheduled, so N watches on
   `pypi:requests` cost one fetch per interval. The unique index on `(kind, coordinates_json)`
   keeps one target per upstream object.
2. **One lease per running check**, named `target:<id>` (`store.TargetLeaseName`) in the existing
   lease table; no schema change. `store.Store.LeaseDueTargets(holder, now, until, limit)` selects
   due targets that no unexpired lease covers and inserts their leases in **one transaction**
   (SQLite: an immediate transaction, so the single write lock serializes callers; Postgres:
   `SELECT … FOR UPDATE SKIP LOCKED` plus the same inserts). `CountDueTargets` feeds the backlog
   metric; `HasDueTargets`, a plain read, runs first so that idle polls (every 1 s) do not take
   SQLite's write lock. A running check renews its lease every TTL/3 (default TTL 5 min) and is
   cancelled if a renewal finds the lease taken, or if renewals keep failing until the last
   renewed expiry has passed. A check is also bounded by `CheckTimeout` (default 3 min, must be
   below the TTL); a timeout counts as an error. A crashed holder's leases expire after the TTL;
   the scheduler deletes expired `target:` lease rows once an hour.
   On Postgres, a per-row conflict while leasing (a lease row that appeared between the select
   and the insert) must skip that row, not abort the whole batch.
   **Clock skew:** leases compare expiry times written and read by different hosts. With renewals
   every TTL/3, a holder's lease is never closer than TTL·2/3 to expiry, so the tolerated skew
   between hosts is about TTL·2/3 (≈ 3 min 20 s by default). On Postgres, `now` should be computed
   in the database (`now()`) instead of on each host, which removes host skew altogether.
   **Fencing:** the scheduler's final target write (`UpdateTargetLeased`) is conditional on still
   holding an unexpired lease, in the same statement; if another holder took the target over, the
   write is dropped and the new holder's schedule wins.
3. **Holder ids are per scheduler instance** (host name + random suffix), so several processes, or
   several schedulers in one process, can share a database.
4. **Next check time.** Success: `now + interval·(1 ± jitter)`, interval 1h and jitter 10% by
   default, error count reset to 0, fetch state replaced. Error: `now + min(base·2^(n−1), 24h)·(1 ± jitter)`
   with base 1 min and n the consecutive error count; the cap applies before jitter so targets at
   the cap do not all land on exactly 24h. The fetch state and `last_checked_at` are kept. Every
   interval is clamped to 30 days so jitter arithmetic cannot overflow.
   A check cut short by shutdown or a lost lease changes nothing and leaves the target due.
5. **Seam to the detect pipeline:** `scheduler.Checker.Check(ctx, store.Target) (Outcome, error)`.
   The checker fetches through the source registry, commits versions, events and deliveries, and
   returns the new fetch state (and optionally an interval override, e.g. the shortest watch
   interval). The scheduler then writes `fetch_state_json`, `next_check_at`, `error_count` and
   `last_checked_at`. Versions are committed before the fetch state, so a crash in between costs
   one unconditional fetch, never a missed version.
   **The checker's commit must be idempotent:** it inserts versions with `ON CONFLICT DO NOTHING`,
   emits events (and their deliveries) only for the rows it actually inserted, and does both in one
   transaction. A time-based lease cannot fully exclude two holders checking the same target (a
   stalled process that resumes after its lease expired, a clock jump), and a crash after the
   commit repeats the check. Leases make a duplicate check rare; only this idempotent commit makes
   it harmless. "Never a duplicate event" holds **because of** the idempotent checker, not because
   of the lease.
6. **No per-host throttling in the scheduler.** Sources fetch through the netguard client, whose
   per-host limiter paces requests; the scheduler only bounds concurrency (worker pool).

## Consequences

### Positive

- Works on SQLite today and maps one-to-one onto `SKIP LOCKED` on Postgres.
- The scheduler does not import sources or the diff engine; they plug in through `Checker`.
- Deterministic tests: clock and random source are injected.

### Negative

- Two transactions per check (the checker's, then the target update). Merging them would need the
  checker to write the schedule, which couples it to scheduling.
- Every poll runs one read (plus a count when metrics are on); the write transaction runs only
  when a target is due.
- Lease rows of crashed holders stay up to an hour before the periodic cleanup deletes them.

### Open questions

- Whether `store.Tx` should also carry a lease fence, so the checker's own commit is rejected
  after a takeover, is left to the maintainer; the idempotent commit above makes it unnecessary
  for correctness.
- The worker pool size, the default interval and the check timeout are `scheduler.Options` fields; exposing them in
  the instance configuration (`angelia.yaml`) changes a public contract and is left to the
  maintainer (#47 review).
- Wiring the role into `angelia serve` waits for the store to be opened there and for a `Checker`
  (#56).

## Alternatives considered

- **A dedicated `lease_until`/`leased_by` column on `target`.** Simpler queries, but needs a
  migration and duplicates the generic lease table that ADR 0003 and #46 already provide.
- **River on Postgres.** Not available on SQLite, which is the default.
- **Backoff from the interval (interval·2^n).** A 1h interval would wait 2h after one transient
  error, which hurts the freshness target (PRD: publish to event < interval + 2 min).

## References

- [architecture.md §3.2](../architecture.md#32-scheduler-and-workers), [§5](../architecture.md#5-data-model-simplified)
- [ADR 0003](0003-sqlite-default-postgres-ha-no-external-queue.md), [ADR 0008](0008-store-history-rows-and-event-cursor.md)
- Story angelia-dev/angelia#47
