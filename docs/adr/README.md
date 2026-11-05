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
| [0006](0006-source-target-and-fetchstate.md) | Source Target, FetchState and error contract | Proposed | #50 |
| [0007](0007-version-schemes-in-house-with-lexical-fallback.md) | Version schemes are in-house, with a lexical fallback | Proposed | #54 |
| [0008](0008-store-history-rows-and-event-cursor.md) | Store history rows have no foreign key to config; the event cursor is a commit-order sequence | Proposed | #46 |
| [0009](0009-scheduler-target-leases-and-backoff.md) | The scheduler leases targets through the generic lease table and backs off exponentially | Proposed | #47 |
| [0010](0010-diff-engine-known-set-and-baseline.md) | The known set never shrinks and is scheme-neutral; baseline is per watch; changes are classified per scheme | Proposed | #56 |
| [0011](0011-wasm-plugin-host-prototype.md) | WASM source plugins on Extism, with HTTP through a netguard host function (prototype) | Proposed | #59 |
| [0012](0012-policy-evaluation-order-and-cooldown.md) | Policy runs in a fixed order after the diff; cooldown holds versions in a table and re-checks them on every check | Proposed | #57 |
| [0013](0013-go-plugin-subprocess-prototype.md) | Keep WASM for third-party plugins; go-plugin measured as a subprocess alternative (prototype) | Accepted | #60 |
| [0014](0014-subscription-policy-overrides.md) | Subscriptions narrow the watch policy with `bump`, `prerelease` and `cooldown`; a per-subscription cooldown is a held delivery | Proposed | #69 |
| [0015](0015-release-event-outbox-in-house.md) | Release events are built in-house as plain structs; the outbox is the delivery rows; events are immutable in the schema | Proposed | #106 |
| [0016](0016-message-templates-text-template-bounded.md) | Message templates are Go `text/template` over the event JSON, with curated functions, a deadline and a size cap, checked against sample events | Proposed | #114 |
| [0017](0017-dispatcher-delivery-leases-and-retries.md) | The dispatcher leases deliveries through the lease table, retries with full-jitter backoff, and passes sinks one request struct | Accepted | #107 |
| [0018](0018-yank-events-reference-the-original.md) | A yank of an emitted version is a `release.yanked` event that repeats the stored payload and references the original event in the store | Proposed | #112 |
| [0019](0019-prometheus-metrics-on-the-listen-address.md) | Prometheus metrics are served on the listen address, from a private registry, with closed label sets | Proposed | #117 |
| [0020](0020-opentelemetry-traces-link-checks-to-deliveries.md) | Traces are OpenTelemetry over OTLP/HTTP, enabled only by `OTEL_*` variables; a check is one trace and each delivery attempt a linked trace, joined through `delivery.traceparent` | Proposed | #118 |
| [0021](0021-source-canonical-target.md) | Sources canonicalise targets through an optional `Canonicalizer` interface (amends ADR 0006) | Accepted | #134 |
| [0022](0022-rest-api-in-house-spec-driven-validation.md) | The REST API validates and routes from the embedded OpenAPI document with an in-house JSON Schema subset; oapi-codegen generates types and the strict interface only, with a pinned template override | Accepted | #71 |
| [0024](0024-secrets-encrypted-at-rest.md) | Secret values are named, write-only rows sealed with AES-256-GCM under a key from a file; configuration references them by name | Accepted | #75 |
| [0025](0025-plugin-folder-and-grants-file.md) | Each installed plugin has a folder in `plugins_dir`; the operator's grants live in its `grants.yaml` | Accepted | #184 |

## Process

- Copy `0000-template.md` to `NNNN-short-title.md` with the next free number, and add a row here.
- A new ADR starts as **Proposed**. The maintainer sets it to **Accepted** when merging the review.
- An accepted ADR is not rewritten. To change a decision, write a new ADR that supersedes it and
  set the old one to **Superseded by ADR NNNN**, linking the new ADR.
- **Deprecated** means the decision no longer applies and nothing replaces it; the ADR is kept for history.
- Phase 0 spikes end with an ADR that records the measurements and the go/no-go.
