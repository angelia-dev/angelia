# Metrics

`angelia serve` exposes Prometheus metrics at `GET /metrics` on the `listen` address, next to `/healthz` and `/readyz` ([ADR 0019](../adr/0019-prometheus-metrics-on-the-listen-address.md)). `--metrics=false` turns the endpoint off.

```yaml
scrape_configs:
  - job_name: angelia
    static_configs:
      - targets: ["angelia:8080"]
```

The endpoint has no authentication, like the probes. It carries no secrets, package names, watch names or URLs (see [Cardinality](#cardinality)), but it does reveal host names of upstream registries and queue sizes: do not publish it to the internet. Restrict it with a network policy or a reverse proxy, as you would the probes.

A Grafana dashboard is in [`deploy/grafana/angelia.json`](../../deploy/grafana/angelia.json) (import it, then pick your Prometheus data source).

## Conventions

- Every Angelia metric starts with `angelia_`. Names are `snake_case`, durations are in seconds (`_seconds`), counters end in `_total`, and each metric has one unit (checked with `promlint` in the tests).
- Go runtime (`go_*`) and process (`process_*`) metrics are exported too.

## Metrics

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `angelia_build_info` | gauge | `version` | Always 1. |
| `angelia_scheduler_due_targets` | gauge | none | Targets due for a check and not leased, at the last scheduler poll. A backlog that keeps growing means the scheduler cannot keep up. |
| `angelia_check_lag_seconds` | histogram | none | How late each check started: lease time minus the time the target was due. This is the due-to-check delay of Phase 0 benchmark 1, whose target is P95 below 2 minutes; buckets at 0.5 s to 1 h, with 60, 90, 120, 180, 240 and 300 s around the target. |
| `angelia_source_fetches_total` | counter | `source`, `result` | Source fetches. `source` is the source kind (`pypi`, `npm`, `github-release`, ...), never the name of a [named instance](../private-registries.md#several-registries-of-one-kind): instances of one kind share a series, which keeps the cardinality bounded. `result` is `ok` (new data), `not_modified` (the server answered 304), `rate_limited` (429, or a rate limit with a reset time), `canceled` (shutdown, not a failure of the source) or `error`. |
| `angelia_netguard_rate_limit_wait_seconds` | histogram | `host` | Time an outgoing attempt waited for the per-host rate limit before it was sent; observed for every attempt (each retry and each redirect hop counts, not each request), zero when it did not wait. `host` is a host of the rate-limit table (the built-in hosts such as `api.github.com` and `crates.io`, plus any host limits configured on the client), lower case; every other host, including webhook and sink hosts, is `other`. |
| `angelia_delivery_attempts_total` | counter | `outcome` | Delivery attempts by outcome: `delivered`, `retry` (failed, due again later) or `dead` (gave up, including a delivery whose earlier attempt never finished and that is given up on without being sent again). Counted by the dispatcher process that made the attempt. |
| `angelia_deliveries` | gauge | `status` | Delivery rows in the store: `pending`, `in_flight`, `delivered`, `dead`. Read from the store at scrape time (cached for 10 s), so it is the same on every process that shares the database. Delivered and dead rows disappear with their event when history is pruned. |
| `angelia_dlq_size` | gauge | none | The dead-letter size: deliveries that gave up and are still in the store. Same value as `angelia_deliveries{status="dead"}`. |

`angelia_deliveries` and `angelia_dlq_size` appear in a process that has a store; the others in every process that runs the matching role.

## Useful queries

ETag hit rate, per source (share of fetches answered with 304):

```promql
sum by (source) (rate(angelia_source_fetches_total{result="not_modified"}[1h]))
/
sum by (source) (rate(angelia_source_fetches_total{result=~"ok|not_modified"}[1h]))
```

P95 check lag over 5 minutes:

```promql
histogram_quantile(0.95, sum by (le) (rate(angelia_check_lag_seconds_bucket[5m])))
```

Share of requests that waited for a rate limit, per host:

```promql
1 - (
  sum by (host) (rate(angelia_netguard_rate_limit_wait_seconds_bucket{le="0.001"}[5m]))
  / sum by (host) (rate(angelia_netguard_rate_limit_wait_seconds_count[5m]))
)
```

`angelia_deliveries` and `angelia_dlq_size` come from the shared database, so every process reports the same value: aggregate with `max`, never `sum`. If the database cannot be counted, they are missing from the scrape (a warning is logged) and the failure is remembered for 10 s.

Alert ideas: `max(angelia_dlq_size) > 0` for 15 minutes, `histogram_quantile(0.95, ...) > 120`, `angelia_scheduler_due_targets` rising for 30 minutes.

## Cardinality

The label sets are closed: `source` is a registered source kind, `result`, `outcome` and `status` are fixed lists, `host` comes from the rate-limit table plus `other`, so sink and webhook hosts (which can contain secrets) never appear. No label carries a package, repository, watch, target, subscription or sink name, and there is no setting to add one: per-package series would grow with the watch list (tens of thousands of series for 10,000 watches). Use the API or the logs to look at one package.

A source kind that has never fetched has no series yet; use `absent()` or `or vector(0)` in alerts accordingly.
