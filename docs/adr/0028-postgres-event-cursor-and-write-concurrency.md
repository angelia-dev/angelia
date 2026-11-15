# ADR 0028: On Postgres, event inserts take a transaction advisory lock so `event.seq` is in commit order; config, API and secret transactions share a second lock; the engine is `pgx` through `database/sql`

- **Status:** Accepted for D14–D22 (maintainer 2026-11-10); agent-decided items reviewed in the PR
- **Date:** 2026-11-10
- **Story:** angelia-dev/angelia#85 (sub-story 85-A); D21 and D22 from angelia-dev/angelia#86

> Written as sub-story 85-A. The maintainer accepted every recommendation of the "Human needed?" sections
> of #85 (D14–D20) and #86 (D21–D22) on 2026-11-10, before any engine code was written. This ADR records
> those answers and the design that follows from them. Design points the decisions did not settle are
> listed under [Agent-decided items for review](#agent-decided-items-for-review). D-numbers are the decision
> numbers of #85 and #86; they are not related to the D-numbers of other stories.

## Context

[ADR 0003](0003-sqlite-default-postgres-ha-no-external-queue.md) makes Postgres the store for HA and
scale-out. Story #85 adds the Postgres engine behind the same contracts as the SQLite engine:
`store.Store`, `store.APIStore` and `store.SecretStore` ([internal/store](../../internal/store/store.go)).
#86 then makes leasing concurrent across replicas. The code above the store was written against SQLite,
where one write lock serializes every write transaction. Two consequences of that have no Postgres
answer yet:

1. **The event cursor.** [ADR 0008](0008-store-history-rows-and-event-cursor.md) §2 makes `event.seq` the
   pull-API cursor and promises that a consumer resuming from its last `seq` sees every committed event.
   The API relies on it: `after` and `next_after` of `GET /v1/events` are `seq`
   ([ADR 0022](0022-rest-api-in-house-spec-driven-validation.md), open question "Event cursor order on
   Postgres"; ADR 0008 amendment of 2026-11-04). On SQLite the promise holds because insert order is
   commit order. On Postgres it does not (see *Why a sequence alone skips events* below).
2. **Write concurrency.** Code paths assume no other writer interleaves: `RecomputeTargetInterval` reads
   the watches' intervals and writes their minimum; file apply and API writes check name uniqueness and
   move watches between targets; secret rotation re-seals a batch in one transaction
   ([ADR 0024](0024-secrets-encrypted-at-rest.md) §5). Postgres at its default isolation (READ COMMITTED)
   lets these interleave.

Constraints: no new YAML keys before 1.0 (D20); the `store` interfaces are contracts (AGENTS.md); leases
keep the semantics of [ADR 0009](0009-scheduler-target-leases-and-backoff.md) and
[ADR 0017](0017-dispatcher-delivery-leases-and-retries.md); events are immutable and their payload is
returned byte for byte ([ADR 0015](0015-release-event-outbox-in-house.md)).

### Why a sequence alone skips events

A Postgres sequence (or identity column) hands out its value when the row is **inserted**, but other
sessions see the row only when its transaction **commits**. Two check transactions A and B:

| Time | A | B | Reader (`ListEventsAfter(cursor)`) |
|---|---|---|---|
| t1 | inserts event, gets `seq` 10 | | |
| t2 | | inserts event, gets `seq` 11 | |
| t3 | | commits | |
| t4 | | | sees 11 only, sets `cursor = 11` |
| t5 | commits | | |
| t6 | | | reads `seq > 11`: event 10 is never returned |

Event 10 is skipped forever. Reproduced on Postgres 16.15 with two `psql` sessions (A inserts and sleeps
2 s before committing, B inserts and commits 0.5 s later): a reader at 1.2 s sees `2:B` only; after both
commits the table holds `1:A, 2:B`. With the lock below, the same reader sees nothing until A commits,
then `1:A, 2:B`.

## Decision

### 1. Commit-ordered `event.seq`: a transaction advisory lock before the first event insert (D14)

- `event.seq` is `BIGINT GENERATED ALWAYS AS IDENTITY` with the default `CACHE 1`. No other code draws
  from its sequence, and `GENERATED ALWAYS` rejects an explicit `seq`.
- Every transaction that inserts an event takes the **event lock**,
  `pg_advisory_xact_lock($schema_key, 2)` (§4), **before its first event insert**, and holds it until it
  commits or rolls back (a transaction-scoped lock cannot be released early). The engine takes it on
  **every** `AppendEvent`, not only the first of a transaction: the lock is re-entrant, so a second call
  costs one statement and no wait, and a savepoint rolled back after the first call (which releases a lock
  taken inside it) cannot leave a later insert unlocked. The standalone `Store.AppendEvent` does the same.
- Only check transactions (`InTx`) and the standalone `Store.AppendEvent` insert events. `AppendEvent`
  inside `InConfigTx` or `InAPITx` is an error on both engines (agent-decided item 1).
- Transactions that insert no event (a check that found nothing new, a lease, a read) never take it.
- Readers take no lock. `ListEventsAfter` stays one `SELECT … WHERE seq > $1 ORDER BY seq LIMIT $2`,
  so it reads one snapshot.

**Correctness argument.** Call an *event transaction* one that inserts at least one event, and let L be
the event lock.

1. *Seq order is commit order.* An event transaction holds L from before its first `nextval` until its
   commit. Two event transactions therefore never hold L at the same time: if A takes L before B, A has
   drawn all its `seq` values and committed before B draws any. With `CACHE 1`, `nextval` is strictly
   increasing across sessions, so every `seq` of A is smaller than every `seq` of B, and A commits first.
2. *A visible set is a prefix.* At commit, Postgres makes a transaction visible to new snapshots
   (`ProcArrayEndTransaction`) before it releases its locks. So when B obtains L, A is already visible to
   every later snapshot. A snapshot that sees an event of B sees every event with a smaller `seq`: it is
   either B's own (same transaction, visible together) or belongs to a transaction that became visible
   before B took L, hence before B committed.
3. *Gaps are permanent.* A rolled-back event transaction burns its `seq` values. `nextval` never returns
   them again, so a gap can never be filled by a later commit.

From 2 and 3, the events a reader sees with `seq > cursor` are all the committed events above the cursor,
and nothing can later commit below the highest `seq` it saw. Resuming from the highest seen `seq` never
skips an event. This is the store-level test "no skipped event under concurrent writers" of #85 (run
`-count=100` on Postgres) and the API test `TestEventPaginationConcurrentWriters`.

**Assumptions the argument needs.** If one fails, the no-skip promise fails with it:

- the lock is held at every `nextval` of the event sequence (taken before the insert, on every
  `AppendEvent`, as above), and no insert path skips it;
- `CACHE 1` (with a cache, session X may hold values 1–20 and session Y 21–40, so Y can commit 21 under the
  lock before X commits 1);
- the sequence only moves up: no `ALTER SEQUENCE … RESTART`, `setval` downward, or `TRUNCATE … RESTART
  IDENTITY`, which would hand out a `seq` a reader has already passed;
- **no committed event is lost.** A failover that promotes an asynchronous replica, `synchronous_commit=off`,
  or a restore from backup can drop events a reader has already seen; the database then hands their `seq`
  values out again to new events, which the reader skips. Only a failover without data loss (synchronous
  replication, or the same storage) keeps the promise. A restore already requires a restart
  (`MatcherCache`), and consumers must be told to reset their cursor.

The engine reads from the primary only. A streaming or logical replica applies commits in WAL order, so
the prefix property would hold there too.

**Cost.** Event transactions serialize from their first event insert to their commit; nothing else does.
That is what SQLite already does for every write. The hold time is the rest of the outbox step plus the
commit. The 1–2 ms that [ADR 0015](0015-release-event-outbox-in-house.md) measured for an outbox step with
10,000 subscriptions is **SQLite**, where each statement is an in-process call. On Postgres each statement
is a network round trip, so the hold time grows with the number of statements after the lock: with one
insert per delivery it is about (events + deliveries) × round trip, plus one WAL flush. **85-D therefore
inserts the deliveries of an event in one batched multi-row statement**, so the hold time is about one
round trip per event plus the flush, whatever the number of subscriptions. Event-transaction throughput is
capped at about one transaction per hold time, since group commit cannot batch their flushes.

A first measurement, with `pgbench` on Postgres 16.15 (two 10 s runs per cell; one 8 s run for the
`synchronous_commit=off` row): each transaction inserts one version row (`ON CONFLICT DO NOTHING`), then
one 800-byte event and three deliveries, with or without the event lock before the event insert. The
sandbox was a shared 4-vCPU container with a load average near 7, so absolute numbers vary 2–5× between
runs:

| Clients | Without lock (tps, mean latency) | With lock (tps, mean latency) |
|---|---|---|
| 1 | 32 / 25 tps, 31–40 ms | 36 / 27 tps, 28–37 ms |
| 4 | 76 / 47 tps, 53–85 ms | 29 / 20 tps, 137–196 ms |
| 16 | 83 / 59 tps, 192–270 ms | 26 / 20 tps, 618–783 ms |
| 16, `synchronous_commit=off` | 60 tps, 268 ms | 18 tps, 870 ms |

With many concurrent emitters the lock capped them at the single-writer rate, 2.3–3.3× below the unlocked
rate, and transactions that emit nothing were unaffected. An earlier exploratory run, before the load rose,
gave 224 vs 105 tps at 1 client, 219 vs 43 at 4 and 142 vs 31 at 16 (without vs with lock); its 1-client
gap did not reproduce. These numbers show the shape of the cost, not its size on real hardware. Demand is
low (at the PRD's 10,000 watches on a 1 h interval there are about 2.8 checks per second, and only checks
that find a new release emit), and #86's crash benchmark on Postgres measures the real throughput.

### 2. Write concurrency: READ COMMITTED plus one config lock (D15)

- Transactions run at **READ COMMITTED**, Postgres's default. No `SERIALIZABLE`, so no `40001` retries and
  `InTx` callbacks need not be safe to rerun.
- **Config lock:** `InConfigTx`, `InAPITx` and `InSecretTx`, and every `Store` method that writes config
  tables in its own transaction (`CreateTarget`, `CreateWatch`, `DeleteWatch`, `CreateSink`, `DeleteSink`,
  `CreateSubscription`, `DeleteSubscription`), take `pg_advisory_xact_lock($schema_key, 1)` as their
  **first statement**. Config, API and secret writes therefore serialize against each other exactly as on
  SQLite. This is what `RecomputeTargetInterval` (lost update between two applies), the file apply and the
  API (name checks, moving a watch and its held versions) and secret rotation (a batch re-sealed with no
  write in between) rely on. Unique indexes still back every name check (`23505` → `store.ErrConflict`).
- `UpdateSecret` keeps its `revision` fence (`UPDATE … WHERE id = $1 AND revision = $2`). Under READ
  COMMITTED a concurrent update of the same row makes Postgres re-check the `WHERE` on the newest row
  version, so exactly one of two writers at the same revision wins, with or without the lock
  (ADR 0024 §5).
- **Check transactions** (`InTx`) take no config lock. `ListWatches` reads the watches with `FOR SHARE`
  (agent-decided item 2). A config transaction that updates or deletes one of those watches waits until
  the check commits, and a check that starts after the config write sees it. Without this, a file apply
  could move a watch to another target and delete its held versions (`DeletePendingReleasesOfWatch`) while
  a running check adds a held version for the old target (`AddPendingRelease`): the foreign key from
  `pending_release` to `watch` takes only a `FOR KEY SHARE` lock, which does not conflict with an update of
  `watch.target_id`, so both commit and the stale hold comes back if the watch moves back (ADR 0014).
  - **Ordering rule.** The lock protects only if the config transaction **updates the watch row before it
    deletes the held versions**: the `UpdateWatch` is what waits for the check's share lock, and the delete
    after it then sees the check's committed row. With the delete first, the delete runs before the check
    commits and the stale row survives even with `FOR SHARE` (reproduced on Postgres 16). Both callers
    follow the rule today: `UpdateWatch` then `DeletePendingReleasesOfWatch` in
    [internal/watchfile/apply.go](../../internal/watchfile/apply.go) and
    [internal/api/watches.go](../../internal/api/watches.go). A comment at both call sites and a `storetest`
    case in 85-D keep it.
  - The query is shared, so `ListWatches` also locks inside `ConfigTx`/`APITx` (rows the config transaction
    may change anyway, under the config lock) and in the standalone `Store.ListWatches` (released at once,
    autocommit). Both are harmless.
- **Subscriptions under READ COMMITTED (known difference from SQLite).** A check reads
  `SubscriptionGeneration` and then the subscriptions as statements of its own transaction, each with its
  own snapshot, and a subscription write can commit while the check runs. On SQLite the write would wait
  for the check. On Postgres an event can be routed by the subscription set of a moment earlier: an event
  that commits just after a subscription is created may have no delivery for it, and one that commits just
  after a subscription is deleted may have a delivery for it (the dispatcher already handles a missing
  subscription, ADR 0008 §1). The `MatcherCache` stays correct: it reads the generation before the list
  ([internal/event/cache.go](../../internal/event/cache.go)), so a concurrent write can only make the cache
  look older than its content, never newer. The `subscription_generation` triggers update one counter row,
  so subscription writers also queue on that row lock (ADR 0015).
- `PruneHistory` takes no advisory lock. It deletes old rows only, and its `event_prune_seq` trigger
  updates one row, so two prunes queue on that row lock.
- **Fallback (D15 b):** if review or the 85-D tests find a path that the config lock and the row locks
  above leave open, every write transaction takes one global advisory lock instead, which is SQLite's
  model exactly. Leases keep their own lock (§3) in that case too.

### 3. Leases (#85 serialized, #86 concurrent) and the in-house queue (D21)

- **D21: leasing stays in-house.** The existing `lease(name, holder, expires_at)` table and
  `SELECT … FOR UPDATE SKIP LOCKED` on `target` and `delivery`, with the same interface and semantics on
  both engines. River is not used. This answers the open question of ADR 0003 and the "River, or a small
  in-house lease table" choice of architecture.md §6.
- **In #85** every lease transaction (`AcquireLease`, `ReleaseLease`, `LeaseDueTargets`,
  `LeaseDueDeliveries`, `UpdateTargetLeased`, `FinishDelivery`, `DeleteExpiredTargetLeases`) takes the
  **lease lock** `pg_advisory_xact_lock($schema_key, 3)` as its first statement. Lease transactions then
  exclude each other across processes, and the holder and attempt fences of ADR 0009 and 0017 and
  `next_attempt_at NULLS FIRST` keep their meaning.
- **The lease lock alone is not SQLite's semantics.** On SQLite nothing else writes between the select of
  due rows and the writes of the lease transaction. On Postgres, writers that take no lease lock can:
  `FailPendingDeliveries` and `FailHeldDelivery` (set a delivery `dead`), `CancelHeldDeliveries` (delete a
  held delivery) and `DeleteTargetIfUnused` (delete a target). The SQLite engine's `StartDeliveryAttempt`
  ([queries.sql](../../internal/store/sqlite/queries.sql)) is an unconditional `UPDATE … WHERE id = ?`;
  ported as is, a delivery made `dead` in between would come back `in_flight`, and one deleted in between
  would return `ErrNotFound` and abort the whole batch. **85-D requires:** the Postgres
  `StartDeliveryAttempt` repeats the status and due predicate of `ListLeasableDueDeliveries` in its
  `WHERE` (pending and due, or in flight with no unexpired lease), and 0 rows means skip that row and drop
  its new lease, not an error. Likewise `LeaseDueTargets` skips a target that is gone when it is read back.
  A `storetest` case covers each. Under READ COMMITTED the `UPDATE` re-checks its `WHERE` on the newest row
  version, so this is safe without the lease lock too.
- **In #86** the lease lock (key 3) is **retired entirely**. `LeaseDueTargets` and `LeaseDueDeliveries`
  select with `FOR UPDATE SKIP LOCKED`; a lease row that appears between the select and the insert skips
  that row, never aborts the batch (ADR 0009 §2). `AcquireLease` is one upsert whose `WHERE` admits only
  the same holder or an expired lease. `ReleaseLease` and `DeleteExpiredTargetLeases` are single fenced
  statements. A fenced write (`UpdateTargetLeased`, `FinishDelivery`) reads its lease row `FOR UPDATE` in
  the same transaction as the write, so a takeover commits either before the fence check (the write is
  dropped) or after the write. No lease transaction takes the config or event lock.

### 4. Lock keys, lock order and lock waits

- **Keys.** The two-argument form `pg_advisory_xact_lock(int4, int4)`. The first argument is the schema
  key, a stable hash of `current_schema()` (`hashtext(current_schema())`), the second the lock kind:
  1 config, 2 event, 3 lease (until #86). `Open` computes the schema key **once**, refuses to start when
  `current_schema()` is NULL (the `search_path` names no existing schema), and passes the key as a query
  parameter to every lock statement. Two databases never share advisory locks; two Angelia schemas in one
  database (and the per-test schemas of #85) do not contend. A hash collision with another schema or
  application only adds waiting, never breaks correctness.
- **Order.** A transaction takes at most these advisory locks, in this order:
  1. the config lock as its first statement, or (until #86) the lease lock as its first statement, never
     both;
  2. its row locks, statement by statement;
  3. the event lock, before each event insert (check transactions only).

  No transaction takes the config lock after any other lock, a lease transaction takes no other advisory
  lock, and no transaction takes both the config and the event lock (config and API transactions do not
  append events, §1). So no transaction waits for one advisory lock while holding another, and advisory
  locks alone cannot deadlock. After #86 only the config and event locks remain.
- **Residual deadlocks with row locks.** A transaction waiting for the event lock may already hold row
  locks (its version inserts, its watches `FOR SHARE`). A cycle needs the event-lock holder to wait,
  directly or through a third transaction, for one of those rows: two overlapping checks of the same target
  after a lost lease (ADR 0009 §5 says this can happen), or a config write touching both a checked target's
  watches and another check's held deliveries at the same moment. Two overlapping checks of the same target
  can also deadlock on their own: both hold `FOR SHARE` on a watch and both call `MarkBaselineDone` on it,
  a share-to-update upgrade. Postgres detects every such cycle after `deadlock_timeout` (1 s by default)
  and aborts one transaction with `40P01`. The engine returns it as a wrapped error (no store sentinel). A
  failed check is retried with backoff, and the idempotent commit of ADR 0009 §5 makes the retry harmless.
- **Lock waits are bounded** by the caller's context: when a deadline or cancellation fires, pgx cancels
  the waiting statement, and the transaction fails and rolls back. An operator can also set `lock_timeout`
  (`55P03`) or `statement_timeout` (`57014`) through the DSN's `options` parameter. The engine returns these
  as wrapped errors, and callers retry them like any failed transaction (a check with backoff, a lease poll
  on its next tick, an API call with a 5xx the client may retry).

### 5. Driver and dependency (D16)

`github.com/jackc/pgx/v5` **v5.12.0** (MIT), used through `database/sql` with `pgx/v5/stdlib`, so the
engine mirrors the SQLite engine's `withTx`/`Queries` structure and goose runs on the same `*sql.DB`.
Modules it adds:

| Module | Version | Licence | Note |
|---|---|---|---|
| `github.com/jackc/pgx/v5` | v5.12.0 | MIT | direct |
| `github.com/jackc/puddle/v2` | v2.2.3 | MIT | linked: `pgx/v5/stdlib` imports `pgxpool` |
| `github.com/jackc/pgpassfile` | v1.0.0 | MIT | `.pgpass` lookup |
| `github.com/jackc/pgservicefile` | v0.0.0-20240606120523-5a60cdf6a761 | MIT | `pg_service.conf` lookup |

`golang.org/x/sync` and `golang.org/x/text` are already required at newer versions (v0.24.0, v0.43.0)
than pgx needs (v0.22.0, v0.41.0). `stretchr/testify` is required by pgx's own tests only. pgx needs
Go 1.25; go.mod is at 1.27. `CGO_ENABLED=0` builds are unaffected (pgx is pure Go).

**When the version lands (note for 85-C).** v5.12.0 was tagged on 2026-10-31. The repository's
`renovate.json` sets `minimumReleaseAge: 14 days`, so the go.mod and go.sum change that adds it waits
until **2026-11-14**. The version stays v5.12.0 (agent-decided item 6).

sqlc (v1.31.1, as for SQLite) generates `internal/store/postgres/*.gen.go` from a second `sql:` entry
(`engine: postgresql`) in `internal/store/sqlc.yaml`. Queries take lists as JSON text
(`json_array_elements_text($1::json)`, the twin of SQLite's `json_each`) rather than array parameters,
because sqlc's `database/sql` output wraps Postgres arrays in `github.com/lib/pq`'s `pq.Array`, a module we
do not take.

### 6. Supported versions, deployment and CI (D17, D18)

- **Postgres 15 to 18 are supported; CI runs 17**, on the image digest already pinned in
  `deploy/compose/full/compose.yaml` (`postgres:17-alpine@sha256:562a6e0a…`). 14 reaches end of life in
  November 2026. Nothing in this design needs a feature newer than 15 (the JSON checks below use casts, not
  Postgres 16's `IS JSON`).
- **Transaction-mode connection poolers are not supported in 1.0** (for example PgBouncer with
  `pool_mode = transaction`): connect directly, or through a session-mode pooler. Three things need one
  server session per client connection: goose's migration lock is a session-level advisory lock; pgx's
  default statement cache prepares statements per server session; and the `search_path` that selects the
  schema is set per session through the DSN. The transaction-scoped advisory locks of §1–§3 would work
  behind such a pooler, but the rest would not.
- The CI `test` job gets a `services: postgres` container with a `pg_isready` health check and sets
  `ANGELIA_TEST_POSTGRES_DSN` and `ANGELIA_TEST_POSTGRES_REQUIRED=1`. Postgres tests therefore count towards
  the existing 80% coverage gate on `internal/`, and cannot pass silently by skipping. No other job changes,
  and `CI OK` keeps its `needs` list. Without the variable, Postgres tests skip locally.

### 7. Migrations and schema (D19)

- **One initial Postgres migration numbered at the SQLite head** when 85-C merges (`00018`, or `00019` if
  #234 merges first), creating the head schema directly, then **one Postgres twin per later SQLite
  migration**. "Version N" means the same schema on both engines, and from 85-C on every migration ships
  both engines. Embedded through `migrations.Postgres()` next to `migrations.SQLite()`. No Postgres database
  exists yet, so nothing needs the 18 historic steps (00011's duplicate check and 00013's `sqlite_sequence`
  recovery have no meaning on a new database).
- Concurrent startups migrate under goose's Postgres session locker (`github.com/pressly/goose/v3/lock`,
  already in the module graph through goose v3.29.0), with `lock.WithLockID` derived from the schema (as
  the schema key of §4), so migrations of different schemas, such as the per-test schemas, do not
  serialize on goose's default lock id.
- **Column mapping** (agent-decided item 7, checked by the parity test of #85): `INTEGER` millisecond
  timestamps and counters become `BIGINT` (ADR 0008 §3); `TEXT` stays `TEXT`; `BLOB` becomes `BYTEA`;
  `INTEGER` 0/1 flags (`baseline_done`, `prerelease`, `yanked`, …) become `BOOLEAN` with no CHECK.
- **JSON columns stay `TEXT`**, with `CHECK (col::json IS NOT NULL)` as the twin of `json_valid` (an invalid
  value fails the cast with `22P02`, mapped to `store.ErrInvalid`). Not `JSONB`, which reorders keys, drops
  whitespace and rewrites escapes: `event.payload_json` and `sink.config_json` must come back byte for byte
  (ADR 0015). Not `json` either, so the parity test compares `TEXT` with `TEXT` and sqlc generates the same
  Go types as on SQLite. The `json` cast (not `jsonb`) accepts what SQLite's `json_valid` accepts, including
  `\u0000`, and keeps the text unchanged (checked on Postgres 16.15). The yank lookup's expression index is
  on `(watch_id, (payload_json::json ->> 'subject'))`, and its query uses the same expression (ADR 0018);
  `json_in`, `->>` and `hashtext` are all `IMMUTABLE`, so the index is allowed.
  One difference remains: `->>` fails with "unsupported Unicode escape sequence" when the document contains
  `\u0000` **anywhere**, not only in `subject`, so the expression index makes Postgres reject an event
  payload that contains an escaped NUL, which SQLite stores. See open questions.
- Triggers have Postgres twins with the same firing conditions: `event_immutable`, the three
  `subscription_generation` triggers and `event_prune_seq`.
- **As built in 85-C (2026-11-14).** The SQLite head was `00020` (the `consumer` table of #79) when 85-C
  landed, so the initial Postgres migration is `00020_initial_schema.sql` and creates `consumer` too; there
  is no `delivery_status_next_attempt_at` (#234). `delivery_status_next_attempt_at_id` is built as
  `(status, next_attempt_at NULLS FIRST, id)`, so the lease order `next_attempt_at NULLS FIRST, id` walks it
  without a sort. The goose lock id is the 64-bit value `schema_key << 32`, which reads as `(schema_key, 0)`
  in `pg_locks`. A test in `internal/store/migrations` fails when a SQLite migration from `00020` on has no
  Postgres twin with the same number, or the reverse. Test schemas come from `internal/store/pgtest`.
  Every `TEXT` column is `COLLATE "C"`, so keys, unique indexes and `ORDER BY`/keyset comparisons use byte
  order like SQLite's `BINARY`, independent of the server locale and of glibc/ICU versions (a test checks
  `attcollation`). The subject expression is collated too: the index is on
  `(watch_id, ((payload_json::json ->> 'subject') COLLATE "C"))`, and the yank query must repeat the
  `COLLATE "C"` to use it. A btree entry is limited to about 2.7 KB on Postgres (a third of an 8 KB page),
  so the unique index on `(kind, coordinates_json)` refuses a target whose coordinates exceed that
  (`54000`), where SQLite has no such limit; coordinates are a few short fields, far below it.

- **As built in 85-D (2026-11-14).** `internal/store/postgres` implements `store.APIStore` and passes the
  `storetest` suite. Every transaction is begun at READ COMMITTED explicitly, and the engine sets
  `default_transaction_isolation` to `read committed` on its connections, so a server or role default of
  REPEATABLE READ or SERIALIZABLE changes nothing (the suite runs under such a default too). The event lock
  is taken on every `AppendEvent`, and the deliveries of an event go in one
  `INSERT … SELECT … FROM json_populate_recordset(NULL::delivery, $1::json)`. `AppendEvent` in `InConfigTx`
  or `InAPITx` returns `store.ErrInvalid` on both engines. `LeaseDueTargets` reads each target back after
  writing its lease and skips (and unleases) one deleted meanwhile; `StartDeliveryAttempt` repeats the due
  predicate, and a row it no longer matches is skipped and unleased. Errors: `23505` → `ErrConflict`,
  `23503` → `ErrInvalidRef`, `23514`, `22P02`, `22P05` and `22021` (a NUL byte or invalid UTF-8 in a `TEXT`
  value) → `ErrInvalid`; `P0001` (`event_immutable`), `40P01` and lock or statement timeouts keep no
  sentinel, as on SQLite. The default pool size is `max(4, number of CPUs)`, pgxpool's default; the other
  pgxpool DSN parameters (`pool_min_conns`, `pool_max_conn_lifetime`, …) are refused with `ErrInvalid`.
  **Text a check or a delivery writes is normalised before it is stored, on both engines**, so that bad
  upstream text cannot wedge a target or a delivery on Postgres (whose `TEXT` holds no NUL and no invalid
  UTF-8, and whose subject index refuses a `\u0000` escape):
  - event payloads: `internal/event` (`encodePayload`) writes `\ufffd` (U+FFFD) in place of every
    `\u0000` escape (invalid UTF-8 is already U+FFFD from `encoding/json`); the yank lookup encodes its
    subject the same way (`subjectKey`);
  - version strings: `internal/diff` skips a fetched raw version that is not valid UTF-8 or holds a NUL, as
    it skips a blank or padded one (counted in `Skipped`);
  - delivery errors: `internal/dispatch` (`truncate`) and `FinishDelivery` of both engines replace invalid
    UTF-8 and NUL in `LastError` with U+FFFD.

  Nothing else is normalised: other text (names, labels, filters, held-delivery reasons, consumer names)
  comes from configuration or the API, and on Postgres a NUL or invalid UTF-8 there is refused with
  `ErrInvalid` where SQLite stores it.

### 8. Configuration surface and pool size (D20)

No new YAML keys. `database.dsn` already accepts `postgres://` and `postgresql://`. TLS (`sslmode`, …) and
server settings (`options`) are DSN parameters, and the password comes from the DSN or
`PGPASSWORD`/`.pgpass`. The DSN, and any part of it, is never logged.

**Pool size** (agent-decided item 5). `pool_max_conns` is a `pgxpool` parameter. Through `pgx/v5/stdlib`,
pgx does not recognise it and sends it to the server as a run-time setting, and the connection fails with
`FATAL: unrecognized configuration parameter "pool_max_conns"` (`42704`). The engine therefore reads
`pool_max_conns` from the DSN itself, removes it before handing the DSN to pgx, and applies it with
`*sql.DB.SetMaxOpenConns`, with a default chosen in 85-D when it is absent. This keeps D20: the pool size is
a DSN parameter, with the name pgx users already know.

### 9. Clock source for leases (D22)

Lease times stay **host times passed through the `store` interface** (`now`, `until`), on both engines.
Using the database's `now()` inside the Postgres lease queries (ADR 0009 §2's "should") stays a later
option: it would give the interface arguments a different meaning per engine and break the injected-clock
tests.

**Start-up skew check.** When the `scheduler`, `worker` or `dispatcher` role starts on Postgres, it measures
its skew against the database clock:

- It takes a few samples (for example 5) of `SELECT clock_timestamp()`, noting the host time before and
  after each, and keeps the sample with the smallest round trip. `clock_timestamp()`, not `now()`: `now()`
  is the start of the transaction, not the time the server answered.
- Skew is the database time minus the midpoint of the host times. Half the round trip is the measurement's
  uncertainty and counts **against** the host: the check uses |skew| + RTT/2.
- Above **10 s** it logs a warning. Above **15 s** it refuses to start those roles with a clear error.

**Why 15 s, not 30 s** (agent-decided item 4). D22 as recommended in #86 refuses above 30 s, the dispatcher
margin of ADR 0017 §2. But each host measures its skew against the database, not against the other hosts.
Two hosts each 30 s off in opposite directions are 60 s apart, twice the margin. ADR 0017 §2 requires
host clocks to differ by less than `max(30 s, LeaseTTL/4)`, which is at least 30 s. With a per-host limit
of 15 s, any two hosts that passed the check differ by at most 30 s, so the check enforces the margin even
at its minimum. The 10 s warning stays as decided. Implemented and tested with an injected clock in #86.

**Why D21 and D22 are here and not in a later ADR.** #86 asks for them as an amendment to this ADR, or a
new one if this one was already accepted. They were decided on the same day, and they bound the same design:
D21 fixes that the lease lock of §3 is replaced by `SKIP LOCKED` on the same tables, not by a second queue,
which the lock order of §4 depends on; D22 fixes which clock the fences of §3 compare. #86 implements both.

## Decision record

| # | Decision | Answer (maintainer, 2026-11-10) | Section |
|---|---|---|---|
| D14 | Commit-ordered `event.seq` on Postgres | (a) transaction advisory lock before the first event insert | §1 |
| D15 | Write concurrency model | (a) READ COMMITTED, one advisory lock for config, API and secret transactions, the event lock, lease locks; config lock first. Fallback (b) one global write lock | §2, §4 |
| D16 | Dependency `github.com/jackc/pgx/v5` | v5.12.0 (MIT) with its MIT transitive modules, through `database/sql` (`pgx/v5/stdlib`) | §5 |
| D17 | Supported Postgres versions | 15 to 18; CI on 17 | §6 |
| D18 | CI change | Postgres service container in the existing `test` job, in the coverage gate | §6 |
| D19 | Migration numbering | One initial migration numbered at the SQLite head, then one twin per later migration | §7 |
| D20 | Configuration surface | No new YAML keys; DSN parameters and `PGPASSWORD` | §8 |
| D21 (#86) | In-house `SKIP LOCKED` or River | In-house | §3 |
| D22 (#86) | Clock source for leases | Host clock; start-up skew check, warn above 10 s, refuse `scheduler`/`dispatcher` above 30 s (this ADR proposes 15 s per host, item 4) | §9 |

## Agent-decided items for review

Points the maintainer's decisions did not settle. They are recorded as decided, and the PR review confirms
or changes them.

1. **`AppendEvent` inside `InConfigTx` or `InAPITx` returns an error, on both engines** (§1, §4). Nothing
   appends events there today. Enforcing it keeps the advisory locks free of a config → event edge, which
   would deadlock with `FOR SHARE` on watches. 85-D enforces it in both engines, with a SQLite test and a
   `storetest` case.
2. **`ListWatches` reads with `FOR SHARE`**, with the rule "update the watch before deleting its held
   versions" in config transactions (§2).
3. **Conditional `StartDeliveryAttempt` on Postgres**, 0 rows skips the row (§3).
4. **Skew refusal at 15 s per host instead of 30 s**, with `clock_timestamp()`, the minimum-RTT sample and
   RTT/2 counted against the host; the check covers the `scheduler`, `worker` and `dispatcher` roles (§9).
5. **`pool_max_conns` read and stripped by the engine** and applied with `SetMaxOpenConns` (§8).
6. **pgx v5.12.0 lands in go.mod on or after 2026-11-14** (Renovate's 14-day minimum release age, §5).
7. **Schema mapping:** `TEXT` for JSON columns with a `::json` CHECK, `BOOLEAN` flags, lists as JSON
   parameters (§5, §7).
8. **Lock keys per schema**, computed once at `Open`; the goose lock id per schema; transaction-mode poolers
   unsupported in 1.0 (§4, §6, §7).
9. **The event lock is taken on every `AppendEvent`** and deliveries are inserted in one batched statement
   per event (§1).

## Consequences

### Positive

- The pull API's no-skip promise holds on Postgres, with a proof whose assumptions are listed and
  checkable, and the `seq` cursor and the API stay as they are.
- Code written for SQLite keeps its guarantees: config, API and secret writes serialize as before, and a
  check never interleaves with a config change to its own watches.
- Check transactions that emit nothing, lease transactions (after #86) and reads run concurrently, which is
  what scaling out needs.
- One driver, pure Go, MIT, and the same `database/sql` structure as the SQLite engine.
- No new configuration surface to freeze before 1.0.

### Negative

- Event-emitting transactions serialize. With many concurrent emitters their throughput is capped at one
  transaction per hold time, and the hold time includes network round trips (§1).
- A transaction that stalls while holding the event lock (a stuck client between an event insert and its
  commit) stalls every emitter. Check transactions do no network I/O inside the transaction (ADR 0009 §5),
  and an operator can bound a stuck session with `idle_in_transaction_session_timeout` on the server or in
  the DSN's `options`.
- Rare deadlocks between the event lock and row locks, or between two checks of the same target, are
  possible (§4). Postgres resolves them in about 1 s, and the failed check is retried.
- The no-skip promise needs a failover without data loss (§1).
- Subscription routing under READ COMMITTED can use the subscription set of a moment earlier (§2).
- Config, API and secret writes do not run in parallel across replicas. They are rare (file apply at
  start, API calls, rotation).
- Per-host rate limits are per process ([ADR 0027](0027-configurable-per-host-rate-limits.md) §7). With N
  replicas running checks, a host can receive N times its configured rate; operators divide the limits by
  the number of `scheduler`/`worker` processes (documented by #86).
- Transaction-mode connection poolers are not supported (§6).
- Every future migration is written twice, once per engine.

### Open questions

- **Drift after start.** D22 checks skew once, at start. A clock that drifts later is not detected.
  A periodic re-check, or the database `now()` of D22 (b), is left for later.
- **NUL in event payloads.** A registry string with a NUL character becomes `\u0000` in the payload, and
  the subject index then rejects the event on Postgres (§7), so that check fails on every retry. 85-C or
  85-D should add a test and pick one of: drop or replace NUL when the event is built (applies to both
  engines, changes no stored data today); or keep the subject in its own `TEXT` column (a schema difference
  from SQLite, unless both engines get it). Recommendation: the first. *Answered in 85-D: replaced with U+FFFD
  when the event is built, with the other normalisations listed in §7 (as built in 85-D); for the
  maintainer to confirm.*
- The pointers in architecture.md §3.2 and §5 and in ADR 0008 and 0022 to this ADR are added with the
  engine (85-E), when the promise starts to hold. ADR 0002's and ADR 0003's open questions are closed by #86.

## Alternatives considered

- **D14 (b): visibility horizon.** Store `pg_current_xact_id()` with each event and read only below
  `pg_snapshot_xmin(pg_current_snapshot())`. Writers never wait. But the horizon is in transaction-id order
  and the cursor is in `seq` order: a transaction with a smaller xid can draw a larger `seq`, so a reader can
  still skip unless the cursor becomes `(xid, seq)`, which changes the public API. A long-running
  transaction also holds back every reader.
- **D14 (c): `SERIALIZABLE` for event transactions.** Serializable isolation orders transactions logically,
  not commits by `seq`; the skip above is still a valid serializable history.
- **Commit timestamps (`track_commit_timestamp`).** Not unique, not monotonic across sessions, and a server
  setting the operator must enable.
- **Taking the event lock at the start of every check transaction.** Simpler deadlock story, but it
  serializes every check commit, including the many that emit nothing, which #86 needs to run in parallel.
- **Taking the event lock only on the first `AppendEvent` of a transaction.** One statement less per event,
  but a savepoint rolled back after it would release the lock and leave later inserts unlocked.
- **D15 (b): one global write lock** for every write transaction. SQLite's semantics exactly and simplest,
  but no write concurrency at all. Kept as the fallback.
- **D15 (c): `SERIALIZABLE` with retry on `40001`.** Every `InTx` callback would need to be safe to rerun,
  which today's callers (counters, caches, logs) are not.
- **Native `pgxpool` instead of `database/sql`.** Pool settings through the DSN for free, but a second
  transaction and query structure next to SQLite's, and goose would need its own `*sql.DB`.
- **`github.com/lib/pq`.** In maintenance mode; pgx is the maintained pure-Go driver.
- **Replaying all SQLite migrations one for one (D19 b).** Steps 00011 and 00013 only make sense on an
  existing SQLite database.
- **New YAML keys such as `database.max_conns` (D20 b).** A public contract to freeze before 1.0 for
  settings the DSN already carries.
- **River (D21 b).** MPL-2.0 and well built, but Postgres only: it brings its own schema, migrations and
  modules (`riverdriver`, `rivershared`, `rivertype`, `robfig/cron/v3`, `tidwall/gjson`/`sjson`), and a
  second leasing path SQLite cannot share, the reason ADR 0009 rejected it.
- **Database clock in lease queries (D22 b).** See §9.
- **Refusing at 30 s of skew per host (D22 as first written).** Lets two hosts differ by up to 60 s, beyond
  the 30 s minimum margin of ADR 0017 §2 (§9).

## References

- Issues angelia-dev/angelia#85 (D14–D20) and #86 (D21–D23; D23 is an executor choice and is not recorded
  here), epic #18.
- [architecture.md §3.2](../architecture.md#32-scheduler-and-workers), [§5](../architecture.md#5-data-model-simplified),
  [§6](../architecture.md#6-language-and-framework-phase-0-assumptions).
- ADRs [0003](0003-sqlite-default-postgres-ha-no-external-queue.md), [0008](0008-store-history-rows-and-event-cursor.md),
  [0009](0009-scheduler-target-leases-and-backoff.md), [0014](0014-subscription-policy-overrides.md),
  [0015](0015-release-event-outbox-in-house.md), [0017](0017-dispatcher-delivery-leases-and-retries.md),
  [0018](0018-yank-events-reference-the-original.md), [0022](0022-rest-api-in-house-spec-driven-validation.md),
  [0024](0024-secrets-encrypted-at-rest.md), [0027](0027-configurable-per-host-rate-limits.md).
- PostgreSQL documentation: [advisory locks](https://www.postgresql.org/docs/17/explicit-locking.html#ADVISORY-LOCKS),
  [sequence functions](https://www.postgresql.org/docs/17/functions-sequence.html) (values are never rolled
  back), [READ COMMITTED and concurrent updates](https://www.postgresql.org/docs/17/transaction-iso.html#XACT-READ-COMMITTED),
  [row-level lock modes](https://www.postgresql.org/docs/17/explicit-locking.html#LOCKING-ROWS).
