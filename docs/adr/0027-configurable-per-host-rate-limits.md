# ADR 0027: Operators can set per-host rate limits in the instance configuration (`http.host_limits`); built-in budgets and server signals still win

- **Status:** Accepted (2026-11-08, maintainer, with the recommended answers below)
- **Date:** 2026-11-08
- **Story:** angelia-dev/angelia#217

## Context

Every request of a source, a sink or a plugin goes through one netguard client
([internal/netguard/doc.go](../../internal/netguard/doc.go)). Its limiter keeps one token bucket per
lower-case hostname and per budget (authenticated or anonymous), in
[limiter.go](../../internal/netguard/limiter.go):

- **Built-in limits** (`DefaultHostLimits`) follow published budgets: `api.github.com` (5,000 per hour with a
  token, 60 without), `gitlab.com` (2,000 and 500 per minute) and `crates.io` (1 per second, its crawler
  policy).
- **Every other host** gets the fallback, `DefaultFallbackLimit`: 5 requests per second with a burst of 5.
  This includes `pypi.org`, `registry.npmjs.org`, `index.crates.io`, private mirrors and webhook hosts.
- `netguard.Config.HostLimits` and `Config.FallbackLimit` can change both, but only from Go.
  `angelia serve` builds the client in `newNetClient` ([cmd/angelia/roles.go](../../cmd/angelia/roles.go))
  without them, and the [instance configuration](../configuration.md#outgoing-http-and-private-networks)
  has no key for them.
- **Server signals pause a host.** A `Retry-After`, or a `X-RateLimit-*` / `RateLimit-*` pair with
  `Remaining` at 0, pauses the host's bucket until the given time (at most 1 hour).
- **Retries.** A 429, a 5xx or a timeout is retried up to 4 attempts with exponential backoff and full
  jitter. The client sleeps through a server-requested wait only up to `MaxRetryWait` (2 minutes).
- **Each attempt takes a token.** Retries and redirect hops each take one from their own host's bucket.
- **The scheduler does not throttle per host** ([ADR 0009 §6](0009-scheduler-target-leases-and-backoff.md)).
  It runs 4 workers by default. Each check is bounded by `CheckTimeout` (3 minutes). When the next slot of a
  bucket falls after the check's deadline, the limiter fails at once, the check counts as an error, and the
  target backs off.

The first 10,000-watch run of benchmark 1 (PR #215) failed on this limit. Every fake ecosystem is served
from one loopback address, so the whole run shares one 5 rps bucket: about 1,500 checks per 5 minutes,
against a demand of 10,000 / 300 s = 33.3 rps. Split by ecosystem, the demand is still about 8.3 rps per
registry (2,500 targets each over the four measured ecosystems). A real instance with that many watches on
`pypi.org` or `registry.npmjs.org` at the 5-minute minimum interval would hit the same cap. On #62
(2026-11-08) the maintainer chose a configurable per-host limit in the instance configuration, to be
settled by a Proposed ADR before any code.

[prd.md](../prd.md) lists politeness (per-host rate limits, conditional requests, shared fetches) as a
requirement, and [architecture.md §3.2](../architecture.md#32-scheduler-and-workers) puts the per-host
limiter in the core. A configurable limit must therefore stay an operator decision. It must not become a
way around politeness.

## Decision

### 1. Config shape

A new block under `http` in `angelia.yaml`. Only rates are configurable: the per-host body cap and
timeout (`HostLimit.MaxBodyBytes`, `HostLimit.Timeout`) stay Go-only and are not part of this decision.

```yaml
http:
  host_limits:
    default:                  # optional: replaces the 5 rps / burst 5 fallback for unlisted hosts
      rps: 5
      burst: 5
    hosts:                    # optional: exact host names
      nexus.internal.example: {rps: 40, burst: 40}
      pypi.org: {rps: 10, burst: 20}
      registry.npmjs.org: {rps: 10}       # burst defaults to ceil(rps)
      api.github.com: {rps: 0.5, burst: 5} # may only lower a built-in budget (section 4)
```

- `rps` is a positive decimal number of requests per second, refilled continuously (the existing token
  bucket, `netguard.Limit.Rate`).
- `burst` is an integer, the bucket size (`netguard.Limit.Burst`). It defaults to `ceil(rps)`, and to at
  least 1.
- **One limit per entry.** It applies to both the authenticated and the anonymous bucket of the host. The
  two buckets stay separate, so each gets this limit (question 5).
- Host names go under `hosts`, never at the same level as `default`. `default` is a valid host name and
  must not be ambiguous.
- No environment override. A map does not fit the flat `ANGELIA_*` scheme, and `sources.<name>.kind` has
  none either (question 8).
- The limits are read once, at start, like the other `http.*` keys. A change needs a restart.
- Go side: `config.HTTP` gains `HostLimits` and `newNetClient` passes the result to
  `netguard.Config.HostLimits` and `Config.FallbackLimit`. Neither `netguard.HostLimit` nor the limiter
  changes, apart from the cap of section 4.

### 2. Defaults and upper bounds

- **Unchanged without the key.** The fallback stays 5 rps with a burst of 5, and the built-in limits of
  section 4 stay as they are. An instance without `http.host_limits` behaves exactly as today.
- Bounds are validation errors, so `config check` and `serve` both fail:

  | Key | Minimum | Maximum |
  | --- | --- | --- |
  | `hosts.<host>.rps` | 1/3600 (one per hour) | 100 |
  | `hosts.<host>.burst` | 1 | 200 |
  | `default.rps` | 1/3600 | 20 |
  | `default.burst` | 1 | 20 |

- **The ceilings.** 100 rps per host is three times the whole benchmark-1 demand on a single host
  (33.3 rps), which leaves room for pagination and retries. It is far above anything a public registry
  should get from one instance. `default` is capped lower because it raises every host that nobody named,
  including webhook hosts and hosts a watch-file author can choose (section 7) (questions 2 and 3).
- **Values are numbers.** A non-number `rps` (`"10/s"`), `0`, a negative value, `NaN` or `Inf` is an
  error. So is a `burst` that is not an integer. Unknown keys in an entry are errors, as everywhere in the
  instance config.

### 3. Matching

- **Exact host name, no port.** Matching follows `normHost`: case-insensitive, with a trailing dot
  ignored. The limiter keys buckets by host name only, so all ports of one host share a bucket: they are
  usually one server.
- **Allowed spellings.** An entry is a host name, an IPv4 address, or an IPv6 address without brackets
  (`::1`), which is how `url.URL.Hostname()` reports it.
- **Refused spellings.** An entry with a scheme, a port, a path, brackets or upper-case letters is an
  error that names the expected spelling. This is the same rule as `http.allow_http_hosts`.
- **No wildcards in v1.** `*.example.com` or `.example.com` is an error. A wildcard would mean either one
  bucket shared by many servers, which is surprising, or one bucket per subdomain. In the second case, a
  party that controls DNS names under the suffix, for example through a webhook URL, could multiply the
  budget at will (question 4).
- **Redirects.** Each hop is limited by its own host, as today. A limit on `pypi.org` does not cover
  `files.pythonhosted.org`.

### 4. Combination with built-in limits, server signals and the scheduler

**Precedence, per host and per bucket:**

1. **Built-in budget hosts** (`api.github.com`, `gitlab.com`, `crates.io`): the effective limit is the
   lower of the entry and the built-in limit, compared rate by rate and burst by burst. An operator can
   slow these hosts down but cannot raise them above their published budget. A value above the built-in
   one is capped, and `config check` warns (question 1).
2. **Other hosts with an entry under `hosts`:** the entry.
3. **Every other host:** `default` if set, else the built-in 5 rps / burst 5.

The built-in body caps and timeouts of `registry.npmjs.org`, `pypi.org` and `index.crates.io` are kept
whatever the rate entry says. Today `limiter.maxBodyBytes` already falls back to them when an entry has
`MaxBodyBytes` 0.

**Server signals.** A configured limit is a ceiling, never a floor: the server can always slow Angelia
down, never speed it up.

- `Retry-After`, `X-RateLimit-*` and `RateLimit-*` still pause the host's bucket. That includes the
  1-hour cap on a pause, and promised slots move back as today.
- A 429 is still retried with full-jitter backoff, at most 4 attempts and at most `MaxRetryWait` of
  server-requested sleep.
- Each retry takes a token like any attempt, so a high limit does not turn into a retry storm.
- No adaptive raising or lowering from responses in v1 (alternatives).

**Scheduler.**

- ADR 0009 §6 stands: the scheduler does not throttle per host, and the netguard bucket remains the only
  pacing.
- **Too low a limit** shows up in three ways: `angelia_netguard_rate_limit_wait_seconds` grows; checks
  fail when their slot falls after `CheckTimeout`, which counts as an error with backoff; and check lag
  grows.
- **Head-of-line blocking.** With 4 workers, checks sleeping on a saturated host also delay checks of other
  hosts. A limit sized to the demand avoids both problems: `rps` ≥ targets on the host ÷ shortest
  interval × requests per check.
- **Out of scope.** A demand estimate in the scheduler log and the worker count (an open question of
  ADR 0009) stay outside this decision (question 9).

### 5. Who may set it

- **Instance configuration only** (`angelia.yaml`, through `--config` or `ANGELIA_CONFIG`). It is the same
  boundary as `http.allow_networks`.
- **Never** the watch file, the REST API, MCP, a plugin manifest or `grants.yaml`. A `host_limits` key in
  any of them is an unknown key, which is an error.
- **Why.** These surfaces can be written by people the operator trusts less. They could then aim the
  instance's request budget at a third party, or exhaust a shared upstream budget for everyone on the
  instance.
- **Read-only exposure.** The API may later show the effective limits read-only. Nothing here adds an
  endpoint.

### 6. Validation and `config check`

**Errors** (fatal for `config check` and `serve`):

- out-of-range values (section 2);
- a malformed host (section 3);
- the same host listed twice after normalisation (`PyPI.org` and `pypi.org.`).

**Warnings** (`config check` and the `serve` start log):

1. **Above a built-in budget.** `http.host_limits.hosts.api.github.com: rps 3 is above the built-in
   1.39 (5,000/h with a token); capped at the built-in limit`. The same applies to `gitlab.com` and
   `crates.io`.
2. **High rate on a public registry.** `http.host_limits.hosts.pypi.org: rps 30 on a public registry; read
   its usage policy, over 20 rps can get this instance blocked`. The warning covers any `rps` above 20 for
   a host that is the default `base_url` of a built-in source: `pypi.org`, `registry.npmjs.org`,
   `index.crates.io`, `crates.io`, `api.github.com` and `gitlab.com`.
3. **Default raised.** `http.host_limits.default: rps 10 is above the built-in 5 and applies to every
   unlisted host, webhook hosts included`.
4. **Possible typo.** `http.host_limits.hosts.nexus.example: no configured source uses this host`. The
   check compares the entry with the hosts of the `sources.*.base_url` values, the defaults of the built-in
   sources, and the plugin `allowed_hosts`. This warning is informational, because sinks are not in the
   instance config.

**Listing.** `config check` lists the effective table once, with the origin of each line, in the style of
its plugin listing:

```text
http host limits (per process):
  127.0.0.1                50 rps, burst 50 (config)
  api.github.com           1.39 rps, burst 10 with a token; 0.016 rps, burst 1 without (built-in)
  crates.io                1 rps, burst 1 (built-in)
  gitlab.com               33.2 rps, burst 10 with a token; 8.2 rps, burst 10 without (built-in)
  other hosts              5 rps, burst 5 (built-in default)
```

### 7. Politeness and abuse

**Limits are per process.**

- Several `serve` processes on one Postgres database each have their own buckets. Three schedulers with
  `pypi.org: {rps: 10}` can send 30 rps.
- The documentation must say so and recommend dividing the intended rate by the number of
  scheduler/worker processes.
- A shared, database-backed limiter is out of scope (alternatives).

**Sources, sinks and plugins share the client.** A limit on a host also paces deliveries to that host. A
webhook receiver on the same host as a registry mirror shares its bucket.

**Public registries.**

- The documentation for this key must say four things:
  - keep public registries at or near the default;
  - rely on conditional requests (a 304 still takes a token here, but costs the registry little);
  - prefer longer intervals or a mirror for large watch sets;
  - every request carries `http.user_agent`, so a registry can identify the instance.
- Section 6 warns above 20 rps on these hosts. Section 4 caps the hosts with a published budget.

**Who can aim the budget.**

- Only the operator sets a limit, and the operator already controls every request the instance sends.
- Watch-file and API authors choose targets on known registries, and sink URLs on any host. They can make
  the instance send requests, but only within the operator's limits:
  - unlisted hosts are capped by `default`, at most 20 rps;
  - wildcards are refused, so a subdomain cannot create a new bucket under a raised limit.
- Raising a limit never widens the SSRF guard: `http.allow_networks` still decides which addresses may be
  dialled.

### 8. Metrics

These follow [ADR 0019](0019-prometheus-metrics-on-the-listen-address.md): closed label sets, and `host`
taken only from the limits table, otherwise `other`.

**`host` label.** The limits table now includes the operator's `hosts`. A configured host appears as a
`host` label value, and that is the operator's explicit choice. The documentation says so, because a
webhook host listed here becomes visible in metrics.

**Existing, unchanged:**

- `angelia_netguard_rate_limit_wait_seconds{host}`
- `angelia_source_fetches_total{source,result="rate_limited"}`
- `angelia_check_lag_seconds`

**New:**

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `angelia_netguard_host_limit_rate` | gauge | `host`, `bucket` (`authenticated`, `anonymous`), `origin` (`builtin`, `config`, `default`) | effective rps |
| `angelia_netguard_host_limit_burst` | gauge | same | effective burst |
| `angelia_netguard_server_pauses_total` | counter | `host`, `reason` (`retry_after`, `ratelimit_reset`) | pauses set by server signals |
| `angelia_netguard_responses_total` | counter | `host`, `class` (`2xx`, `304`, `3xx`, `4xx`, `429`, `5xx`, `error`) | responses per attempt |
| `angelia_netguard_deadline_refusals_total` | counter | `host` | waits refused because the slot was after the caller's deadline |

The deadline refusals are the direct sign that a limit is too low for the demand (section 4).

The gauges are set once at start for the table hosts and for `other`, the fallback. The new counters go
through the existing `netguard.Observer` interface as new methods. That is a Go-internal change, not a
public contract.

### 9. Benchmark settings (#62, #216)

**Configuration.** The bench writes, next to its existing `http.allow_networks` entry, one limit for the
single fake-registry host. That host is the address the run actually uses: `127.0.0.1` in process mode,
the bridge gateway or the Docker Desktop address in container mode.

```yaml
http:
  allow_networks: [127.0.0.1/32]
  host_limits:
    hosts:
      127.0.0.1: {rps: 50, burst: 50}
```

**Why 50.** It is 1.5 times the 33.3 rps demand of 10,000 targets at the 5-minute interval, all on one
host. It stays under the fake GitHub limit (`rate_limit: {rps: 80}` in
[fakereg.yaml](../../bench/fakereg/fakereg.yaml)) and under the 100 rps ceiling.

**Reporting.** The report prints the effective limit next to the verdict, and `bench/load/README.md` says
that the result holds only with that limit.

**#216.** #216 measures the due-to-check delay with P95 < 2 minutes. The limit is set high enough that the
bench measures the scheduler, not the 5 rps fallback. A run with the limit at the default stays possible
(through a new harness variable such as `BENCH_HOST_RPS=5`) to show the cap (question 7).

**Workers.** If the run is still bound by the 4 workers rather than the limit, that is a finding for the
worker-count question of ADR 0009, not a reason to raise this limit.

## Consequences

### Positive

- Operators of large instances and private mirrors can size the limit to their demand without a rebuild.
  Benchmark 1 can measure the scheduler instead of the 5 rps fallback.
- Built-in budgets cannot be raised by mistake, and server signals keep the last word, so the change cannot
  make Angelia less polite towards hosts with published limits.
- Only the instance config can set it, and the bounds are tight, so less-trusted surfaces gain nothing.
- Raising a limit is visible in `config check`, in the start log and in the metrics. Too low a limit is
  visible through the deadline refusals.

### Negative

- **New public contract.** `http.host_limits` is a new config key, and its shape is hard to change after
  1.0.
- **Ceilings.** Operators of big internal mirrors may find 100 rps too low. Raising the ceiling later is
  additive; lowering it would break configs.
- **No split.** One limit per entry cannot express a different anonymous budget for a private host.
- **Per-process limits.** Several processes in an HA deployment multiply the effective rate, which the
  operator must account for.
- **More metrics.** Five new series families, all bounded by the limits table.

### Open questions

- Whether the scheduler should log, or expose, an estimate of the demand per host against its limit.
  That would be a follow-up story (question 9).
- Whether the per-host body cap and timeout (`MaxBodyBytes`, `Timeout`) should also be configurable,
  for npm or PyPI mirrors with large documents. That is a separate story with its own bounds.
- The worker count and `CheckTimeout` in the instance config remain ADR 0009's open question.

## Alternatives considered

- **Raise the built-in fallback (for example to 20 rps).**
  - It needs no new config key, but it raises every host at once: small self-hosted mirrors, webhook
    receivers and registries with unpublished limits.
  - It would still not cover 33 rps on one host.
  - Politeness defaults should be conservative, and the operator who needs more should say so.
- **Higher built-in limits for `pypi.org` and `registry.npmjs.org`.** Neither publishes a budget to derive
  a number from. It does not help mirrors or the benchmark's loopback registry, and it would change
  behaviour for every instance.
- **A per-source key (`sources.<name>.rate_limit`).** Buckets are per host, not per source:
  - `github-release` and `github-tag` share `api.github.com`;
  - two named npm instances can point at one mirror;
  - sinks and plugins hit the same hosts.

  Two sources setting different limits for one bucket would conflict. It may come later as a shortcut
  that fills the host table.
- **Settable from the watch file or the API.** Refused (section 5). It would let less-trusted authors aim
  the instance's request budget, and change the politeness of a shared client.
- **Adaptive limits only (AIMD on 429 and rate-limit headers), with no config.**
  - Many registries and mirrors send no such signal until they block.
  - It reacts after the harm.
  - It is harder to test and to explain.

  It could complement static limits later.
- **Suffix or wildcard matching.** Refused for v1 (section 3): it is ambiguous between a shared bucket and
  one bucket per name, and the per-name reading lets DNS names multiply the budget.
- **A shared, database-backed limiter for HA.** It would make limits global across processes, at the cost
  of a store round trip per request or per token batch. It is not needed until HA deployments exist.
- **No limit for allowlisted private networks.** It would conflate the SSRF allowlist with politeness.
  Internal mirrors (Nexus, Artifactory) are often the most fragile hosts.
- **A bench-only flag that turns the limiter off.** The benchmark should measure the product as operators
  run it, with a documented setting, not a code path that ships unused.

## Questions for the maintainer

**Answered 2026-11-08:** the maintainer accepted every recommendation: 1(a), 2(a), 3(a), 4(a), 5(a), 6(a), 7(a), 8(a), 9(a). The text below is kept as the record of the options.

1. **Built-in budget hosts** (`api.github.com`, `gitlab.com`, `crates.io`).
   - (a) Entries may only lower them; a higher value is capped, with a warning.
   - (b) Entries may raise them, with a warning.
   - (c) Raising needs an explicit per-bucket form.

   **Recommendation: (a).** Their budgets are published, so going above them only produces 403/429. A
   GitHub Enterprise budget can come as a later, explicit option.
2. **`default` key.**
   - (a) Include it, capped at 20 rps / burst 20.
   - (b) Include it, lower-only.
   - (c) Leave it out; only named hosts.

   **Recommendation: (a).** Some operators want to be stricter everywhere, and a few want a little more.
   The low cap bounds what watch-file-chosen hosts can get.
3. **Upper bound for named hosts.**
   - (a) A hard cap of 100 rps / burst 200 (errors above).
   - (b) No cap, with a warning above 100.
   - (c) Another value.

   **Recommendation: (a).** It covers 3 times the benchmark demand on one host, and raising the cap later
   is additive.
4. **Wildcards.**
   - (a) None in v1.
   - (b) `*.example.com` with one bucket per matched host.
   - (c) `*.example.com` with one shared bucket.

   **Recommendation: (a)**, for the reasons in section 3.
5. **Separate authenticated and anonymous limits per entry.**
   - (a) One limit for both buckets in v1.
   - (b) Optional `authenticated:` and `anonymous:` sub-blocks now.

   **Recommendation: (a).** No use case yet. Adding the sub-blocks later is additive.
6. **Units.**
   - (a) `rps` (decimal) only.
   - (b) Also a budget form, `{requests: 5000, per: 1h, burst: 10}` (`netguard.Budget`).

   **Recommendation: (a)**, simpler to validate and to read. The budget form can be added later.
7. **Benchmark shape.**
   - (a) One fake host at 50 rps.
   - (b) One loopback address per ecosystem at 10 rps each, closer to real registries.

   **Recommendation: (a)** for #216, with a run at the default limit kept as information. (b) can follow
   if the maintainer wants per-registry realism.
8. **Environment override.**
   - (a) None.
   - (b) `ANGELIA_HTTP_HOST_LIMITS` as JSON.

   **Recommendation: (a)**, consistent with other map-shaped keys.
9. **Demand warning.**
   - (a) A follow-up story where the scheduler logs, and exposes, the estimated demand per host against
     its limit.
   - (b) Nothing.

   **Recommendation: (a)**, as a separate `Agent` story after this ADR is accepted.

## References

- [architecture.md §3.2](../architecture.md#32-scheduler-and-workers),
  [§6](../architecture.md#6-language-and-framework-phase-0-assumptions),
  [§8](../architecture.md#8-security); [prd.md](../prd.md) (politeness)
- [configuration.md, Outgoing HTTP](../configuration.md#outgoing-http-and-private-networks)
- [internal/netguard/limiter.go](../../internal/netguard/limiter.go),
  [client.go](../../internal/netguard/client.go)
- [ADR 0009](0009-scheduler-target-leases-and-backoff.md) (no per-host throttling in the scheduler),
  [ADR 0011](0011-wasm-plugin-host-prototype.md) (plugin requests through netguard),
  [ADR 0019](0019-prometheus-metrics-on-the-listen-address.md) (closed `host` label),
  [ADR 0023](0023-webhook-sink-standard-webhooks.md) (sinks share the client)
- angelia-dev/angelia#62 (maintainer decisions of 2026-11-08), #215 (first 10k run), #216 (due-to-check
  delay)

## Amendment 2026-11-08: implementation notes (#219)

The decision is unchanged. Implementing it (angelia-dev/angelia#219) settled these details, which the
sections above leave open or state differently:

- **Benchmark host (§9).** The bench sets the limit on the host name of the fake registry's URL, not on
  an IP address: `127.0.0.1` in process mode, `host.docker.internal` in container mode. The limiter keys
  its buckets on `URL.Hostname()`, so a limit on the bridge gateway or Docker Desktop address would never
  apply to requests sent to `host.docker.internal`. `BENCH_HOST_RPS` (default 50) and `BENCH_HOST_BURST`
  (default `ceil(rps)`) set it; `BENCH_HOST_RPS=5` runs at the default limit.
- **IPv6 entries (§3).** An IPv6 address must be written in its canonical form (`::1`, not `0:0::1`), as
  `netip.Addr.String()` prints it, because matching is by exact spelling; the error names the canonical
  form. An address with a zone (`fe80::1%eth0`) is refused.
- **Warning on built-in budgets (§4, §6).** One entry applies to both buckets. The warning compares the
  entry with the authenticated bucket, the higher built-in limit of every built-in host. The anonymous
  bucket is capped at its own lower budget without a warning; `config check` lists both effective
  buckets.
- **A host named `other` (§8).** It is a valid host name and is accepted, but its metrics share the
  `host="other"` label of every unlisted host. `config check` and the `serve` start log warn about it.
