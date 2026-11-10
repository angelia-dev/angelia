# ADR 0028: On Postgres, event inserts take a transaction advisory lock so `event.seq` is in commit order; config, API and secret transactions share a second lock; the engine is `pgx` through `database/sql`

- **Status:** Accepted (maintainer decision 2026-11-10)
- **Date:** 2026-11-10
- **Story:** angelia-dev/angelia#85 (sub-story 85-A); D21 and D22 from angelia-dev/angelia#86

> Written as sub-story 85-A. The maintainer accepted every recommendation of the "Human needed?" sections
> of #85 (D14–D20) and #86 (D21–D22) on 2026-11-10, before any engine code was written. This ADR records
> those answers and the design that follows from them. D-numbers are the decision numbers of #85 and #86;
> they are not related to the D-numbers of other stories.

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

Constraints: no new YAML keys before 1.0 ([architecture.md §6](../architecture.md#6-language-and-framework-phase-0-assumptions)
names "Postgres (`pgx`)" but no version); the `store` interfaces are contracts (AGENTS.md); leases keep the
semantics of [ADR 0009](0009-scheduler-target-leases-and-backoff.md) and
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
  `pg_advisory_xact_lock(<schema key>, 2)` (§4), **before its first event insert**, and holds it until
  it commits or rolls back (a transaction-scoped lock cannot be released early). The engine takes it once
  per transaction, on the first `AppendEvent`, whether the transaction is `InTx`, `InConfigTx` or
  `InAPITx`, and in the standalone `Store.AppendEvent`.
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

The argument needs all three of: the lock taken **before** the first `nextval`; `CACHE 1` (with a cache,
session X may hold values 1–20 and session Y 21–40, so Y can commit 21 under the lock before X commits 1);
and no insert path that skips the lock. A streaming or logical replica applies commits in WAL order, so
the prefix property holds there too; the engine reads from the primary only.

**Cost.** Event transactions serialize from their first event insert to their commit; nothing else does.
That is what SQLite already does for every write. The hold time is the rest of the outbox step plus the
commit: inserting the events and their deliveries (about 1–2 ms with 10,000 subscriptions and a warm
cache, [ADR 0015](0015-release-event-outbox-in-house.md)) and one WAL flush. Event-transaction throughput
is therefore capped at about one transaction per hold time, because group commit cannot batch their
flushes. Demand is far below that: at the PRD's 10,000 watches on a 1 h interval there are about 2.8
checks per second, and only checks that find a new release write events. Measured cost, with `pgbench` on
Postgres 16.15 (two 10 s runs per cell; one 8 s run for the `synchronous_commit=off` row): each
transaction inserts one version row (`ON CONFLICT DO NOTHING`), then one 800-byte event and three
deliveries, with or without the event lock before the event insert. The sandbox was a shared 4-vCPU
container with a load average near 7 during the runs, so absolute numbers vary 2–5× between runs; read
the ratios:

| Clients | Without lock (tps, mean latency) | With lock (tps, mean latency) |
|---|---|---|
| 1 | 32 / 25 tps, 31–40 ms | 36 / 27 tps, 28–37 ms |
| 4 | 76 / 47 tps, 53–85 ms | 29 / 20 tps, 137–196 ms |
| 16 | 83 / 59 tps, 192–270 ms | 26 / 20 tps, 618–783 ms |
| 16, `synchronous_commit=off` | 60 tps, 268 ms | 18 tps, 870 ms |

One writer pays nothing measurable in these runs. With many concurrent emitters the lock caps them at the
single-writer rate (here 18–36 tps), 2.3–3.3× below the unlocked rate, while every transaction that emits
nothing is unaffected. The `synchronous_commit=off` row shows that in this sandbox the hold time was
dominated by statement time under load, not by the flush. An earlier exploratory run, before the load
rose, gave 224 vs 105 tps at 1 client, 219 vs 43 at 4 and 142 vs 31 at 16 (without vs with lock); its
1-client gap (an uncontended lock costs one extra statement) did not reproduce in the two runs above. A dedicated server with a fast WAL device flushes in well under 1 ms, so the
cap there is in the hundreds of event transactions per second. #86's crash benchmark on Postgres measures
it under the real workload.

### 2. Write concurrency: READ COMMITTED plus one config lock (D15)

- Transactions run at **READ COMMITTED**, Postgres's default. No `SERIALIZABLE`, so no `40001` retries and
  `InTx` callbacks need not be safe to rerun.
- **Config lock:** `InConfigTx`, `InAPITx` and `InSecretTx`, and every `Store` method that writes config
  tables in its own transaction (`CreateTarget`, `CreateWatch`, `DeleteWatch`, `CreateSink`, `DeleteSink`,
  `CreateSubscription`, `DeleteSubscription`), take `pg_advisory_xact_lock(<schema key>, 1)` as their
  **first statement**. Config, API and secret writes therefore serialize against each other exactly as on
  SQLite. This is what `RecomputeTargetInterval` (lost update between two applies), the file apply and the
  API (name checks, moving a watch and its held versions) and secret rotation (a batch re-sealed with no
  write in between) rely on. Unique indexes still back every name check (`23505` → `store.ErrConflict`).
- `UpdateSecret` keeps its `revision` fence (`UPDATE … WHERE id = $1 AND revision = $2`). Under READ
  COMMITTED a concurrent update of the same row makes Postgres re-check the `WHERE` on the newest row
  version, so exactly one of two writers at the same revision wins, with or without the lock
  (ADR 0024 §5).
- **Check transactions** (`InTx`) take no config lock. A check transaction reads its target's watches
  (`ListWatches`) with `FOR SHARE`. A config transaction that updates or deletes one of those watches
  waits until the check commits, and a check that starts after the config write sees it. Without this, a
  file apply could move a watch to another target and delete its held versions
  (`DeletePendingReleasesOfWatch`) while a running check adds a held version for the old target
  (`AddPendingRelease`): the foreign key from `pending_release` to `watch` takes only a `FOR KEY SHARE`
  lock, which does not conflict with an update of `watch.target_id`, so both commit and the stale hold
  comes back if the watch moves back (ADR 0014). This row lock is not part of the maintainer's D15 text;
  it is how this ADR meets D15's aim ("keeps SQLite's guarantees where the code relies on them").
- The subscription cache needs no lock: `MatcherCache` reads `SubscriptionGeneration` before
  `ListSubscriptions` ([internal/event/cache.go](../../internal/event/cache.go)), so under READ COMMITTED a
  concurrent subscription write can only make the cache look older than its content, never newer. The
  `subscription_generation` triggers update one counter row, so subscription writers also queue on that
  row lock (ADR 0015).
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
  **lease lock** `pg_advisory_xact_lock(<schema key>, 3)` as its first statement. That is correct across
  processes and gives SQLite's semantics, including the holder and attempt fences of ADR 0009 and 0017 and
  `next_attempt_at NULLS FIRST`.
- **In #86** `LeaseDueTargets` and `LeaseDueDeliveries` select with `FOR UPDATE SKIP LOCKED` and drop the
  lease lock. A lease row that appears between the select and the insert skips that row, never aborts the
  batch (ADR 0009 §2). A fenced write (`UpdateTargetLeased`, `FinishDelivery`) then reads its lease row
  `FOR UPDATE` in the same transaction as the write, so a takeover commits either before the fence check
  (the write is dropped) or after the write. No lease transaction takes the config or event lock.

### 4. Lock keys and lock order

- **Keys.** The two-argument form `pg_advisory_xact_lock(int4, int4)`: the first is a stable hash of
  `current_schema()` (for example `hashtext(current_schema())`), the second the lock kind, 1 config,
  2 event, 3 lease. Two databases never share advisory locks; two Angelia schemas in one database (and the
  per-test schemas of #85) do not contend. A hash collision with another schema or application only adds
  waiting, never breaks correctness. Only transaction-scoped locks are used, so they also work behind a
  connection pooler in transaction mode. (goose's migration lock, below, is session-scoped.)
- **Order.** A transaction takes at most these advisory locks, in this order:
  1. the config lock, as the first statement, or the lease lock, as the first statement (never both);
  2. its row locks, statement by statement;
  3. the event lock, just before the first event insert.

  No transaction takes the config lock after any other lock, and a lease transaction takes no other
  advisory lock. The only edge between advisory locks is config → event, so advisory locks alone cannot
  deadlock.
- **Residual deadlocks with row locks.** A transaction waiting for the event lock may already hold row
  locks (its version inserts, its watches `FOR SHARE`). A cycle needs the event-lock holder to wait, directly
  or through a third transaction, for one of those rows: two overlapping checks of the same target after a
  lost lease (ADR 0009 §5 says this can happen), or a config write touching both a checked target's watches
  and another check's held deliveries at the same moment. Postgres detects the cycle after
  `deadlock_timeout` (1 s by default) and aborts one transaction with `40P01`. The engine returns it as a
  wrapped error (no store sentinel). A failed check is retried with backoff, and the idempotent commit of
  ADR 0009 §5 makes the retry harmless. A config transaction must not append events (none does today): a
  config transaction holding the event lock and waiting for a watch that a check holds `FOR SHARE`, while
  that check waits for the event lock, would deadlock on every such overlap.

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

sqlc (v1.31.1, as for SQLite) generates `internal/store/postgres/*.gen.go` from a second `sql:` entry
(`engine: postgresql`) in `internal/store/sqlc.yaml`. Queries take lists as JSON text
(`json_array_elements_text($1::json)`, the twin of SQLite's `json_each`) rather than array parameters,
because sqlc's `database/sql` output wraps Postgres arrays in `github.com/lib/pq`'s `pq.Array`, a module we
do not take.

### 6. Supported versions and CI (D17, D18)

- **Postgres 15 to 18 are supported; CI runs 17**, on the image digest already pinned in
  `deploy/compose/full/compose.yaml` (`postgres:17-alpine@sha256:562a6e0a…`). 14 reaches end of life in
  November 2026. Nothing in this design needs a feature newer than 15 (the JSON checks below use casts, not
  Postgres 16's `IS JSON`).
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
  already in the module graph through goose v3.29.0). Being session-scoped, it needs a direct connection or
  a session-mode pooler during migration.
- **Column mapping** (agent-decided, checked by the parity test of #85): `INTEGER` millisecond timestamps
  and counters become `BIGINT` (ADR 0008 §3); `TEXT` stays `TEXT`; `BLOB` becomes `BYTEA`; `INTEGER` 0/1
  flags (`baseline_done`, `prerelease`, `yanked`, …) become `BOOLEAN` with no CHECK.
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

### 8. Configuration surface (D20)

No new YAML keys. `database.dsn` already accepts `postgres://` and `postgresql://`. TLS (`sslmode`, …), pool
and timeouts are DSN parameters (with `database/sql`, the pool size is set in code, see open questions), and
the password comes from the DSN or `PGPASSWORD`/`.pgpass`. The DSN, and any part of it, is never logged.

### 9. Clock source for leases (D22)

Lease times stay **host times passed through the `store` interface** (`now`, `until`), on both engines. When
a role starts on Postgres, it compares the host clock with `SELECT now()` (minus half the round trip). Above
**10 s** of skew it logs a warning; above **30 s**, the dispatcher margin of ADR 0017 §2, it refuses to start
the `scheduler` and `dispatcher` roles with a clear error. Implemented and tested with an injected clock in
#86. Using the database's `now()` inside the Postgres lease queries (ADR 0009 §2's "should") stays a later
option: it would give the interface arguments a different meaning per engine and break the injected-clock
tests.

**Why D21 and D22 are here and not in a later ADR.** #86 asks for them as an amendment to this ADR, or a
new one if this one was already accepted. They were decided on the same day, and they bound the same design:
D21 fixes that the lease lock of §3 is replaced by `SKIP LOCKED` on the same tables, not by a second queue,
which the lock order of §4 depends on; D22 fixes which clock the fences of §3 compare. #86 implements both
and needs no further decision from this ADR.

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
| D22 (#86) | Clock source for leases | Host clock; start-up skew check, warn above 10 s, refuse `scheduler`/`dispatcher` above 30 s | §9 |

Agent-decided and recorded here: `TEXT` for JSON columns, `BOOLEAN` flags, lists as JSON parameters (§7, §5);
lock keys per schema (§4); `FOR SHARE` on a check's watches (§2).

## Consequences

### Positive

- The pull API's no-skip promise holds on Postgres, with a proof that depends on three checkable facts
  (lock before `nextval`, `CACHE 1`, no unlocked insert path), and the `seq` cursor and the API stay as
  they are.
- Code written for SQLite keeps its guarantees: config, API and secret writes serialize as before, and a
  check never interleaves with a config change to its own watches.
- Check transactions that emit nothing, lease transactions (after #86) and reads run concurrently, which is
  what scaling out needs.
- One driver, pure Go, MIT, and the same `database/sql` structure as the SQLite engine.
- No new configuration surface to freeze before 1.0.

### Negative

- Event-emitting transactions serialize; with many concurrent emitters their throughput is capped at one
  transaction per hold time (measured 2.3–3.3× below unlocked with 4–16 clients in a loaded sandbox).
- A transaction that stalls while holding the event lock (a stuck client between the first event insert and
  its commit) stalls every emitter. Check transactions do no network I/O inside the transaction (ADR 0009
  §5), and an operator can bound a stuck session with `idle_in_transaction_session_timeout` on the server or
  in the DSN's `options`.
- Rare deadlocks between the event lock and row locks are possible (§4). Postgres resolves them in about
  1 s, and the failed check is retried.
- A config transaction must not append events. Nothing in the code enforces this; it is a rule for
  reviewers until a test or the engine does.
- Config, API and secret writes do not run in parallel across replicas. They are rare (file apply at
  start, API calls, rotation).
- Every future migration is written twice, once per engine.

### Open questions

- **Pool size.** With `database/sql`, pgx's `pool_max_conns` DSN parameter does not apply: `stdlib` opens
  `*sql.DB`, whose pool is set by `SetMaxOpenConns` in code. 85-D picks a default; D20 rules out a YAML key,
  so making it configurable later means a DSN parameter the engine reads itself, or a new key decided then.
- **Drift after start.** D22 checks skew once, at start. A clock that drifts later is not detected.
  A periodic re-check, or the database `now()` of D22 (b), is left for later.
- **NUL in event payloads.** A registry string with a NUL character becomes `\u0000` in the payload, and
  the subject index then rejects the event on Postgres (§7), so that check fails on every retry. 85-C or
  85-D should add a test and pick one of: drop or replace NUL when the event is built (applies to both
  engines, changes no stored data today); or keep the subject in its own `TEXT` column (a schema difference
  from SQLite, unless both engines get it). Recommendation: the first.
- **Enforcing "no events in a config transaction".** The engine could return an error from `AppendEvent`
  inside `InConfigTx`/`InAPITx`. Left to 85-D, if the shared suite allows it.
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

## References

- Issues angelia-dev/angelia#85 (D14–D20) and #86 (D21–D23; D23 is an executor choice and is not recorded
  here), epic #18.
- [architecture.md §3.2](../architecture.md#32-scheduler-and-workers), [§5](../architecture.md#5-data-model-simplified),
  [§6](../architecture.md#6-language-and-framework-phase-0-assumptions).
- ADRs [0003](0003-sqlite-default-postgres-ha-no-external-queue.md), [0008](0008-store-history-rows-and-event-cursor.md),
  [0009](0009-scheduler-target-leases-and-backoff.md), [0014](0014-subscription-policy-overrides.md),
  [0015](0015-release-event-outbox-in-house.md), [0017](0017-dispatcher-delivery-leases-and-retries.md),
  [0018](0018-yank-events-reference-the-original.md), [0022](0022-rest-api-in-house-spec-driven-validation.md),
  [0024](0024-secrets-encrypted-at-rest.md).
- PostgreSQL documentation: [advisory locks](https://www.postgresql.org/docs/17/explicit-locking.html#ADVISORY-LOCKS),
  [sequence functions](https://www.postgresql.org/docs/17/functions-sequence.html) (values are never rolled
  back), [READ COMMITTED and concurrent updates](https://www.postgresql.org/docs/17/transaction-iso.html#XACT-READ-COMMITTED),
  [row-level lock modes](https://www.postgresql.org/docs/17/explicit-locking.html#LOCKING-ROWS).
