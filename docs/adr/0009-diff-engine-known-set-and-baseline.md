# ADR 0009: The known set never shrinks; baseline is per watch; backports are classified against the merged set

- **Status:** Proposed
- **Date:** 2026-11-01
- **Story:** angelia-dev/angelia#56

## Context

The diff engine compares a target's known version set (`version` table) with what a source just
listed and reports new versions, including backports, yank transitions and disappearances
([architecture.md §3.4](../architecture.md#34-normalizer-and-policy-engine)). The first check of a
watch records a baseline without emitting ([prd.md §6](../prd.md#6-functional-requirements) F4).
Several watches share one target, and a check must be idempotent: running it twice on the same
input gives no changes the second time. Upstream data is untrusted and registries sometimes answer
with partial or empty lists.

## Decision

Encoded in `internal/diff` (`Compute` is pure, `Apply` runs inside `store.Tx`) and in migration
`internal/store/migrations/sqlite/00003_version_removed_at.sql`.

1. **The known set never shrinks.** A known version that a non-empty fetch no longer lists gets
   `version.removed_at` set and is reported once as `removed`, logged only (no event in Phase 0).
   If it is listed again, `removed_at` is cleared and it is reported as `returned` (logged only),
   never as `new`. Without the column, a removal would be reported on every check.
2. **An empty fetch changes nothing** (no removals, WARN logged when the known set is not empty).
   A registry glitch returning `[]` is far more likely than every version being deleted.
3. **Yanks.** A yank flag that differs from the known row is a `yanked` or `unyanked` change. A
   version first seen already yanked is a `new` change with `Yanked` set; the policy engine
   (#57) decides whether it emits.
4. **Baseline is per watch**, not per target: `Apply` marks `watch.baseline_done` on the first
   successful check of every watch that lacks it, and gives that watch no changes, while other
   watches on the same target get theirs. A `NotModified` fetch also counts (the known set is
   current). An empty first fetch is a valid baseline: the package's first release is then
   reported.
5. **Previous version and bump.** For each new version, `Previous` is the greatest version strictly
   below it in the **union of the known set and this fetch's new versions** (yanked and removed
   included), and `Bump = scheme.Bump(previous, new)` as ADR 0007 requires. Several new versions
   at once are therefore classified as if seen one by one (1.0.1 patch, 1.1.0 minor, 2.0.0 major),
   independently of poll timing. With no previous version, `Previous` and `Bump` are empty.
6. **Backport** is true when a version known *before* this check is greater than the new one
   (1.9.4 after 2.0.0). Versions that arrive in the same fetch do not make each other backports.
7. **Order.** Changes are grouped by kind (new, yanked, unyanked, removed, returned), ascending by
   scheme order within a kind, so events are emitted oldest first.
8. **Unit of work.** `Apply` reads the known set and the watches through `store.Tx`, so the fetch
   loop commits the known set, baselines, events, deliveries and fetch state together (ADR 0004).
   For this, `store.Tx` gains `ListVersions`, `ListWatches`, `UpdateVersion` and
   `MarkBaselineDone`.

## Consequences

### Positive

- Re-running a check is a no-op; flapping registry answers do not create duplicate `new` events.
- A watch added to a busy target does not replay the target's history.

### Negative

- `version` rows are never deleted by the diff engine; a target that publishes many builds grows
  without bound (retention of `version` is not planned, architecture.md §5).
- A new version with no previous one has an empty `bump`; the event schema must allow it or the
  event mapping (#106) must choose a value.

### Open questions

- Whether `release.yanked` should also fire for a version yanked before it was ever emitted, and
  whether un-yank gets an event type, is left to #112.
- Postgres (#85) needs the same `removed_at` column in its first migration.

## Alternatives considered

- **Delete removed versions from the known set.** A version that comes back would be reported as
  new, and a flapping registry would emit duplicates.
- **Report removals on every check.** Breaks idempotency and floods the log.
- **Previous = latest known version.** ADR 0007 rules it out: a backport would be `other`.
- **Previous from the known set only.** Several new versions at once would all be classified
  against the same old version (1.0.0 → 2.0.0 major, 1.0.0 → 1.1.0 minor), so the result would
  depend on how often the target is polled.

## References

- [ADR 0004](0004-cloudevents-purl-event-contract.md), [ADR 0007](0007-version-schemes-in-house-with-lexical-fallback.md),
  [ADR 0008](0008-store-history-rows-and-event-cursor.md).
- Stories #56 (this), #57 (policy), #106 (event mapping and outbox), #112 (yank events).
