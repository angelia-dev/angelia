# ADR 0018: A yank of an emitted version is a `release.yanked` event that repeats the stored payload and references the original event in the store

- **Status:** Proposed
- **Date:** 2026-11-02
- **Story:** angelia-dev/angelia#112

ADR 0017 is reserved for the dispatcher (PR #169).

## Context

Story #112 turns yank transitions into `release.yanked` events and checks that pre-releases are
`release.prerelease` events. The contract is the spec's release event v1
(angelia-dev/spec `docs/release-event-v1.md`): `release.yanked` is emitted when "a version **this
watch has emitted** becomes yanked upstream, detected by a later check. One event per watch per
yank"; it "repeats the classification of the event the watch emitted for that version ... read from
the stored earlier payload", with `yanked: true`, `cooldown_satisfied_at: null` and `detected_at` the
check that saw the yank. Not events in v1: a version yanked while held, a version first seen yanked,
un-yank, removal and return.

The maintainer's rules on the issue (2026-11-01) add: `policy.*` on a subscription does not apply to
yanked events (also ADR 0014 decision 5), and a yank cancels held deliveries of that version (ADR
0014 section 4, columns `hold_watch_id` and `hold_raw`).

What already exists:

- `diff.Apply` reports a yank of a known version as a `diff.KindYanked` change to every watch on the
  target that has its baseline (ADR 0010), once: the known set records the new state.
- `policy.Apply` drops a version held for the watch cooldown when it is yanked (ADR 0012); it never
  emitted, so no event.
- `event.Build` already maps a pre-release to `release.prerelease` and a final release to
  `release.published` (#106), and the subscription filter's `type` key already accepts the three
  types (#69).
- Events are immutable rows with the envelope in `payload_json` (ADR 0015).

The acceptance criterion says the yank "references the original event id". The v1 payload has no
field for it, and v1 allows no extension attribute from the producer.

## Decision

1. **Where.** `event.EmitYanks` runs in the check's transaction, after `Emit`, from `event.Commit`.
   It reads the per-watch changes of `diff.Apply`; `policy.Apply` is unchanged.
2. **Which versions.** For each `KindYanked` change of a watch, the event the watch emitted for the
   version is the latest `release.published` or `release.prerelease` row of that watch whose
   CloudEvents `subject` (`<kind>:<package>@<raw>`) is the version's on the watch's current target
   (`store.Tx.GetLatestEventBySubject`). The subject, not only the raw version, so that a watch moved
   to another target does not yank a version of its previous package. No such event (baseline
   versions, versions the policy dropped, versions yanked while held, versions emitted under another
   target): no event, an Info log.
3. **Payload.** `event.BuildYank` copies the stored payload of that event and changes only: a new
   `id`; `type` `dev.angelia.release.yanked.v1`; `time` and `data.detected_at` the check time;
   `data.yanked` true; `data.cooldown_satisfied_at` null; envelope `source` and `data.watch` from the
   current instance name and watch (so filters match the labels the watch has now). Classification,
   URLs, `published_at`, `notes`, `purl`, `subject`, `data.source` and `data.package` are the
   original's. The result validates against the vendored spec schemas and equals the spec's
   `yanked-pypi.json` example built from the same release (test).
4. **Reference to the original event.** In the store, not in the payload: migration
   **`00009_event_yank_ref.sql`** adds `event.ref_event_id` (TEXT, NULL; no foreign key, like every
   history column, ADR 0008), exposed as `store.Event.RefEventID`, set on every `release.yanked` row.
   The same migration adds the expression index `event_watch_subject (watch_id,
   json_extract(payload_json, '$.subject'))` for the lookup of decision 2; no row is rewritten, so the
   immutability trigger of 00007 stays as it is. A test checks that the query plan uses the index.
5. **Routing.** One delivery per subscription whose filter matches (`type` `release.yanked`, the
   stored `bump`, the current labels); `subscription.Policy.Evaluate` admits yanks unconditionally, so
   `policy.prerelease` and `policy.cooldown` do not drop or hold them. A subscription that does not
   decode gets a dead delivery, as in `Emit` (ADR 0015 decision 2). `Emit` and `EmitYanks` share this
   step (`compiled.route`).
6. **Held deliveries.** Before the event is looked up, `store.Tx.CancelHeldDeliveries(watch, raw,
   now)` deletes the deliveries with that `hold_watch_id` and `hold_raw` that are still held
   (pending, `held_until` after now), as ADR 0014 section 4 says ("delete"). A hold that already
   ended is an ordinary queued delivery and is sent. The original event stays.
7. **One event per watch per yank.** Every watch that emitted the version gets its own event,
   referencing its own original. The diff reports a yank once, so later checks add nothing. A version
   un-yanked (logged at Info, no event in v1) and yanked again is a second yank and emits a second
   `release.yanked`, referencing the same original.
8. **Pre-releases.** Unchanged code (`event.Build`): a pre-release passes the watch only when its
   `prerelease` mode is `include` or `only` and is then `release.prerelease`, never
   `release.published`. #112 adds end-to-end tests for the three modes and for type filters.

## Consequences

### Positive

- The yank says exactly what subscribers were told, read from the immutable log, with no second
  classification that could disagree.
- The link to the original is queryable without parsing payloads; nothing in the public contract
  changes.
- No new table; one additive migration; the dispatcher is untouched.

### Negative

- A consumer of the event cannot see the original event id; it has to correlate on `subject` +
  `type` or `data.purl` (the spec's deduplication advice). See the open questions.
- When retention prunes the original event, a later yank of that version finds nothing and emits no
  event (Info log). Retention defaults to 90 days.
- A subscription whose held delivery a yank cancelled still receives the `release.yanked` event of a
  version it was never told about (policy does not apply to yanks).
- The expression index and the `json_extract` lookup are SQLite syntax; the Postgres port needs the
  `jsonb` equivalent (`(payload_json->>'subject')`).

### Open questions

- **Original event id in the payload.** The AC says "referencing the original event id". The v1
  payload has no such field. Options: (a) store only (this ADR); (b) an additive optional
  `data.yanked_event_id` (or similar) in a spec minor release, which needs a spec PR and a maintainer
  decision on the name; (c) a CloudEvents extension attribute, which v1 forbids producers to emit.
  Recommendation: (a) now, (b) if a consumer asks for it.
- **Re-yank after un-yank.** Decision 7 emits again. The alternative, at most one `release.yanked`
  per watch and version ever, needs a lookup of `ref_event_id`; it is a one-line change if preferred.
- **Cancelled holds and the yank.** Decision 5 sends the yank to a subscription whose held delivery
  was cancelled. The alternative is to skip those subscriptions, which makes `policy.cooldown`
  influence yank routing, against the maintainer's rule.

## Alternatives considered

- **A `subject` or `version` column on `event`.** Faster to read, but filling it for existing rows
  rewrites immutable rows (the trigger of 00007 would have to be dropped and re-created). The
  expression index gives the same lookup with no rewrite.
- **Look the version up in `pending_release` or the known set instead of the event log.** The spec
  says the classification is the stored payload's, and only the event log knows which watch emitted
  what.
- **Mark cancelled held deliveries `dead` with a reason** (like a removed subscription) instead of
  deleting them. Keeps them visible, but ADR 0014 section 4 says delete and the partial index stays
  smaller. Easy to switch if the maintainer prefers visibility.

## References

- angelia-dev/spec at 68e022e9d85cc8351fcc5cac29094af369cb8102, `docs/release-event-v1.md`
- [ADR 0008](0008-store-history-rows-and-event-cursor.md), [ADR 0010](0010-diff-engine-known-set-and-baseline.md),
  [ADR 0012](0012-policy-evaluation-order-and-cooldown.md), [ADR 0014](0014-subscription-policy-overrides.md),
  [ADR 0015](0015-release-event-outbox-in-house.md)
- `internal/event/yank.go`, migration `00009_event_yank_ref.sql`
- Issue #112 and its comment of 2026-11-01 on yank rules
