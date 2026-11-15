# ADR 0031: On Postgres, lease transactions lock their rows with `FOR UPDATE SKIP LOCKED` instead of an advisory lock; lease times stay host times, checked against the database clock at start

- **Status:** Accepted (D21 and D22 accepted by the maintainer on 2026-11-10); the implementation details below are reviewed in the PR
- **Date:** 2026-11-15
- **Story:** angelia-dev/angelia#86 (sub-story 86-A)

> D21 and D22 are the decisions of #86. The maintainer accepted both recommendations on 2026-11-10, and
> [ADR 0028](0028-postgres-event-cursor-and-write-concurrency.md) §3 and §9 first recorded them, with the design
> they bound. This ADR records them again as the decisions of #86 and describes how 86-A implements them:
> the queries, the lock order of the fenced writes and the start-up clock check.

## Context

Epic #18 requires that two replicas each of `scheduler` and `dispatcher` run against one Postgres database
with no target checked by two holders at once and no lost delivery. [architecture.md §3.2](../architecture.md#32-scheduler-and-workers)
names `SELECT … FOR UPDATE SKIP LOCKED` for Postgres, and §6 offered "River, or a small in-house lease
table usable on both" engines; [ADR 0003](0003-sqlite-default-postgres-ha-no-external-queue.md) left that
choice open.

The Postgres engine of #85 serialized every lease transaction behind one advisory lock (ADR 0028 §3, lock
kind 3). That kept SQLite's semantics, but only one process could lease at a time. [ADR 0009](0009-scheduler-target-leases-and-backoff.md)
§2 asks that "a per-row conflict while leasing … must skip that row, not abort the whole batch", and that
`now` "should be computed in the database". [ADR 0017](0017-dispatcher-delivery-leases-and-retries.md) §2
fences a delivery's finish by holder and attempt and needs host clocks within at least 30 s of each other.

## Decision

### D21: in-house leasing with `FOR UPDATE SKIP LOCKED` (maintainer, 2026-11-10)

Leasing stays in-house: the existing `lease(name, holder, expires_at)` table, the holder and attempt fences,
and the same `store.Store` interface and semantics on both engines. River is not used. No lease
transaction takes an advisory lock; lock kind 3 is retired. The config lock (kind 1) and the event lock
(kind 2) of ADR 0028 are unchanged, and no lease transaction takes either.

### D22: host clock, checked at start (maintainer, 2026-11-10)

Lease times stay the host times passed through the `store` interface (`now`, `until`) on both engines. At
start, a process that runs the `scheduler` or `dispatcher` role on Postgres measures its skew against the
database clock, warns above 10 s and refuses to start above the refusal limit (§5). Using the database's
`now()` inside the Postgres lease queries (D22 b) stays a later option.

### 1. The lease queries ([internal/store/postgres/queries.sql](../../internal/store/postgres/queries.sql))

- `ListLeasableDueTargets` selects due targets with no unexpired lease,
  `ORDER BY next_check_at, id LIMIT n FOR NO KEY UPDATE SKIP LOCKED`.
- `ListLeasableDueDeliveries` keeps the three disjoint branches of #232/#235 (pending with a NULL
  `next_attempt_at`, pending and due, in flight), each read in order from
  `delivery_status_next_attempt_at_id`. Postgres refuses `FOR UPDATE` on a `UNION`, so each branch is a CTE
  with `LIMIT n FOR NO KEY UPDATE SKIP LOCKED`, and the final `ORDER BY next_attempt_at NULLS FIRST, id LIMIT n`
  merges them. The order is SQLite's: the storetest case `LeaseDueDeliveriesKeepsTheOrder` (#235's order
  test, run through the interface against a reference model) runs on both engines.
- A branch locks up to `n` rows, so a call locks at most `3n` rows and leases at most `n`. The rows the final
  `LIMIT` drops stay locked until the lease transaction commits, a few milliseconds, and other callers skip
  them meanwhile. They are not lost: they are due again at the next poll.
- `LeaseDueTargets` and `LeaseDueDeliveries` run select, lease upserts and (for deliveries)
  `StartDeliveryAttempt` in one READ COMMITTED transaction. A `testing` hook (`afterLeaseSelect`) sits
  between the select and the writes.

### 2. A lease is one upsert; a lost race skips the row

`AcquireLease` is one statement:

```sql
INSERT INTO lease (name, holder, expires_at) VALUES ($name, $holder, $until)
ON CONFLICT (name) DO UPDATE SET holder = excluded.holder, expires_at = excluded.expires_at
WHERE lease.holder = excluded.holder OR lease.expires_at <= $now
```

It takes a new lease, renews the holder's own lease, or takes over an expired one. Under READ COMMITTED the
`WHERE` is evaluated on the newest committed row, after waiting for a transaction that holds it. A lease
transaction uses the same upsert per selected row. When it writes no row (another holder's live lease
appeared after the select, for example from a caller whose statement snapshot was taken before this
transaction committed), the row is **skipped**: the call returns fewer rows and never `ErrConflict`
(ADR 0009 §2). A delivery whose `StartDeliveryAttempt` matches no row is skipped too and its new lease
released (ADR 0028 §3).

### 3. Fenced writes lock the row, then the lease

A fenced write must not apply after a takeover has committed. A plain `UPDATE … WHERE EXISTS (lease of
holder)` reads the lease from its statement snapshot and could miss a takeover that commits meanwhile, so:

- `UpdateTargetLeased` locks the target (`SELECT … FOR NO KEY UPDATE`), then the holder's lease row
  (`SELECT expires_at FROM lease WHERE name = $1 AND holder = $2 FOR UPDATE`), checks that it has not
  expired at `now`, and only then updates the target.
- `FinishDelivery` locks the delivery, then the holder's lease row (expired or not, as on SQLite, with the
  attempt fence of ADR 0017), then updates the delivery and releases the lease.
- A takeover locks the same two rows in the same order (the select, then the lease upsert). So a takeover
  either commits before the fence check, and the fenced write returns `false` and writes nothing, or waits
  for the fenced write to commit. The common order (row, then lease) keeps the two from deadlocking.
- `ReleaseLease` is a single fenced `DELETE`.
- Target and delivery rows are locked `FOR NO KEY UPDATE`, the lock an `UPDATE` of non-key columns takes,
  not `FOR UPDATE`. It still excludes other lease transactions, updates and deletes, so the fence argument
  holds, but not the `FOR KEY SHARE` that an insert of a referencing row (`watch.target_id`,
  `version.target_id`) takes: a config or check transaction inserting a watch or version neither hides the
  target from lease calls nor blocks `UpdateTargetLeased`. No foreign key references `delivery`.

### 4. Lease cleanup and the other writers

- `DeleteExpiredTargetLeases` is one statement that deletes expired target leases selected
  `FOR UPDATE SKIP LOCKED`: rows a renewal or takeover holds are left for the next run. Two instances
  running it at once neither wait for nor deadlock with each other or a lease transaction, and each expired
  lease is deleted once.
- Writers that take no lease (`FailPendingDeliveries`, `FailHeldDelivery`, `CancelHeldDeliveries`,
  `DeleteTargetIfUnused`, `SetTargetInterval`) now wait for the row locks of an open lease transaction, and
  lease transactions skip rows those writers hold. A writer therefore never changes a row between the
  select and the writes of a lease transaction; the re-checks of ADR 0028 §3 stay as a second guard.

### 5. The start-up clock check

`open.CheckClock` (with `postgres.Store.ClockSkew`) runs in `serve` after the store is opened, when the
process runs the `scheduler` or `dispatcher` role on Postgres, and before any role starts:

- five reads of `SELECT clock_timestamp()` (the time the server answers; `now()` is the start of the
  transaction), each between two reads of the host clock; the sample with the shortest round trip is kept;
- skew = database time − midpoint of the host times; the check uses |skew| + RTT/2, so the measurement's
  uncertainty counts against the host;
- above **10 s**: a warning in the log; above **15 s**: `serve` stops with an error wrapping
  `open.ErrClockSkew` that says to synchronise the host clock;
- the engine comes from `database.dsn`: on SQLite (one process, one host) there is nothing to check; on
  Postgres the check fails closed, so a store that cannot measure the skew is refused, not passed.

The refusal limit is the 15 s per host of ADR 0028 §9 (agent-decided item 4 there, merged with that ADR),
not the 30 s first written in #86: each host is measured against the database, so a per-host limit of 15 s
keeps any two hosts within the 30 s minimum margin of ADR 0017 §2. The `worker` role has no work of its
own and does not open the store, so it is not checked. Both limits are constants in
[internal/store/open/clock.go](../../internal/store/open/clock.go).

### 6. What does not change

- The `store.Store` interface, the lease TTLs and timeouts of ADR 0009 and ADR 0017, and the SQLite engine.
- Per-host rate limits stay per process (ADR 0027 §7); [configuration.md](../configuration.md#several-replicas-postgres)
  tells operators to divide them by the number of `scheduler` processes.

## Consequences

### Positive

- Several `scheduler` and `dispatcher` processes lease from one Postgres database at once, each getting a
  disjoint set, with the fences of ADR 0009 and ADR 0017 unchanged.
- No new dependency, one interface and one semantics for both engines.
- `LeaseDueDeliveries(limit=16)` with 20,000 pending and 1,000 in-flight deliveries took 12–13 ms on the
  development machine (Postgres 16, local socket; `BenchmarkLeaseDueDeliveries`), the query alone about
  2.5 ms, under the 20 ms target of #86.

### Negative

- A delivery lease call locks up to three times the rows it leases (§1). Concurrent dispatchers skip them for
  the length of one lease transaction.
- A lease call costs two statements per leased target (upsert, read) and five per leased delivery (upsert,
  start, event, subscription and sink reads). A poll of 16 is about 80 round trips; on a slow network to the database the
  call takes longer, though it holds no global lock.
- Clock drift after start is not detected (ADR 0028, open question).

### Open questions

- Batching the per-row statements of a lease call, if the benchmark on the CI runner or in 86-B shows the
  round trips matter.
- A periodic skew re-check, or the database clock in the lease queries (D22 b).

## Alternatives considered

- **River (D21 b).** Postgres only, with its own schema and migrations, and a second leasing path SQLite
  cannot share (ADR 0009 rejected it for that reason). See ADR 0028.
- **Database `now()` in the lease queries (D22 b).** Gives the `now`/`until` arguments a different meaning per
  engine and breaks the injected-clock tests. Left for later.
- **One `OR` query for deliveries with `FOR UPDATE SKIP LOCKED`.** Locks exactly `n` rows, but Postgres
  cannot read three status ranges of the index in one ordered scan, so it sorts the whole due backlog on
  every poll, the cost #232 removed on SQLite.
- **Select ids without locks, then lock them.** Locks only what it returns, but a caller whose candidates
  are all locked by another caller gets nothing, even with due rows left.
- **Keep the lease lock for the fenced writes.** Correct, but every fenced write would again queue behind
  every lease transaction of every process.

## References

- Issues angelia-dev/angelia#86 (D21–D23), #85, epic #18.
- ADRs [0002](0002-one-binary-several-roles.md), [0003](0003-sqlite-default-postgres-ha-no-external-queue.md),
  [0009](0009-scheduler-target-leases-and-backoff.md), [0017](0017-dispatcher-delivery-leases-and-retries.md),
  [0027](0027-configurable-per-host-rate-limits.md), [0028](0028-postgres-event-cursor-and-write-concurrency.md).
- Tests: `internal/store/storetest/multiprocess.go`, `internal/store/postgres/lease_test.go`,
  `internal/store/postgres/lease_bench_test.go`, `internal/store/open/clock_test.go`, and the Postgres
  variants in `internal/scheduler`, `internal/dispatch`, `internal/event` and `cmd/angelia`.
- PostgreSQL documentation: [the locking clause](https://www.postgresql.org/docs/17/sql-select.html#SQL-FOR-UPDATE-SHARE),
  [INSERT … ON CONFLICT](https://www.postgresql.org/docs/17/sql-insert.html#SQL-ON-CONFLICT).
