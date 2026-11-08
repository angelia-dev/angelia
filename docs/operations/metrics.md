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
| `angelia_check_lag_seconds` | histogram | none | How late each check started: lease time minus the time the target was due. The Phase 0 target is P95 below 2 minutes. |
| `angelia_source_fetches_total` | counter | `source`, `result` | Source fetches. `source` is the source kind (`pypi`, `npm`, `github-release`, ...), never the name of a [named instance](../private-registries.md#several-registries-of-one-kind): instances of one kind share a series, which keeps the cardinality bounded. `result` is `ok` (new data), `not_modified` (the server answered 304), `rate_limited` (429, or a rate limit with a reset time), `canceled` (shutdown, not a failure of the source) or `error`. |
| `angelia_netguard_rate_limit_wait_seconds` | histogram | `host` | Time an outgoing attempt waited for the per-host rate limit before it was sent; observed for every attempt (each retry and each redirect hop counts, not each request), zero when it did not wait. `host` is a host of the rate-limit table (the built-in hosts `api.github.com`, `crates.io` and `gitlab.com`, plus the hosts of [`http.host_limits.hosts`](../configuration.md#per-host-rate-limits)), lower case; every other host, including webhook and sink hosts, is `other`. The same rule applies to every `angelia_netguard_*` metric. |
| `angelia_netguard_host_limit_rate` | gauge | `host`, `bucket`, `origin` | Effective rate limit of a host in requests per second, set once at start by every process that sends requests (scheduler, dispatcher). `bucket` is `authenticated` or `anonymous`; `origin` is `builtin` (the built-in limit applies unchanged), `config` (from `http.host_limits.hosts`, also when part of the entry was capped at a built-in budget) or `default` (from `http.host_limits.default`). `host` is a host of the rate-limit table, or `other` for the fallback that applies to every unlisted host. Limits apply per process: see [Per-host rate limits](../configuration.md#per-host-rate-limits). |
| `angelia_netguard_host_limit_burst` | gauge | `host`, `bucket`, `origin` | Effective burst (bucket size) of a host, with the same labels. |
| `angelia_netguard_server_pauses_total` | counter | `host`, `reason` | Pauses of a host's bucket set by the server: `reason` is `retry_after` (a `Retry-After` header) or `ratelimit_reset` (an `X-RateLimit-*` or `RateLimit-*` pair with `Remaining` at 0). |
| `angelia_netguard_responses_total` | counter | `host`, `class` | Outgoing attempts by outcome, each retry and redirect hop counted: `class` is `2xx`, `304`, `3xx`, `4xx`, `429`, `5xx`, or `error` when the attempt got no response (a transport error or a timeout). |
| `angelia_netguard_deadline_refusals_total` | counter | `host` | Requests refused at once because the host's next rate-limit slot came after the caller's deadline (a check has 3 minutes). It is the direct sign that a host's limit is too low for the demand. |
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

Requests refused because a host's limit is too low for the demand, and the 429 rate, per host:

```promql
sum by (host) (rate(angelia_netguard_deadline_refusals_total[15m]))
sum by (host) (rate(angelia_netguard_responses_total{class="429"}[15m]))
```

Alert ideas: `max(angelia_dlq_size) > 0` for 15 minutes, `histogram_quantile(0.95, ...) > 120`, `angelia_scheduler_due_targets` rising for 30 minutes.

## Cardinality

The label sets are closed: `source` is a registered source kind, `result`, `outcome`, `status`, `bucket`, `origin`, `reason` and `class` are fixed lists, `host` comes from the rate-limit table plus `other`, so sink and webhook hosts (which can contain secrets) never appear unless the operator lists one in `http.host_limits.hosts`: a listed host is a label value by the operator's choice (ADR 0027 §8). No label carries a package, repository, watch, target, subscription or sink name, and there is no setting to add one: per-package series would grow with the watch list (tens of thousands of series for 10,000 watches). Use the API or the logs to look at one package.

A source kind that has never fetched has no series yet; use `absent()` or `or vector(0)` in alerts accordingly.
