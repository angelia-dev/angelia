# ADR 0004: CloudEvents 1.0 and purl are the event contract; event is not delivery; transactional outbox

- **Status:** Proposed
- **Date:** 2026-11-01
- **Story:** angelia-dev/angelia#43

## Context

The event contract is the product ([architecture.md §4](../architecture.md#4-the-event-contract-the-product)).
Consumers (CI, chat, agents) need one structured, versioned event per new version. The original proposal
mixed the event and its deliveries, which makes retries and replay hard
([§2](../architecture.md#2-review-of-the-proposed-split), change 4). No lost events across crashes is a
requirement ([prd.md §7](../prd.md#7-non-functional-requirements)); see also G3 and G4 in
[prd.md §5](../prd.md#5-goals-and-non-goals).

## Decision

1. **Format.** Events are **CloudEvents 1.0**, JSON format. The schema lives in its own repo (`spec`) and is
   versioned with SemVer. Types look like `dev.angelia.release.published.v1`; the payload is in `data`
   (architecture.md §4 shows the draft example).
2. **Package identity.** Packages are identified with **purl** (`data.purl`, for example
   `pkg:pypi/requests@2.33.0`) so consumers can join with SBOMs, OSV and Renovate.
3. **Event is not delivery.** One immutable `ReleaseEvent` row; N `Delivery` rows, one per matching
   subscription, each with its own retry state (architecture.md §2, §5).
4. **Transactional outbox.** The event and its deliveries are written in the **same database transaction**
   as the state change. Dispatch reads deliveries from the database
   ([ADR 0003](0003-sqlite-default-postgres-ha-no-external-queue.md)).
5. **Delivery semantics.** At-least-once. Each delivery carries `idempotency-key = event.id + subscription.id`.
   Retries, dead-letter and replay are per delivery (architecture.md §3.5).
6. **Event types** for v1: `release.published`, `release.yanked`, `release.prerelease`; `release.advisory` later.
   Event ids are ULIDs, monotonic, and serve as cursors in the pull API (§3.1, §5).

## Consequences

### Positive

- A retry or replay of one delivery never alters or duplicates the event.
- No lost events across crashes, and no two-system write.
- Standard envelope: existing CloudEvents SDKs work; purl lets consumers correlate with SBOM and OSV data.
- The same event feeds push sinks and pull channels (cursor API, SSE, MCP).

### Negative

- The `event` and `delivery` tables grow; they are pruned after N days (default 90).
- Consumers must handle duplicates (at-least-once).
- Once published, `data` fields are a public contract; changes need a new type version.

### Open questions

- **Public field names** of `data` (for example `ecosystem`, `bump`, `cooldown_satisfied_at`) appear in the draft example only. AGENTS.md requires the maintainer to name public schema fields; the example is not frozen by this ADR.
- Which `data` fields are required versus optional is not stated.
- Whether the `source` URI form `angelia://<instance>/watches/<ecosystem>/<name>` is final.
- Versioning rules for the `v1` type suffix and the `spec` repo SemVer are not defined; the spec freeze is a Phase 3 exit criterion ([prd.md §9](../prd.md#9-roadmap)).
- `release.yanked` and `release.prerelease`: payloads and emission rules are not specified.
- Exact outbox mechanics (a separate outbox table, or the `delivery` rows themselves) are not specified; §2 and §5 imply the `delivery` rows.

## Alternatives considered

- **A bespoke JSON payload.** Simpler to start, but every consumer needs custom parsing and there is no
  ecosystem tooling; competing payloads are built for humans, not machines ([prd.md §1](../prd.md#1-problem), §4).
- **Event and delivery in one record** (the original "batch" proposal). Per-subscription state is mixed with
  the immutable fact, so retry and replay become hard (architecture.md §2).
- **Publish after commit, without an outbox** (write the DB, then send). A crash between the two loses the
  event, violating the no-loss requirement (prd.md §7).
- **Package identified by ecosystem plus name strings only.** Works inside Angelia, but consumers could not
  join with SBOMs, OSV and Renovate (architecture.md §4).

## References

- [architecture.md §2](../architecture.md#2-review-of-the-proposed-split), [§3.4](../architecture.md#34-normalizer-and-policy-engine), [§3.5](../architecture.md#35-dispatcher-and-sink-plugins-deliver), [§4](../architecture.md#4-the-event-contract-the-product), [§5](../architecture.md#5-data-model-simplified)
- [prd.md §5](../prd.md#5-goals-and-non-goals) (G3, G4), [§7](../prd.md#7-non-functional-requirements)
- CloudEvents 1.0 specification; Package URL (purl) specification
