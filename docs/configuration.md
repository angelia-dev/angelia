# Instance configuration

Settings of one Angelia process. Watches, sinks and subscriptions are separate: they live in the [watch file](watch-file.md) (Phase 0 format: approved, but may still change before 1.0)
(`angelia serve --watches`, later `angelia apply`). A watch's `policy` keys are described in [policy.md](policy.md).
Precedence, lowest to highest: built-in default, YAML file, `ANGELIA_*` environment variable.
Check a configuration with `angelia config check [--config FILE]`.

> **For the maintainer to confirm:** the defaults, allowed values and env spellings below were
> chosen conservatively where the issue (#44) did not specify them. They become a public contract
> once released.

| Key | Env override | Default | Allowed |
|---|---|---|---|
| `instance.name` | `ANGELIA_INSTANCE_NAME` | `angelia` | `^[a-z0-9][a-z0-9.-]{0,62}$` |
| `listen` | `ANGELIA_LISTEN` | `:8080` | `host:port` |
| `database.dsn` | `ANGELIA_DATABASE_DSN` | `sqlite:///var/lib/angelia/angelia.db` | `sqlite://`, `postgres://`, `postgresql://` URL |
| `roles` | `ANGELIA_ROLES` (comma-separated) | `[all]` | `api`, `scheduler`, `worker`, `dispatcher`, `all` |
| `secrets.key_file` | `ANGELIA_SECRETS_KEY_FILE` | empty | path (encryption itself is Phase 1) |
| `log.level` | `ANGELIA_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `log.format` | `ANGELIA_LOG_FORMAT` | `json` | `json`, `text` |
| `http.user_agent` | `ANGELIA_HTTP_USER_AGENT` | `angelia/<version>` | non-empty, single line |
| `sources.<kind>.base_url` | `ANGELIA_SOURCES_<KIND>_BASE_URL` | empty | absolute http(s) URL |
| `sources.<kind>.token_env` | `ANGELIA_SOURCES_<KIND>_TOKEN_ENV` | empty | name of an environment variable |
| `plugins.<kind>.allowed_hosts` | none | empty | lower-case hostnames or IP literals, added to the plugin manifest's `allowed_hosts` |
| `plugins.<kind>.credentials.<host[:port]>.token_env` | none | none | name of an environment variable; the host must be in the merged `allowed_hosts`; no port means 443 |
| `plugins.<kind>.credentials.<host[:port]>.scheme` | none | empty | one word such as `Bearer`, written before the token |

- In env names, `<KIND>` is upper-case and `-` becomes `_` (`github-release` -> `GITHUB_RELEASE`).
- Tokens are never configured inline: `token_env` names the variable that holds the token. A
  `token:` key is an unknown key and an error.
- `instance.name` names this instance. It is the `<instance>` in the `source` of every release event,
  `angelia://<instance>/watches/<watch_id>` (see the
  [event contract](architecture.md#4-the-event-contract-the-product)). Pick a stable name: changing it
  changes the `source` of later events, and consumers that key on `source` see them as coming from a
  new instance. Use a different name per instance if events from several instances reach
  one consumer.
- The config file is optional; select it with `--config` or `ANGELIA_CONFIG`.
- Unknown keys are errors reported as `<file>: line N: ...`.
- Environment variables that are set but empty still override (`ANGELIA_LISTEN=` is then an invalid
  value); for `token_env`, an empty variable counts as unset and `config check` warns.
- The `ANGELIA_SOURCES_` prefix is reserved for source overrides; only names ending in `_BASE_URL`
  or `_TOKEN_ENV` are read, others are ignored (so a `token_env` may be called e.g.
  `ANGELIA_SOURCES_GITHUB_TOKEN`). Source kinds are lower-case `[a-z0-9]` words joined by hyphens.
- `roles`: `all` cannot be combined with other roles; duplicates are removed. `listen` port is 1-65535.
  A sqlite DSN needs a path, a postgres DSN a host.
- `base_url` must not contain credentials, and `token_env` must be a variable name: values that
  look like tokens (`ghp_`, `gho_`, `ghs_`, `github_pat_`, `glpat-`, `npm_`, `pypi-` prefixes) are
  rejected without being echoed. Tokens print as `[REDACTED]` in logs and `%v`.
- A config file must contain a single YAML document.

## Installed plugins (draft)

> **Draft, for the maintainer to confirm (#145):** the `plugins` keys follow the WASM plugin host prototype
> ([ADR 0011](adr/0011-wasm-plugin-host-prototype.md), amendment of #145) and may change with it. `angelia serve`
> does not load plugins yet.

`plugins.<kind>` configures the plugin whose manifest declares `kind`, when it is installed:

```yaml
plugins:
  crates:
    allowed_hosts: [crates-mirror.example.com]   # added to the manifest's list
    credentials:
      crates-mirror.example.com:
        token_env: CRATES_MIRROR_TOKEN           # the variable holding the token (port 443)
        scheme: Bearer                           # optional: "Authorization: Bearer <token>"
      crates-mirror.example.com:8443:            # the same host on another port
        token_env: CRATES_MIRROR_TOKEN
```

- Host names: matching is case-insensitive and ignores a trailing dot, everywhere. In this file write them in
  canonical form (lower case, no trailing dot, canonical IP literal); anything else is an error, so two keys
  cannot name the same host.
- `allowed_hosts` extends the manifest's list, it never replaces it. Same rules: exact match on every hop,
  redirects included, any port, no scheme, port, path or wildcard. The merged list, with where each entry
  comes from and the ports that get a credential, is logged when the plugin is installed (`wasm plugin installed`).
- A credential is bound to one host and port: `<host>` means port 443, `<host>:<port>` (`[addr]:port` for
  IPv6) another port, for example a mirror on 8443. The host adds it as the `Authorization` header of every
  request to that host and port, over https only (a plain http request to it fails), replacing any
  `Authorization` the plugin set. It is never sent to another host or port, allowed or not, including after a
  redirect, and it is not in the plugin's config map.
- **Bind a credential only to a host that does not reflect the `Authorization` header** (in a body, a header,
  an error page). The host redacts echoes of the token as `[REDACTED]`, refuses `Range` requests and compressed
  bodies it cannot redact on credentialed requests, but that is best effort: an encoded or transformed echo
  reaches the plugin. See the [ADR 0011 amendment](adr/0011-wasm-plugin-host-prototype.md#amendment-2026-11-04-145-operator-allowed_hosts-and-host-injected-credentials).
- Credentials live only here, as an environment variable name; never in the watch file or the plugin config
  map. A `token:` key is an unknown key and an error. An unset or empty variable is a `config check` warning,
  and requests to that host then fail, as do tokens shorter than 8 bytes. The variable is read once, at
  start: a rotated token needs a restart. A secret-store reference will be an additional key (#75).
- There are no `ANGELIA_PLUGINS_*` environment overrides.

## `angelia serve`

`angelia serve` runs the selected roles in one process.

- `--roles=api,scheduler,worker,dispatcher` (or `all`) overrides `roles` from the config. `all` cannot be combined with other roles.
- The process serves `/healthz` and `/readyz` on the `listen` address. Roles that expose HTTP routes mount them on the same port.
  - `/healthz` returns 200 while the process is up.
  - `/readyz` returns 503 until the instance is ready (database reachable, migrations applied) and again as soon as shutdown starts, so load balancers stop sending new work.
- The same address serves Prometheus metrics at `/metrics` (see [operations/metrics.md](operations/metrics.md)); `--metrics=false` turns it off.
- On SIGTERM or SIGINT the roles are cancelled and get `--drain-timeout` (default 25s) in total to finish or release in-flight work, then the process exits 0. If they do not stop in time, or a role fails or exits on its own, the process exits non-zero. Keep the drain timeout below the Kubernetes `terminationGracePeriodSeconds` (30s by default).

## Tracing

OpenTelemetry tracing is **off by default** and is not configured in `angelia.yaml`. `angelia serve` turns it on when the standard OpenTelemetry environment variables ask for an OTLP exporter ([ADR 0020](adr/0020-opentelemetry-traces-link-checks-to-deliveries.md)):

| Variable | Effect |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | Enables tracing and names the collector. Without one, nothing is exported. |
| `OTEL_TRACES_EXPORTER` | Empty or `otlp` keeps it on; `none` (or anything else) turns it off. |
| `OTEL_SDK_DISABLED=true` | Turns it off. |
| `OTEL_EXPORTER_OTLP_HEADERS`, `_TIMEOUT`, `_COMPRESSION`, `_CERTIFICATE`, `_INSECURE` (and the `_TRACES_` variants) | Read by the exporter. |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES`, `OTEL_TRACES_SAMPLER[_ARG]` | Read by the SDK; the service name defaults to `angelia`. |

- Only `http/protobuf` is supported (the OTLP default). `OTEL_EXPORTER_OTLP_PROTOCOL=grpc` makes `serve` fail at start.
- One trace per check (`angelia.check`, with `angelia.http` and `angelia.commit` below it). Each delivery attempt is a trace of its own (`angelia.delivery`) with a span link to the check that created it.
- Spans hold ids, kinds, status codes and counts, and the host of a request only if it has an entry in the netguard limits table (else `other`). They never hold a URL, a header, a sink configuration or an error message.
- When tracing is on, the final flush (at most 2 s) is taken out of `--drain-timeout`, so the drain gets 2 s less. Export failures are logged as warnings, at most one per 30 s.
- Log lines written with a span in their context carry `trace_id` and `span_id`.
