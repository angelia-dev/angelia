# Architecture decision records

Each ADR records one architectural decision: its context, the decision, its consequences and the
alternatives considered. All ADRs use the same [template](0000-template.md).

| ADR | Title | Status | Story |
|---|---|---|---|
| [0001](0001-go-as-implementation-language.md) | Go is the implementation language (provisional until Phase 0 benchmarks) | Proposed | #43 |
| [0002](0002-one-binary-several-roles.md) | One binary, several roles | Proposed | #43 |
| [0003](0003-sqlite-default-postgres-ha-no-external-queue.md) | SQLite by default, Postgres for HA, no external queue | Proposed | #43 |
| [0004](0004-cloudevents-purl-event-contract.md) | CloudEvents 1.0 and purl as the event contract; event is not delivery; transactional outbox | Proposed | #43 |
| [0005](0005-standard-webhooks-signing.md) | Sign webhook deliveries with Standard Webhooks | Proposed | #43 |
| [0006](0006-version-schemes-in-house-with-lexical-fallback.md) | Version schemes are in-house, with a lexical fallback | Proposed | #54 |

## Process

- Copy `0000-template.md` to `NNNN-short-title.md` with the next free number, and add a row here.
- A new ADR starts as **Proposed**. The maintainer sets it to **Accepted** when merging the review.
- An accepted ADR is not rewritten. To change a decision, write a new ADR that supersedes it and
  set the old one to **Superseded by ADR NNNN**, linking the new ADR.
- **Deprecated** means the decision no longer applies and nothing replaces it; the ADR is kept for history.
- Phase 0 spikes end with an ADR that records the measurements and the go/no-go.
