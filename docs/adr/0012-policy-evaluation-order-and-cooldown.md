# ADR 0012: Policy runs in a fixed order after the diff; cooldown holds versions in a table and re-checks them on every check

- **Status:** Proposed
- **Date:** 2026-11-01
- **Story:** angelia-dev/angelia#57

## Context

Each watch has a policy ([architecture.md §3.4](../architecture.md#34-normalizer-and-policy-engine),
[prd.md §6](../prd.md#6-functional-requirements) F4): `include` / `exclude` regex, `prerelease`,
`constraint`, `bump` and `cooldown` ("emit only once the version is older than N, and drop it if
yanked in between"). The key names are those of the watch file (#48, `docs/watch-file.md` §2.3),
stored in `watch.policy_json` with the watch's `scheme`. The diff engine (ADR 0010) gives each watch
its changes classified with the watch's scheme and leaves alias suppression to the policy. The event
example in architecture.md §4 carries `cooldown_satisfied_at` equal to `published_at` + 72 h. A held
version must survive restarts, and the check is one transaction (ADR 0004).

## Decision

Encoded in `internal/policy` (`Decode`, `Compile`, `Engine.Evaluate`, `Apply`, `TrustsPublishedAt`),
migration `internal/store/migrations/sqlite/00005_pending_release.sql` (00004 is taken by the
watch-file apply, PR #132) and the `store.Tx` methods `ListPendingReleases`, `AddPendingRelease`,
`DeletePendingRelease`, `DeletePendingReleasesOfWatch`. User-facing rules are in
[policy.md](../policy.md).

1. **Order.** Held versions are re-checked first. Then, for each new version: yanked → drop; alias →
   drop; `include`; `exclude`; `prerelease`; `constraint`; `bump`; `cooldown` (emit or hold). The first
   failing step is the logged reason. Yank, un-yank, removal and return are not candidates.
2. **Aliases are dropped**: a new version whose `Previous` has the same precedence (diff sets
   `Bump = other` in that case, ADR 0010 §8) **and** whose `Previous` passes this watch's `include`
   and `exclude` (raw-string) filters. If the watch filters the `Previous` out, it never announced it,
   so the new version is not a duplicate for that watch (`include: ['^v']` with `1.2.0` then
   `v1.2.0`). There is no key to keep aliases; one release is announced once. In one fetch, the first
   listed entry wins. Only the raw filters are consulted: the `prerelease`, `constraint` and `bump`
   results of two equal-precedence versions are the same.
3. **Regexes match the raw string**, unanchored (RE2). Exclude wins over include.
4. **Constraint** syntax is scheme-independent (`>=`, `>`, `<=`, `<`, `==`/`=`, `!=`, bare version;
   space or comma = AND; `||` = OR); operands are parsed with the watch's scheme, completed with `.0`
   at most twice when the scheme rejects them. Matching is the scheme's precedence only; a lexical
   fallback version never matches a non-empty constraint. No `^`, `~` or `x` ranges in Phase 0.
5. **Bump filter** keeps only listed values (`major`, `minor`, `patch`); an empty bump (no previous
   version) or `prerelease`/`other` never passes a non-empty filter.
6. **Cooldown start** (security; **pending maintainer confirmation**, this is the recommended option
   and may change) is the detection time (`detected_at`) by default. It is the upstream publish date,
   when known and not later than the detection time, only for source kinds whose date is assigned by
   the registry at upload and cannot be back-dated by the publisher: `pypi`, `npm`, `crates`,
   `github-release` (`policy.TrustsPublishedAt`, passed to `Apply` as `Input.TrustPublishedAt`).
   `github-tag` (committer date, chosen by whoever pushes), `oci` (the image's `created` field),
   plugins and unknown kinds count from detection, so a back-dated tag cannot skip the cooldown. A new
   kind counts from detection until it is reviewed and added to the table. `cooldown_satisfied_at` =
   start + cooldown, set only when the cooldown is not zero.
7. **Held versions live in `pending_release`** (`watch_id`, `target_id`, `raw`, `held_at`,
   `release_json`), keyed by (`watch_id`, `target_id`, `raw`), deleted with the watch or the version
   (cascade). The target is part of the key so that rows a watch left on its previous target never
   conflict with the same raw version on its new target (which would fail every check of the new
   target). The watch-file apply (#48, PR #132) must call `DeletePendingReleasesOfWatch` in its
   transaction when it moves a watch to another target.
   `release_json` is a snapshot of the version as detected (`policy.Release`: classification, dates,
   URLs, metadata), so the event built at release time describes the version as it was found.
8. **Re-evaluated on every successful check** of the target, `304 Not Modified` included, inside the
   check's transaction, after `diff.Apply` has updated the known set. `ListPendingReleases` joins
   the known set, so the yank and removal state is the current one, not a transition seen in this
   check. A held version that is yanked is dropped for good; one that is no longer listed is kept
   but not emitted; one that the **current** filters reject is dropped; one older than the **current**
   cooldown is emitted.
9. **An invalid stored policy skips the watch** for that check (ERROR log); it does not fail the
   transaction, so other watches of the target still get their events.
10. **Output.** `policy.Apply` returns `[]WatchReleases`; turning a `Release` into a CloudEvent and
    deliveries is the event-mapping story (#106), in the same transaction.

## Consequences

### Positive

- A held version is a row, so restarts, leases moving to another node and policy edits are handled
  without any in-memory timer. The scheduler needs no change.
- A yank during the window is never missed between checks as long as it is still visible at one check.
- The event for a released version is identical whichever check releases it.
- Changing the filters or the cooldown applies to held versions at the next check.

### Negative

- A new table and four `store.Tx` methods; Postgres (#85) needs the same table.
- Emission is up to one check interval after the cooldown ends (`cooldown_satisfied_at` is exact).
  A target whose checks keep failing releases nothing.
- A yank and un-yank both between two checks is not seen.
- A version first seen yanked never emits, even if un-yanked later.
- Held rows of a watch moved to another target (#48 D7) are ignored by the query; they stay until the
  watch is deleted unless the watch-file apply calls `DeletePendingReleasesOfWatch` (required of
  PR #132). If it does not, they come back if the watch moves back to the old target.
- Versions from `github-tag`, `oci` and plugin sources always wait the full cooldown after
  detection, even when their date is genuine.
- `baseline: false` is decoded but not honoured: the diff engine always baselines.

### Open questions

- **Cooldown start** (§6) awaits the maintainer's decision; the table of trusted kinds may change.
- Whether removal upstream while held should drop the version (like a yank) instead of keeping it.
  As implemented, a version removed while held and never listed again keeps its row (a leak) until
  the watch or the version is deleted; the known set never shrinks (ADR 0010), so the version row
  does not go away on its own. A follow-up could drop rows removed for longer than the cooldown.
- Validation of `constraint` when a watch file is applied (#48): `policy.Decode` checks the syntax,
  but the operands depend on the watch's scheme (`Compile`). The watch-file apply should call
  `Compile` with the watch's scheme so that `>=latest` is rejected when stored, not at the first
  check (follow-up issue to file).
- Whether pre-releases should be excluded from `<X` when X is a release (PEP 440 and npm do this).
- Field names of the event (`cooldown_satisfied_at`, …) are the maintainer's (ADR 0004); `Release`
  uses the architecture example's names.

## Alternatives considered

- **No table: release versions whose maturity time falls between the previous and the current
  check.** No state, but it cannot remember a yank that was later undone, depends on
  `last_checked_at` being exact, and misses versions when a check is skipped.
- **Store `due_at` instead of the snapshot's dates.** Simpler query, but a cooldown change would not
  apply to held versions.
- **Re-classify held versions at release time** from the known set. The previous version could
  change between detection and release, and URLs are not in the known set.
- **Fail the check on an invalid policy.** One bad watch would block every watch on its target.
- **A `keep_aliases` key.** Not in the watch file's keys; aliases are duplicates for every consumer we
  know of.

## References

- [ADR 0004](0004-cloudevents-purl-event-contract.md), [ADR 0007](0007-version-schemes-in-house-with-lexical-fallback.md),
  [ADR 0010](0010-diff-engine-known-set-and-baseline.md).
- Stories #48 (watch file), #56 (diff), #57 (this), #85 (Postgres), #106 (event mapping), #112 (yank events).
