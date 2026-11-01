# ADR 0015: Release events are built in-house as plain structs; the outbox is the delivery rows; events are immutable in the schema

- **Status:** Proposed
- **Date:** 2026-11-01
- **Story:** angelia-dev/angelia#106

## Context

Story #106 turns what a watch policy emits ([ADR 0012](0012-policy-evaluation-order-and-cooldown.md)) into
release events and deliveries, in the check's transaction ([ADR 0004](0004-cloudevents-purl-event-contract.md)).
The contract is the spec repository's release event v1 (angelia-dev/spec#6, `docs/release-event-v1.md`).
[architecture.md §6](../architecture.md#6-language-and-framework-phase-0-assumptions) lists
`cloudevents/sdk-go` and `oklog/ulid/v2`; the issue also names `package-url/packageurl-go`, which §6 does not list.
ADR 0004 left open whether the outbox is a separate table or the `delivery` rows.

## Decision

1. **No new module.** `internal/event` maps a `policy.Release` to a plain `Envelope` struct whose `encoding/json`
   form is the CloudEvents 1.0 structured-mode event. ULIDs come from `internal/ulid` (standard library, the
   generator the watch file already used). purls are built by `event.PURL` from the spec's table:
   `packageurl-go` is not in §6, and the spec deviates from it anyway (go-proxy keeps case; the pypi and crates
   names come from the registry). `cloudevents/sdk-go` can come with a sink that needs its transports.
2. **The outbox is the delivery rows** (no separate table). `event.Commit` runs `diff.Apply`, `policy.Apply` and
   `event.Emit` in one `store.InTx`. `Emit` writes one event per emitted release and one delivery per subscription
   whose filter matches and whose `subscription.Policy.Evaluate` does not drop it; a held delivery carries
   `store.Hold` ([ADR 0014](0014-subscription-policy-overrides.md)). An event no subscription takes is still
   written, for the pull API. A subscription whose stored filter or policy does not decode cannot be matched and
   does not block the others: each event gets a `dead` delivery for it, with `last_error`
   `invalid subscription filter/policy: <error>`, and an Error log, so the problem is visible where deliveries
   are listed. This is a choice the maintainer can revisit (alternatives: skip with a log only, or fail the
   check).
3. **Stored form.** `event.type` holds the full CloudEvents type (`dev.angelia.release.published.v1`) and
   `event.payload_json` the whole envelope, so a replay sends the same bytes, `id` and `time` included.
4. **Immutability.** The store API has no event update; a second append of an id is `ErrConflict`; migration
   00007 adds a trigger that aborts any `UPDATE` of `event`. Retention still deletes rows.
5. **pypi and crates purls.** The pypi source stores `info.name` as `Metadata["project_name"]`, and the crates
   source the index line's `name` (the crate name as published, in its case) as `Metadata["crate_name"]`. When
   it is missing (an old held release, a mirror or index without it) the purl is `null`, which the spec allows,
   instead of a guess from the normalised name.
6. **Schema conformance in tests.** The two spec schemas and the spec's examples are vendored under
   `internal/event/testdata/spec` with their commit. A test-only validator covers the subset of JSON Schema
   2020-12 the schemas use, with ECMA-262 `\s` and `\S` (Unicode white space) in patterns; it is checked
   against the spec's 41 valid and invalid examples. A static walk of both schema documents panics on any keyword
   it does not know, including in subschemas no test instance reaches.

## Consequences

### Positive

- No dependency added; the event code is small and follows the spec text directly.
- A check is written entirely or not at all (tested by failing each write of a check in turn).
- Replay is byte-identical.

### Negative

- The test validator is ours to maintain; a schema change that uses a new keyword makes it panic until it is
  extended.
- The vendored schemas must be refreshed by hand when the spec changes.
- Unless the caller passes a `MatcherCache`, `Emit` reads and decodes every subscription inside the check's write
  transaction. Measured in review at 10,000 subscriptions: about 87 ms per `Emit` (about 85 ms of it loading the
  subscriptions), during which SQLite's single write lock is held. See the update below.

### Open questions

- oci, go-proxy and maven purls are built from `data.package`; their sources do not exist yet and may report
  digests or coordinates that change this.
- `release.yanked` emission is not part of this story.

## Update: matcher cache (story #166)

`event.MatcherCache` keeps the compiled matcher and decoded subscription policies between `Emit` calls. The
component that calls `Commit` or `Emit` owns it and passes it as `CheckInput.Matcher` or `EmitInput.Matcher`; there
is no package-level state, and a nil cache keeps the old per-call load.

- **Invalidation is in the database, not in the writers.** Migration 00008 adds a one-row `subscription_generation`
  table and triggers that bump it on every insert, update and delete of a `subscription` row. The watch-file apply,
  the API and a second process are covered without each having to call a bump. `store.Tx.SubscriptionGeneration`
  reads it.
- **Inside the transaction** `Emit` reads that counter (one row). Equal to the cached generation: the cache is exactly
  the committed set, so no per-id existence re-check is needed (the delivery table has no foreign key to
  `subscription`, by design). Different: it reloads inside the transaction, once per subscription change.
- **Outside the transaction** `Commit` calls `MatcherCache.Warm` first when its committer also lists subscriptions
  (the SQLite store does), so the reload normally happens before the write lock is taken.
- Measured with 10,000 subscriptions (`BenchmarkEmit10kSubscriptions`, `TestEmitHoldsTheTransactionBriefly`): a
  transaction holding one `Emit` takes about 1 to 2 ms with a warm cache against about 70 ms without.

## Alternatives considered

- **cloudevents/sdk-go for the envelope.** Listed in §6, but it adds a module tree for what a struct does. v1
  emits no extension attributes (the envelope schema only allows names matching `^[a-z0-9]{1,20}$`, for
  intermediaries), so nothing needs the SDK's attribute handling.
- **A JSON Schema library in tests** (for example `santhosh-tekuri/jsonschema`). Not in §6; would need a
  maintainer decision. Recommended if the schemas grow beyond the current subset.

## References

- angelia-dev/spec at 68e022e9d85cc8351fcc5cac29094af369cb8102: `schemas/release/v1.json`,
  `schemas/release/envelope/v1.json`, `docs/release-event-v1.md`.
- Issue #106 and its comment on emission rules.
