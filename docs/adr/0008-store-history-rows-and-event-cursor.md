# ADR 0008: Store history rows have no foreign key to config; the event cursor is a commit-order sequence

- **Status:** Proposed
- **Date:** 2026-11-01
- **Story:** angelia-dev/angelia#46

## Context

The SQLite store (#46) implements the data model of
[architecture.md §5](../architecture.md#5-data-model-simplified). Two questions came up in review
and the maintainer answered them on PR #128:

- With foreign keys from `event` to `watch` and from `delivery` to `subscription`, a watch or
  subscription cannot be deleted while its events or deliveries exist, which is up to 90 days of
  retention. Config-as-code (`angelia apply`) must be able to remove them at any time.
- [ADR 0004](0004-cloudevents-purl-event-contract.md) used the event ULID as the pull-API cursor.
  ULIDs are minted before the transaction commits, so with several writers a smaller ULID can commit
  after a reader has paged past a larger one, and that reader skips the event.

## Decision

1. **History rows keep config IDs without a foreign key.** `event.watch_id` and
   `delivery.subscription_id` have no foreign key; a delivery reaches its sink through the
   subscription, so deleting a sink is blocked only by subscriptions that use it. Deleting a watch,
   subscription or sink leaves events and deliveries untouched. Foreign keys remain between config
   tables (`watch → target`, `version → target`, `subscription → sink`) and from `delivery` to
   `event`. Encoded in `internal/store/migrations/sqlite/00001_initial_schema.sql` and
   `store.Store.Delete*`.
2. **The event cursor is `event.seq`,** an integer the store assigns at insert. On SQLite it is
   `INTEGER PRIMARY KEY AUTOINCREMENT`: writes are serialized, so insert order is commit order, and
   values are not reused after retention deletes the newest rows. `ListEventsAfter(afterSeq, limit)`
   orders and pages by it. The ULID stays the public event id (CloudEvents `id`, idempotency key).
3. **Timestamps** are unix milliseconds in UTC: `INTEGER` on SQLite, `BIGINT` on Postgres.
4. **Delivery statuses** are `pending`, `in_flight`, `delivered` and `dead`, enforced by a CHECK.

## Consequences

### Positive

- Removing config never fails because of history, and never rewrites the immutable log.
- A pull consumer that resumes from its last `seq` sees every committed event exactly in commit order.

### Negative

- The store no longer checks that a new event names an existing watch, or a new delivery an
  existing subscription; the writer (scheduler, dispatcher) must.
- Readers of history must tolerate a `watch_id` or `subscription_id` whose row is gone (the API
  and the dispatcher stories decide how to show or handle it).
- The pull API exposes a second identifier (`seq`) next to the event id.

### Open questions

- Postgres (Phase 2): a sequence is assigned at insert, not at commit, so concurrent transactions
  can still commit out of `seq` order. The Postgres engine needs a commit-ordered cursor (for
  example a single-writer append lock, or reading only below the oldest in-flight transaction).
- The OpenAPI `after` parameter of `GET /v1/events` must carry `seq` (spec repository).

## Alternatives considered

- **Soft delete of config rows.** Keeps the foreign keys but every config query must filter
  deleted rows, and names cannot be reused without extra unique-index care.
- **`ON DELETE SET NULL`.** Rewrites rows of an immutable log and loses which watch an event came from.
- **ULID cursor.** Skips events under concurrent writers, as above.
- **Implicit `rowid` as cursor.** Not stable: `VACUUM` may renumber rowids of a table without an
  `INTEGER PRIMARY KEY`.

## References

- PR angelia-dev/angelia#128 (maintainer's answers), story #46.
- [architecture.md §3.1, §5](../architecture.md#31-api), [ADR 0004](0004-cloudevents-purl-event-contract.md).

## Amendment (2026-11-04, #186)

The Postgres question under "Open questions" is still open: no commit-ordered cursor is chosen yet, so the no-skip guarantee above holds on SQLite only. The pull API relies on it: its `after` cursor and `next_after` are `seq` (OpenAPI spec, `docs/api.md`, item 11), so the API's no-skip promise is not yet met on Postgres.
