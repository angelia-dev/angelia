# ADR 0014: Subscriptions narrow the watch policy with `bump`, `prerelease` and `cooldown`, evaluated at match time; a per-subscription cooldown is a held delivery

- **Status:** Proposed
- **Date:** 2026-11-01
- **Story:** angelia-dev/angelia#69

The maintainer answered the open questions on PR #162 (section "Maintainer decisions" below). The
implementation is in the same PR; where the decisions changed a section, the section says so. ADR 0013
is reserved for #60.

## Context

Two consumers of one watch can want different policies: CI wants every patch, chat only minors after
72 h ([architecture.md §3.4](../architecture.md#34-normalizer-and-policy-engine): policy "per watch or
subscription"). Today the watch policy (ADR 0012, [policy.md](../policy.md)) decides which versions
become events and when. A subscription has only a `filter` ([watch-file.md §2.5](../watch-file.md))
that selects events: `source`, `labels`, `bump`, `package`, `type`. Nothing on a subscription defers
or drops by pre-release status, and nothing holds a delivery.

Facts that shape the design:

- A subscription is not tied to a watch. Its filter selects events from every watch that matches
  `source`, `labels` and `package`. So **at load time we do not know which watch policy a subscription
  narrows**, and a "cannot widen" check against one watch is impossible in general.
- The event is created, and the deliveries for the matching subscriptions with it, in the check's
  transaction (#106). By then the watch policy has already admitted the version.
- `delivery` already has `status` (`pending`, `in_flight`, `delivered`, `dead`), `next_attempt_at`, and
  no foreign key to the subscription (ADR 0008). The dispatcher leases pending rows whose
  `next_attempt_at` has passed.
- `filter.bump` already keeps only listed bumps (`major`, `minor`, `patch`), with the same meaning as
  the watch's `bump`.

## Decision

### 1. Keys

`bump` is **not** new: `filter.bump` stays the way to say it, and is documented as a narrowing of the
watch policy. Two keys are added, in a new `policy` block of the subscription (the same word and key
names as `watches[].policy`, so the mental model is one):

```yaml
subscriptions:
  - name: ci-everything
    sink: ci
    filter: { labels: { team: platform } }        # inherits the watch policy as is

  - name: chat-minors
    sink: chat
    filter:
      labels: { team: platform }
      bump: [minor]                               # narrows the watch's bump
    policy:
      prerelease: none                            # narrows the watch's prerelease
      cooldown: 72h                               # at least the watch's cooldown
```

| Key | Type | Default | Validation |
|---|---|---|---|
| `policy.prerelease` | string | absent = inherit the watch | `none`, `only` or `include`; `include` is accepted and narrows nothing |
| `policy.cooldown` | duration | absent = `0` (no extra hold) | Go syntax; clamped to `0` to `8760h` and cut to whole seconds (not an error, decision 2) |
| `filter.bump` | list | any | unchanged: `major`, `minor`, `patch` |

Unknown keys under `policy` are errors, as everywhere in the file. A subscription's `policy` is
stored as `subscription.policy_json` (defaults filled, so the no-op comparison stays exact), and
`filter_json` is unchanged. The default differs from the watch's: the watch defaults to
`prerelease: none`, a subscription defaults to "inherit", because defaulting it to `none` would be a
narrowing nobody wrote.

### 2. "Narrow only"

A subscription sees the events its watch policy admitted, then applies its own rules. That makes
widening impossible by construction, and defines each key as an intersection:

| Key | Effective rule | Result of a "widening" value |
|---|---|---|
| `bump` | version passes the watch's `bump` **and** the subscription's | listing `major` where the watch allows only `minor`: `major` never arrives (the watch dropped it) |
| `prerelease` | the admitted set is intersected: watch `include` admits both, `none` finals only, `only` pre-releases only. Sub `include` keeps what the watch admitted; `none` or `only` keeps the matching part | `include` on a watch with `none`: no pre-release (the watch dropped them). `only` on a watch with `none`: the intersection is empty, so nothing is delivered |
| `cooldown` | measured from the **same start** as the watch's (ADR 0012 §6), not added to it. Effective hold = `max(watch, subscription)` | a value below the watch's cooldown changes nothing: the version is already that old when the event exists |

Ordering for `prerelease` is by the set each mode admits, not a scale: `include` is the widest;
`none` and `only` are disjoint and both narrower than `include`. A subscription value is valid under
a watch when its set is a subset of the watch's.

**Recommendation: clamp, never fail.** Because a subscription spans watches, a value that is a no-op
(or empty) for one watch is right for another (`prerelease: none` is a no-op on a `none` watch and
narrows an `include` watch). A load-time error would reject legitimate files and would have to
re-run whenever any watch changes. So the loader validates syntax only, the effective policy is
always the intersection, and the documentation says "a subscription can only receive less, later".
`angelia config check` may print a **warning** (not an error) when a subscription can be proven
useless against every file-managed watch it matches (for example `prerelease: only` on watches that
are all `none`). The warning is optional for this story.

### 3. Where it is evaluated

At **match time**, in the check's transaction, after the watch policy and in the subscription
matcher (`internal/subscription`), before the delivery rows are written:

1. `Filter.Match` as today, with `bump` now documented as narrowing.
2. `policy.prerelease`: drop if the subscription's mode (or the watch's) excludes this version.
3. Cooldown: `releaseAt = start + max(watch cooldown, policy.cooldown)`, where `start` is the watch
   policy's cooldown start. If `releaseAt` is not after the event's creation time, the delivery is
   created as today. Otherwise the delivery is created **held**.

Steps 2 and 3 are one pure function, `subscription.Policy.Evaluate(event, now)`, returning admit, hold
until T or drop for (watch policy result, subscription policy, event). A `release.yanked` event is always
admitted (decision 5). Calling it from the check's transaction and building the held `store.Delivery`
(`Hold`) is the match step of #106, not this story.

A held delivery is an ordinary `pending` delivery whose `next_attempt_at` is `releaseAt`. The
dispatcher needs no change: it leases pending rows when their time comes, so release is "the
dispatcher reaches `next_attempt_at`", with no timer in memory. Match time (not dispatch time) is
chosen because the dispatcher then stays a pure sender, and the decision uses the same snapshot of the
release as the event.

Editing a subscription **does not touch deliveries that are already queued** (watch-file.md §4, D8:
queued deliveries of a kept subscription are sent). It **does** decide again the **held** ones, that is
the pending deliveries whose `held_until` is still ahead (maintainer decision 6). On a watch-file apply
that changes a subscription's `policy`, every such delivery is recomputed with the new policy from what
its hold recorded:

- the new release time is `start + max(watch cooldown, new subscription cooldown)`; if it has passed,
  the hold is cleared and `next_attempt_at` is now; if it is ahead, `held_until` and `next_attempt_at`
  move to it (earlier or later);
- if the new `policy.prerelease` excludes the version, the delivery becomes `dead` with a reason.

A hold whose time has already passed is just a queued delivery and is not touched. The recomputation
is `watchfile.reevaluateHeld`, one function with one call site, using the same `Policy.Evaluate` as
the match step, so it can be dropped without changing anything else. What it needs is stored on the
delivery (section 4).

A yank while a delivery is held must cancel it, as it does for a version held by the watch's cooldown
("dropped if yanked meanwhile"). The yank handling (#112) calls a store method that removes held
deliveries for the yanked version (section 4). Without it a chat would receive a version the registry
had withdrawn before the chat was told of it.

### 4. Storage

One additive migration, **`00006_subscription_policy_holds.sql`** (next free after 00005; the number
was free on main; there is no Postgres engine yet, which will get the same DDL with `BIGINT`):

```sql
-- +goose Up
ALTER TABLE subscription ADD COLUMN policy_json TEXT NOT NULL DEFAULT '{}'
    CHECK (json_valid(policy_json));
ALTER TABLE delivery ADD COLUMN held_until             INTEGER;  -- NULL = not held
ALTER TABLE delivery ADD COLUMN hold_started_at        INTEGER;  -- the cooldowns count from here
ALTER TABLE delivery ADD COLUMN hold_watch_cooldown_ms INTEGER;  -- the watch cooldown already waited
ALTER TABLE delivery ADD COLUMN hold_prerelease        INTEGER;  -- 1 when the version is a pre-release
ALTER TABLE delivery ADD COLUMN hold_watch_id          TEXT;     -- no FK, like the rest of delivery
ALTER TABLE delivery ADD COLUMN hold_raw               TEXT;     -- raw upstream version
CREATE INDEX delivery_held ON delivery (subscription_id) WHERE held_until IS NOT NULL;
CREATE INDEX delivery_hold_version ON delivery (hold_watch_id, hold_raw) WHERE held_until IS NOT NULL;
```

Nothing is dropped or rewritten; existing rows get `'{}'` and `NULL`. For a held delivery,
`status = 'pending'`, `next_attempt_at = held_until`. `hold_started_at`, `hold_watch_cooldown_ms` and
`hold_prerelease` are what re-evaluation on a subscription edit needs; `hold_watch_id` and `hold_raw`
identify the version for cancellation. `status` is not extended (the public status list stays four values).
A hold ended by the clock keeps `held_until`; a hold ended by an edit is cleared. "Currently held"
means `status = 'pending' AND held_until > now`.

Survival: held deliveries are rows, so a restart, a lease moving to another node or a crash between
checks loses nothing.

Lifecycle:

- **Yank of the version** (#112): delete the watch's held deliveries with `hold_watch_id` and
  `hold_raw` still held.
- **Subscription deleted** (watch-file apply, or the API): **all** its pending deliveries, held or
  not, become `dead` with `last_error = 'subscription removed'` (maintainer decision 4; this amends
  D8 in watch-file.md, which deleted them). In-flight, delivered and dead rows are left alone.
- **Watch re-targeted** (source or coordinates changed; `pending_release` rows are already deleted by
  `DeletePendingReleasesOfWatch`): held deliveries with that `hold_watch_id` are set `dead` with
  `last_error = 'watch re-targeted while held'`; their raw versions belong to another target.
- **Watch deleted**: held deliveries are not touched (history rule, ADR 0008); they are sent when
  due, like the other queued deliveries.
- **Subscription's sink changed or removed**: unchanged rules; a held delivery follows its
  subscription.

The re-target rule is not implemented here: nothing creates held deliveries until #106, and the yank
cancellation belongs to #112; the columns they need are in the migration.

## Consequences

### Positive

- One watch can feed consumers with different policies, with no second watch and no second fetch.
- "Cannot widen" holds by construction; there is no validation that depends on other watches.
- No new table and no dispatcher change; the hold uses the existing lease clock.
- Restarts need no special code.

### Negative

- Cooldown is not re-evaluated at release time: a policy change reaches held deliveries when the
  subscription is edited, but not deliveries already queued.
- A subscription can ask for something useless (`prerelease: only` on a `none` watch) with no error.
- Four new columns on `delivery`/`subscription`; `hold_watch_id` and `hold_raw` duplicate facts held
  in the event payload, to make cancellation an indexed delete.
- `filter.bump` and `policy.*` are in two blocks.

### Resolved questions

1. **Block layout.** Resolved: `bump` stays in `filter`, the new keys are in `policy`, no `policy.bump`
   alias.
2. **Clamp or error.** Resolved: clamp; never fail at load time for a useless value. Syntax errors
   still fail. The optional `config check` warning is not part of this story.
3. **Cooldown start.** Resolved: the same start as the watch's; the effective hold is the max.
4. **#107.** Resolved: on subscription delete, all pending deliveries become `dead` with a reason;
   D8 is amended. The re-target rule stays as proposed and waits for the match step.
5. **Event types.** Resolved: `policy.*` does not apply to `release.yanked`.
6. **Edits and held deliveries.** Resolved as "queued": edits do not touch queued deliveries but
   decide again the held ones (section 3).
7. **Ownership.** Resolved by the scope of #69 below.

### Maintainer decisions (2026-11-01, PR #162)

1. Layout: OK. `bump` stays in `filter`, the new keys go in `policy`, and there is no `policy.bump`
   alias.
2. Clamp useless values. Never fail at load time.
3. Effective cooldown = max(watch, subscription), measured from the same start.
4. On subscription delete, ALL pending deliveries of that subscription become `dead` with a reason,
   not only held ones. This amends D8 in [watch-file.md](../watch-file.md).
5. `policy.*` does not apply to `release.yanked`.
6. Subscription edits do not touch already-queued deliveries (no hold), but do re-evaluate held
   deliveries: on a watch-file apply, every pending delivery of the subscription with `held_until`
   set is recomputed with the new policy, as described in section 3.
7. Scope of #69: the watch-file keys, validation and clamping; migration 00006; the store queries; the
   pure evaluation function the #106 match step calls (admit, hold until T, drop); the held-delivery
   re-evaluation on apply; and dead-on-delete. Wiring it into the event pipeline is #106's job.

## Alternatives considered

- **Reuse `pending_release` with a `subscription_id`.** That table holds a version before any event
  exists, so one version would be held per consumer and the first consumer's event could not be
  emitted before the last's cooldown. Rejected.
- **A `subscription_hold` table** re-evaluated at every check like `pending_release`. Gives live
  re-evaluation and yank handling for free, but duplicates the dispatcher's clock and needs a second
  emit path. Rejected; kept as the fallback if live re-evaluation is required.
- **Evaluate at dispatch time.** Keeps the match simple but makes the dispatcher read policy, version
  and yank state. Rejected.
- **A load-time error for widening values.** Impossible without knowing the watch (section 2).
- **A new `held` delivery status.** Changes the public status list and needs a table rebuild on
  SQLite. Rejected.

## References

- [ADR 0008](0008-store-history-rows-and-event-cursor.md), [ADR 0012](0012-policy-evaluation-order-and-cooldown.md)
- [policy.md](../policy.md), [watch-file.md §2.5 and §4](../watch-file.md)
- `internal/subscription`, `internal/policy`, migration `00005_pending_release.sql`
- Stories #57, #58, #69, #106, #107, #112
