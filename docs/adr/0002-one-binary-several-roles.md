# ADR 0002: One binary, several roles

- **Status:** Proposed
- **Date:** 2026-11-01
- **Story:** angelia-dev/angelia#43

## Context

The initial proposal had separate components: a core API, verifiers and a dispatcher. Self-hosters want
**one container**; separate services from day one would mean a broker, service discovery and three images
to version ([architecture.md §2](../architecture.md#2-review-of-the-proposed-split), change 1).
The goal G7 in [prd.md §5](../prd.md#5-goals-and-non-goals) is one container with SQLite that scales with Postgres.

## Decision

Angelia ships as **one binary** (`cmd/angelia`) with several **roles**:

```
angelia serve --roles=api,scheduler,worker,dispatcher    # default: all
```

- Roles are `api`, `scheduler`, `worker` and `dispatcher`. The default is `all`.
- To scale out, run more `worker` or `dispatcher` replicas of the same code (architecture.md §2, §7).
- Internal boundaries are Go packages with interfaces (`Source`, `Sink`, see architecture.md §3), **not network hops**.
- Roles coordinate through the store (leasing, see [ADR 0003](0003-sqlite-default-postgres-ha-no-external-queue.md)), not through a broker.

## Consequences

### Positive

- Homelab mode is a single `docker run` with SQLite on a volume (architecture.md §7).
- One image and one version to release, sign and document.
- The same code runs as a monolith or split across Deployments (Helm: `api` and `dispatcher` Deployments, `worker` HPA).

### Negative

- All roles share one release cadence and one dependency set; a role cannot be upgraded alone.
- SQLite is a single-node store with a single-process lease, so scale-out across processes needs Postgres ([ADR 0003](0003-sqlite-default-postgres-ha-no-external-queue.md); architecture.md §3.2).
- Package boundaries are enforced by code review and interfaces only, not by process isolation.

### Open questions

- Is the role list final? architecture.md names four; whether SSE and MCP serve from `api` or need their own role is not stated.
- Do `/healthz` and `/readyz` ([prd.md §7](../prd.md#7-non-functional-requirements)) report per role? Not specified.
- Can several `scheduler` instances run at once? The docs only say "a single-process lease on SQLite" and `SKIP LOCKED` on Postgres.

## Alternatives considered

- **Separate services (core, verifier, dispatcher).** The original proposal. Needs a broker, service
  discovery and three images to version, which defeats the one-container goal (architecture.md §2).
- **A single fixed all-in-one process with no roles.** Simplest to run, but cannot scale `worker` or
  `dispatcher` independently, which the Helm layout and Postgres HA mode rely on
  (architecture.md §7; prd.md §9 phase 2).

## References

- [architecture.md §2](../architecture.md#2-review-of-the-proposed-split), [§3](../architecture.md#3-component-view), [§7](../architecture.md#7-deployment)
- [prd.md §5](../prd.md#5-goals-and-non-goals) (G7)
