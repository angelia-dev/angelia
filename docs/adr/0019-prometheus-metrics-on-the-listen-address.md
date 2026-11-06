# ADR 0019: Prometheus metrics are served on the listen address, from a private registry, with closed label sets

- **Status:** Proposed
- **Date:** 2026-11-02
- **Story:** angelia-dev/angelia#117

## Context

[architecture.md §6](../architecture.md#6-language-and-framework-phase-0-assumptions) lists
"Prometheus `/metrics`" in the observability stack. Story #117 asks for check lag, fetch results by
source and status, ETag hit rate, rate-limit waits, deliveries by status and the DLQ size, with
Prometheus naming, bounded cardinality and a Grafana dashboard.

What exists: the supervisor owns one HTTP listener (`listen`) that serves `/healthz` and `/readyz` and
lets roles mount routes (`supervisor.HTTPRole`, ADR 0002). There is no REST API server yet (#71). The
scheduler already has a `Metrics` interface (`DueBacklog`, `CheckLag`). The detect pipeline that
glues scheduler, sources and diff is not merged, and `serve` still starts stub roles.

## Decision

1. **Library.** The official `github.com/prometheus/client_golang` (Apache-2.0), with its transitive
   modules (`client_model`, `common`, `procfs`, `beorn7/perks`, `cespare/xxhash`,
   `munnerz/goautoneg`, `go.yaml.in/yaml/v2`; Apache-2.0, MIT, BSD-3). `promlint` from the same module
   checks names in tests.
2. **Package.** All metric code is in `internal/metrics`. Other packages only gain small hooks, and
   none imports `internal/metrics` or Prometheus: `scheduler.Metrics` (existing), `dispatch.Metrics`
   (`AttemptFinished(outcome)`), `netguard.Observer` (`RateLimitWait(host, d)`). `*metrics.Metrics`
   implements all three. Source fetches are counted by wrapping a source (`Metrics.Source`), so the
   `Source` interface does not change. Nothing registers on the global default registry.
3. **Where.** `GET /metrics` is served on the supervisor's `listen` address by a small role
   (`Metrics.Role`, an `HTTPRole`) that `serve` appends to the selected roles. `--metrics=false`
   disables it. It is mounted by every process regardless of `--roles`, because each process counts
   what it does. When the API server (#71) lands, it keeps using the same listener; no second port.
   No YAML key is added.
4. **No authentication** on `/metrics`, like the probes: Prometheus scrapers expect a plain GET and
   the content is non-sensitive (counts, upstream registry host names). The operations page says to
   restrict network access. A later story can add a token if the API auth (#71 and following)
   defines one.
5. **Closed label sets.** `source` (registered kinds), `result` (ok, not_modified, rate_limited,
   canceled, error), `outcome` (delivered, retry, dead, other), `status` (pending, in_flight,
   delivered, dead), `host` (only hosts of the netguard rate-limit table, `DefaultHostLimits` plus `Config.HostLimits`, else `other`; sinks share the netguard client and webhook hosts can carry secrets, so netguard folds unlisted hosts before the observer sees them). No package, watch, target,
   subscription or sink name is ever a label.
6. **Store-backed gauges.** `angelia_deliveries{status}` and `angelia_dlq_size` are read at scrape time
   with `Store.CountDeliveriesByStatus` (one `GROUP BY status` query that reads only the status index, new sqlc query, no migration),
   cached for 10 s, failures included, bounded by a 5 s timeout. A failed query leaves those metrics out of the scrape
   and does not fail it; the failure is logged. The values come from the shared database, so dashboards aggregate them with `max`.
7. **ETag hit rate** is a query (`not_modified / (ok + not_modified)` over
   `angelia_source_fetches_total`), not a metric.

## Consequences

### Positive

- One port, no new config key, nothing to expose twice behind a proxy.
- Hooks are tiny interfaces, so instrumentation does not tie other packages to Prometheus, and tests
  use fakes.
- Series count is bounded and independent of the number of watches.

### Negative

- `/metrics` is open to whoever can reach `listen`, as are the probes.
- `go.mod` gains nine modules (two direct, seven indirect). `go.yaml.in/yaml/v2` and `google.golang.org/protobuf` are linked into the binary, which grows by about 3.7 MB (23.4 to 27.1 MB).
- A binary running only `--roles=api` still reports `angelia_scheduler_*` metrics at zero or absent;
  dashboards must tolerate that.
- `Store` gains a method (internal interface), so every implementation (Postgres, later) must add it.

### Open questions

- Wiring: `serve` creates `Metrics` but the scheduler, dispatcher, netguard client and sources are
  not constructed there yet. Whoever wires those roles passes `Metrics` as their hook and wraps sources
  with `Metrics.Source`, and calls `RegisterDeliveryCounts(store)`.
- Fetch duration and check outcome (success or failure per check) metrics were not asked for and are
  left out; both would be additive.
- Whether `/metrics` should move to a separate admin port once the public API is exposed to the
  internet.

## Alternatives considered

- **In-house text exposition, no dependency.** About 150 lines for counters, gauges and histograms.
  Rejected: reimplementing the format (escaping, histogram buckets, concurrency) is risk without
  value, and the official client is the ecosystem default. It stays an option if the dependency is
  refused.
- **OpenTelemetry metrics SDK with a Prometheus exporter.** More modules and a heavier API for the
  same output; the stack in §6 names Prometheus for metrics and OpenTelemetry for traces.
- **A separate metrics port.** Stronger isolation, but a second listener to configure, secure and
  drain, and a new config key (a public contract).
- **A `package` label.** Useful to look at one package, but unbounded; refused by the acceptance
  criteria.

## References

- Story angelia-dev/angelia#117; [docs/operations/metrics.md](../operations/metrics.md);
  [deploy/grafana/angelia.json](../../deploy/grafana/angelia.json).
- ADR 0002 (roles share a listener), ADR 0009 (scheduler), ADR 0017 (dispatcher).
- Prometheus: [metric and label naming](https://prometheus.io/docs/practices/naming/).

## Amendment (2026-11-06, #198)

The wiring question above is closed: `angelia serve` passes its `Metrics` to the scheduler, the
dispatcher and the netguard client, wraps the scheduler's sources with `Metrics.Source`, and calls
`RegisterDeliveryCounts` on the store when a role opens it. With `--metrics=false` every hook is nil.
