# ADR 0010: The known set never shrinks and is scheme-neutral; baseline is per watch; changes are classified per scheme

- **Status:** Proposed
- **Date:** 2026-11-01
- **Story:** angelia-dev/angelia#56

## Context

The diff engine compares a target's known version set (`version` table) with what a source just
listed and reports new versions, including backports, yank transitions and disappearances
([architecture.md §3.4](../architecture.md#34-normalizer-and-policy-engine)). The first check of a
watch records a baseline without emitting ([prd.md §6](../prd.md#6-functional-requirements) F4).
Several watches share one target (one fetch per target), but the version scheme is chosen **per
watch** (`schemes.Select(kind, override, logger)`, ADR 0007, #68): one watch on a GitHub tag may say
`scheme: calver` while another keeps the default. A check must be idempotent: running it twice on
the same input gives no changes the second time. Upstream data is untrusted and registries
sometimes answer with partial or empty lists.

## Decision

Encoded in `internal/diff` (`Compute` and `Result.Classify` are pure, `Apply` runs inside
`store.Tx`) and in migration `internal/store/migrations/sqlite/00003_version_removed_at.sql`.

1. **The known set never shrinks.** A known version that a fetch with at least one usable entry no
   longer lists gets `version.removed_at` set and is reported once as `removed`. If it is listed
   again, `removed_at` is cleared and it is reported as `returned`, never as `new`. Without the
   column, a removal would be reported on every check. Removal detection assumes `Source.List`
   returns the **complete** list of versions; removals and returns are only logged (no event in
   Phase 0). A `Complete` flag on `source.Result` can be added later for sources that page or
   truncate.
2. **A fetch without usable entries changes nothing** (no removals; a WARN when the known set is
   not empty). Usable means non-blank, without leading or trailing white space, not repeated.
   A registry glitch returning `[]` is far more likely than every version being deleted.
3. **Raw strings as given.** The known set is keyed by the raw upstream string (ADR 0007). Raw
   strings with leading or trailing white space are skipped and counted, not trimmed:
   normalisation is the source's job. When a raw string is repeated, the first entry wins (sources
   list newest first), even if a later copy carries a different yank flag.
4. **Yanks.** A yank flag that differs from the known row is a `yanked` or `unyanked` change. A
   version first seen already yanked is a `new` change with `Yanked` set; the policy engine
   (#57) decides whether it emits.
5. **Baseline.**
   - Per watch, not per target: `Apply` marks `watch.baseline_done` on the first successful check
     of every watch that lacks it, and gives that watch no changes, while other watches on the
     same target get theirs. A `NotModified` fetch counts too (the known set is current).
   - **First fill of an empty known set:** when the known set was empty before the check and the
     fetch adds more than one version, the check is a baseline for **every** watch, including
     watches whose baseline is already done. So a target whose first fetch was empty (a glitch, or
     a package not published yet) does not replay its whole history on the next check. A single
     first version is still reported.
   - The concern with this rule: a brand-new package that publishes two versions in the same poll
     interval right after being watched is not announced. We accept it as much rarer than a
     transient empty answer.
6. **The known set is scheme-neutral.** `Normalized` and `Prerelease` of new rows are written with
   the source kind's **default** scheme (`schemes.Select(kind, "", logger)`), never with a watch's
   override, so stored rows never depend on which watches exist.
7. **Classification is per scheme.** `Apply` classifies new versions (`Previous`, `Bump`,
   `Backport`, and the scheme's opinion on pre-release when the source gives none) **once per
   distinct scheme** among the watches (`Input.SchemeFor`), and gives each watch the classification
   for its scheme, in that scheme's order. Within a target, schemes are told apart by name:
   `Select` gives one configuration per name and source kind. `removed`, `yanked`, `unyanked` and
   `returned` are the same for every scheme.
8. **Previous version and bump**, for each new version and its classifying scheme:
   - If an entry of **equal precedence** exists, `Previous` is that entry and `Bump` is therefore
     `other`. A known entry wins; otherwise the earliest entry of the same fetch. Examples: `v1.2.0`
     after `1.2.0`, `1.0.0` after `1.0` in PEP 440, `1.2.0+b` after `1.2.0+a`. Whether such aliases
     are suppressed is the policy engine's job (#57).
   - Otherwise `Previous` is the greatest version strictly below it in the **union of the known set
     and this fetch's new versions** (yanked and removed included), and
     `Bump = scheme.Bump(previous, new)` as ADR 0007 requires. Several new versions at once are
     classified as if seen one by one (1.0.1 patch, 1.1.0 minor, 2.0.0 major), whatever the poll
     timing.
   - Only entries parsed by the **same scheme** as the new version are considered. A lexical
     fallback (`latest`, a four-part tag under SemVer) is never compared with a scheme version, in
     either direction. With no candidate, `Previous` and `Bump` are empty.
9. **Backport** is true when a version known before this check, **not removed**, is greater than the
   new one, and both were parsed by the classifying scheme's primary parser (1.9.4 after 2.0.0).
   Removed versions are not a reason to call something a backport, but they can still be its
   `Previous`. Versions that arrive in the same fetch do not make each other backports.
10. **Order.** Changes are grouped by kind (new, yanked, unyanked, removed, returned), ascending by
    scheme order within a kind, so events are emitted oldest first.
11. **Unit of work.** `Apply` reads the known set and the watches through `store.Tx`, so the fetch
    loop commits the known set, baselines, events, deliveries and fetch state together (ADR 0004).
    For this, `store.Tx` gains `ListVersions`, `ListWatches`, `UpdateVersion` and
    `MarkBaselineDone`. If `AddVersion` reports a version as already known (a concurrent check),
    `Apply` fails with `store.ErrConflict` so the transaction rolls back.

## Consequences

### Positive

- Re-running a check is a no-op; flapping registry answers do not create duplicate `new` events.
- A watch added to a busy target does not replay the target's history, nor does a target whose
  first answer was empty.
- Watches with different schemes on one target each get a consistent classification, and the stored
  rows don't change when an override is added or removed.

### Negative

- `version` rows are never deleted by the diff engine; a target that publishes many builds grows
  without bound (retention of `version` is not planned, architecture.md §5).
- A new version with no previous one has an empty `bump`; the event schema must allow it or the
  event mapping (#106) must choose a value.
- Classification costs one sort of the known set per distinct scheme on a target per check.
- `Prerelease` in the known set is the default scheme's opinion; a calver watch's opinion is only
  in its `Change`.

### Open questions

- Whether `release.yanked` should also fire for a version yanked before it was ever emitted, and
  whether un-yank gets an event type, is left to #112.
- Postgres (#85) needs the same `removed_at` column in its first migration.
- Sources that cannot list everything need the `Complete` flag before removals are trusted.

## Alternatives considered

- **Delete removed versions from the known set.** A version that comes back would be reported as
  new, and a flapping registry would emit duplicates.
- **Report removals on every check.** Breaks idempotency and floods the log.
- **One scheme per target.** Simpler, but contradicts the per-watch override (#68) and would make
  stored rows depend on watch config.
- **Previous = latest known version.** ADR 0007 rules it out: a backport would be `other`.
- **Previous from the known set only.** Several new versions at once would all be classified
  against the same old version, so the result would depend on how often the target is polled.
- **Ignore equal-precedence entries when choosing Previous.** `v1.2.0` next to `1.2.0` would be
  classified as a real `minor` or `patch` release and emitted twice.
- **Empty first fetch keeps per-watch baseline only** (option (a)). The next non-empty answer would
  emit the whole history.

## References

- [ADR 0004](0004-cloudevents-purl-event-contract.md), [ADR 0007](0007-version-schemes-in-house-with-lexical-fallback.md),
  [ADR 0008](0008-store-history-rows-and-event-cursor.md).
- Stories #56 (this), #57 (policy), #68 (calver and override), #106 (event mapping and outbox),
  #112 (yank events).
