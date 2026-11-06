# ADR 0003: SQLite by default, Postgres for HA, no external queue

- **Status:** Proposed
- **Date:** 2026-11-01
- **Story:** angelia-dev/angelia#43

## Context

Self-hosters want one container ([ADR 0002](0002-one-binary-several-roles.md)). Scheduler and dispatcher need to
lease due watches and due deliveries ([architecture.md §3.2](../architecture.md#32-scheduler-and-workers),
[§3.5](../architecture.md#35-dispatcher-and-sink-plugins-deliver)). The stack assumption is in
[§6](../architecture.md#6-language-and-framework-phase-0-assumptions); the data model is in
[§5](../architecture.md#5-data-model-simplified). Goal G7: one container with SQLite, scaling with Postgres
([prd.md §5](../prd.md#5-goals-and-non-goals)).

## Decision

- **SQLite is the default store**, through `modernc.org/sqlite` (pure Go, no CGO).
- **Postgres** (`pgx`) is the store for HA and scale-out.
- Queries are generated with `sqlc`; migrations use `goose` and live in `internal/store/migrations`.
- **No external queue or broker is required.** Watches and deliveries are leased from the database:
  `SELECT … FOR UPDATE SKIP LOCKED` on Postgres, a single-process lease on SQLite (architecture.md §3.2).

## Consequences

### Positive

- Zero extra services for the homelab mode: `docker run` with a volume.
- No broker to operate, secure or version.
- The transactional outbox ([ADR 0004](0004-cloudevents-purl-event-contract.md)) needs only one database transaction.

### Negative

- SQLite limits the deployment to a single node; scale-out needs Postgres.
- Two engines to test and migrate.
- The database is the queue, so leasing throughput is bounded by the database.
- `modernc.org/sqlite` performance is unmeasured so far; Phase 0 benchmarks cover it.

### Open questions

- Job leasing on Postgres: `River`, or "a small in-house lease table usable on both" engines. architecture.md §6 offers both and does not choose.
- How far does single-node SQLite go? The target is 10,000 watches (architecture.md §10, question 2); a Phase 0 measurement.
- Whether one schema and lease implementation serve both engines is implied by "usable on both" but not stated as a rule.
- Secret encryption at rest (`sink.config_json` holding references only, see ADR 0024) is a security decision outside this ADR.
- Migration policy for data-dropping or data-rewriting migrations is not covered; AGENTS.md sends those to the maintainer.

## Alternatives considered

- **External queue or broker** (for example NATS, Kafka, RabbitMQ). Adds a service to run and version for
  every self-hoster, which contradicts the one-container goal; the DB lease is enough (architecture.md §2, §3.2).
- **Postgres only.** Simplest to reason about for HA, but removes the zero-dependency homelab mode
  (G7, architecture.md §7).
- **SQLite through a CGO driver.** Breaks the `CGO_ENABLED=0` rule ([ADR 0001](0001-go-as-implementation-language.md), AGENTS.md).

## References

- [architecture.md §3.2](../architecture.md#32-scheduler-and-workers), [§5](../architecture.md#5-data-model-simplified), [§6](../architecture.md#6-language-and-framework-phase-0-assumptions), [§7](../architecture.md#7-deployment), [§10](../architecture.md#10-open-questions-for-phase-0)
- [prd.md §5](../prd.md#5-goals-and-non-goals) (G7)
