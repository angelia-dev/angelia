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
| `secrets.key_file` | `ANGELIA_SECRETS_KEY_FILE` | empty | path to the key file; see [Stored secrets](#stored-secrets) |
| `secrets.previous_key_files` | `ANGELIA_SECRETS_PREVIOUS_KEY_FILES` (comma-separated) | empty | paths of older key files, kept during a rotation |
| none | `ANGELIA_SECRETS_KEY` | unset | the key itself (base64), instead of `secrets.key_file` |
| `log.level` | `ANGELIA_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `log.format` | `ANGELIA_LOG_FORMAT` | `json` | `json`, `text` |
| `http.user_agent` | `ANGELIA_HTTP_USER_AGENT` | `angelia/<version>` | non-empty, single line |
| `http.allow_networks` | `ANGELIA_HTTP_ALLOW_NETWORKS` (comma-separated) | empty | list of CIDR prefixes (`10.0.0.0/8`, `fd00::/8`); see [Outgoing HTTP](#outgoing-http-and-private-networks) |
| `http.allow_http_hosts` | `ANGELIA_HTTP_ALLOW_HTTP_HOSTS` (comma-separated) | empty | list of lower-case host names or IPv4 addresses, no scheme or port |
| `sources.<kind>.base_url` | `ANGELIA_SOURCES_<KIND>_BASE_URL` | empty | absolute http(s) URL |
| `sources.<kind>.token_env` | `ANGELIA_SOURCES_<KIND>_TOKEN_ENV` | empty | name of an environment variable, never one starting with `ANGELIA_` |
| `plugins_dir` | `ANGELIA_PLUGINS_DIR` | `<data dir>/plugins` | absolute path; see [Installed plugins](#installed-plugins) |
| `api.allowed_env` | none | empty | list of environment variable names; never a name starting with `ANGELIA_` |

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
  or `_TOKEN_ENV` are read, others are ignored. Source kinds are lower-case `[a-z0-9]` words joined by
  hyphens.
- **Security restriction:** no `token_env` (sources, plugin `credentials`) and no `*_env` key (watch file,
  API) may name a variable starting with `ANGELIA_`. Those variables hold the instance's own settings,
  among them the secrets key `ANGELIA_SECRETS_KEY`, which a reference would otherwise send to a remote host
  as a token. Such a name fails the check with `names starting with ANGELIA_ are always refused`.
- `roles`: `all` cannot be combined with other roles; duplicates are removed. `listen` port is 1-65535.
  A sqlite DSN needs a path, a postgres DSN a host.
- `base_url` must not contain credentials, and `token_env` must be a variable name: values that
  look like tokens (`ghp_`, `gho_`, `ghs_`, `github_pat_`, `glpat-`, `npm_`, `pypi-` prefixes) are
  rejected without being echoed. Tokens print as `[REDACTED]` in logs and `%v`.
- A config file must contain a single YAML document.
- `api.allowed_env` lists the environment variables that a sink created or changed through the REST
  API may name in a `*_env` key (such as the webhook's `secret_env`). Names match exactly. The list
  is empty by default, so an API sink can name no variable until the operator allows one. A name
  starting with `ANGELIA_` is always refused, in this list and in the API, because those variables
  hold the instance's own settings. The API answers a refused name with
  `422 env-not-allowed`. The watch file is not limited by this list.

## Outgoing HTTP and private networks

Every request of a source or a sink goes through one HTTP client with an SSRF guard. It refuses
loopback, private, link-local, CGNAT (`100.64.0.0/10`), multicast, broadcast, `0.0.0.0/8`,
`192.0.0.0/24`, `198.18.0.0/15`, `240.0.0.0/4` and IPv6 ranges that embed or reach IPv4 addresses
(NAT64, 6to4, Teredo, IPv4-compatible).

- `http.allow_networks` lists the prefixes the guard accepts anyway, for example a homelab
  `192.168.1.0/24` or an internal `fd00:10::/32`. It applies to sources and sinks alike. Keep it narrow:
  a prefix of length 0 (`0.0.0.0/0`, `::/0`) turns the guard off for that address family, and
  `config check` warns about it.
- `http.allow_http_hosts` lists the hosts a webhook may reach over plain `http://` (watch-file decision
  D12). Every other webhook URL must be `https://`. Matching is by host name, case-insensitive.
- Both lists live only here: the watch file and the REST API cannot widen them.
- `angelia serve` builds one HTTP client from these keys, shared by the scheduler role (sources) and the
  dispatcher role (sinks): `allow_networks` is what both may dial. The webhook sink checks
  `allow_http_hosts` again at every delivery, so removing a host stops plain `http://` even for sinks
  stored while it was listed (a permanent failure). `config check`, the watch file and the API apply
  `allow_http_hosts` when a sink is written.
- Write IPv4 prefixes in IPv4 form: an IPv4-mapped prefix (`::ffff:10.0.0.0/104`) is refused, because
  the guard checks mapped addresses as IPv4.
- The guard checks the addresses a URL's host resolves to, then, for a direct connection, the address
  actually dialled, which defeats DNS rebinding. When `HTTPS_PROXY` or `HTTP_PROXY` is set (read once at
  start, with `NO_PROXY`), connections to the proxy are not checked, since proxies usually have private
  addresses, and only the URL check applies: the proxy resolves the target, so rebinding protection is
  its job.

See [webhooks.md](webhooks.md) for the webhook sink, [github-dispatch.md](github-dispatch.md) for the GitHub dispatch sink
and [shoutrrr.md](shoutrrr.md) for the chat and push sink. A `shoutrrr` sink derives its HTTP origins from its URL
secret; each is checked against `allow_http_hosts` and `allow_networks` at every delivery.

## REST API

The `api` role serves the REST API of the
[OpenAPI specification](https://github.com/angelia-dev/spec/blob/main/openapi/angelia.v1.yaml)
under `/v1/` on the `listen` address. The vendored copy is in `api/openapi/`.

- Every request needs a bearer token. **Token storage is not built yet** (story #72): until it is,
  `angelia serve` uses an authenticator that refuses every token, so every API request gets
  `401 unauthorized`. The health, readiness and metrics routes need no token.
- Bodies are limited to 1 MiB (`413 payload-too-large`). While the process drains, the API answers
  `503 unavailable` with `Retry-After: 5`, and open event streams are closed.
- Each request writes one `api request` record to the log with its request id, method, path, route,
  status, size, duration, principal and scope. Headers, the query string and bodies are never
  logged, and a path segment that looks like a credential is replaced with `[redacted]`. Replays
  also write an `api audit` record.

## Installed plugins

Each installed plugin has its own folder in `plugins_dir` ([ADR 0025](adr/0025-plugin-folder-and-grants-file.md)).
The folder name is the plugin's kind:

```
<plugins_dir>/
  crates/
    manifest.json   # from the plugin author: kind, allowed_hosts, config_schema (ADR 0011)
    plugin.wasm     # the module
    grants.yaml     # optional, from the operator: extra hosts and credentials
```

- `plugins_dir` defaults to `<data dir>/plugins`. The data directory is the directory of the sqlite database
  file (`/var/lib/angelia` with the default `database.dsn`), or `/var/lib/angelia` with Postgres. An empty
  `ANGELIA_PLUGINS_DIR` means the default. A missing `plugins_dir` means no plugins are installed. A
  `plugins_dir` that exists but cannot be read fails `config check` when you configured it, and is only a
  warning for the default (it may belong to the service user).
- Each folder must hold a `manifest.json` whose `kind` is the folder name. Entries whose name starts with `.`
  are skipped. Plain files and symbolic links are ignored with a warning: a plugin folder must be a real
  directory, not a link.
- **Ownership and mode** (not on Windows): `plugins_dir`, each plugin folder and `grants.yaml` must be owned by
  the user Angelia runs as, or by root, and must not be writable by group or others. A directory with the
  sticky bit may be group- or world-writable. `grants.yaml` must be a regular file, not a symbolic link.
  Anything else is refused with the `chmod` or owner to fix. Run `angelia config check` as the user Angelia
  runs as (for example `sudo -u angelia angelia config check`): run as root or another user, it refuses files
  owned by the service user.
- `angelia serve` does not load plugins yet: installing and running them is
  [#88](https://github.com/angelia-dev/angelia/issues/88), which uses this layout. Today `angelia config check`
  reads the folders and validates `grants.yaml`.
- The `plugins.<kind>` keys of the instance config (draft of #145) moved here. An instance config that still
  has a `plugins` key fails with `plugins: moved to the plugin's folder: write allowed_hosts and credentials in
  <plugins_dir>/<kind>/grants.yaml`.

### `grants.yaml`

```yaml
allowed_hosts: [crates-mirror.example.com, gitlab.example.com]   # added to the manifest's list
credentials:
  crates-mirror.example.com:
    token_env: CRATES_MIRROR_TOKEN           # the variable holding the token (port 443)
    scheme: Bearer                           # optional: "Authorization: Bearer <token>"
  crates-mirror.example.com:8443:            # the same host on another port
    token_env: CRATES_MIRROR_TOKEN
  gitlab.example.com:
    token_env: GITLAB_TOKEN
    header: PRIVATE-TOKEN                    # optional: another credential header
```

| Key | Default | Allowed |
|---|---|---|
| `allowed_hosts` | empty | lower-case hostnames or IP literals, added to the manifest's `allowed_hosts` |
| `credentials.<host[:port]>.token_env` | required | name of an environment variable, never one starting with `ANGELIA_`; the host must be in the manifest's or this file's `allowed_hosts`; no port means 443 |
| `credentials.<host[:port]>.scheme` | empty | one word such as `Bearer`, written before the token; only with the `Authorization` header |
| `credentials.<host[:port]>.header` | `Authorization` | a header netguard counts as a credential: `Authorization` or `PRIVATE-TOKEN` (any case) |

- Unknown keys are errors. There are no environment overrides.
- **File mode:** `grants.yaml` holds no secret (only variable names), but it grants network access, so the
  risk is tampering: a file that the group or others can write, or that another user owns, is refused
  (`chmod go-w grants.yaml`). Readable by others is fine.
- Host names: matching is case-insensitive and ignores a trailing dot, everywhere. In this file write them in
  canonical form (lower case, no trailing dot, canonical IP literal); anything else is an error, so two keys
  cannot name the same host.
- `allowed_hosts` extends the manifest's list, it never replaces it. Same rules: exact match on every hop,
  redirects included, any port, no scheme, port, path or wildcard. When the plugin host installs the plugin
  it also logs the merged list, with where each entry comes from and the ports that get a credential
  (`wasm plugin installed`).
- A credential is bound to one host and port: `<host>` means port 443, `<host>:<port>` (`[addr]:port` for
  IPv6) another port, for example a mirror on 8443. The plugin host adds it as the `Authorization` header (or
  the `header` you name) of every request to that host and port, over https only: a plain http request to it
  fails, even on a private network. It is never sent to another host or port, allowed or not, including after
  a redirect, and it is not in the plugin's config map.
- **A plugin cannot set a credential header itself.** A request in which the plugin sets `Authorization`,
  `PRIVATE-TOKEN` or another header netguard counts as a credential is refused with `bad_request`, on every
  host. Only credentials the host injects use a host's authenticated rate budget.
- **Bind a credential only to a host that does not reflect the credential header** (in a body, a header, an
  error page). The host redacts echoes of the token as `[REDACTED]`, refuses `Range` requests and compressed
  bodies it cannot redact on credentialed requests, but that is best effort: an encoded or transformed echo
  reaches the plugin. See the [ADR 0011 amendment](adr/0011-wasm-plugin-host-prototype.md#amendment-2026-11-04-145-operator-allowed_hosts-and-host-injected-credentials).
- Credentials live only here, as an environment variable name; never in the watch file or the plugin config
  map. A `token:` key is an unknown key and an error. An unset or empty variable is a `config check` warning,
  and requests to that host then fail, as do tokens shorter than 8 bytes. The variable is read once, at
  start: a rotated token needs a restart. Plugin credentials cannot name a stored secret yet.

### `angelia config check` output

`config check` lists every plugin with the hosts it may reach and the hosts that get a credential. It names
the variable that holds a token, never the token. For the `crates` plugin (its manifest allows `crates.io`)
with the `grants.yaml` above, `CRATES_MIRROR_TOKEN` set and `GITLAB_TOKEN` unset:

```
configuration OK
plugins in /var/lib/angelia/plugins: 1
  crates
    allowed hosts: crates.io (manifest), crates-mirror.example.com (grants), gitlab.example.com (grants)
    credentials:
      crates-mirror.example.com:443  Authorization: Bearer <token from $CRATES_MIRROR_TOKEN>
      crates-mirror.example.com:8443  Authorization: <token from $CRATES_MIRROR_TOKEN>
      gitlab.example.com:443  PRIVATE-TOKEN: <token from $GITLAB_TOKEN, not set>
secrets: no key configured (0 stored)
```

An invalid `grants.yaml` or manifest fails the check with `invalid plugins: ...`. The last line reports
the [secrets key](#stored-secrets) and how many stored secrets the database holds (read-only: the check
never creates or migrates the database).

## Stored secrets

Sink credentials can be stored in the instance, encrypted at rest with AES-256-GCM, and named from a sink
config with a `*_ref` key (such as the webhook's `secret_ref`). The design is
[ADR 0024](adr/0024-secrets-encrypted-at-rest.md). Values are write-only: no API response, CLI output or
log line contains one. Manage them with [`angelia secret`](cli.md#stored-secrets) (through the API, `admin`
scope) and the keys with `angelia secrets` (instance-side).

At every delivery attempt the dispatcher role resolves each `*_ref` key of the sink's config from the store, and
each `*_env` key from the environment, before the sink is called; values are not cached. A secret bound to
`hosts` is resolved only when the URL the sink sends to (the key its kind marks `x-angelia-destination`,
the webhook's `url`, the GitHub dispatch sink's `api_base_url`) has a host and port equal to one entry. A missing secret, a host outside the binding or
a value that cannot be decrypted fails the attempt before anything is sent, and it is retried; `last_error`
names the key and the secret (`dispatch: secret /secret_ref: ... secret ci-webhook does not exist`), never
the value. Sinks follow no redirect, so a bound secret never leaves its hosts; a sink kind that sent one
anywhere other than its destination would first need netguard's per-request check (ADR 0024 §2). The
binding compares host and port, not the scheme: `http://h:443/` matches a binding of `h`, which needs `h` in
`http.allow_http_hosts`.

> **Source and registry credentials** cannot reference stored secrets yet (#111). There, `hosts` will be
> mandatory (`422 secret-host-binding-required`) and every redirect hop will be checked against it.

### The key

- `secrets.key_file` (the documented default) holds **one standard-base64 encoding of 32 random bytes**
  (44 characters), optionally followed by one newline. Anything else is a startup error that never echoes
  the content. Create one with `angelia secrets keygen --out /etc/angelia/secrets.key` (created `0600`,
  never overwrites a file); without `--out` the key is printed to stdout, the only command that ever prints
  key material.
- `ANGELIA_SECRETS_KEY` holds the same value inline, for platforms that only inject environment variables.
  Setting it together with `secrets.key_file` is an error. After reading it the server calls
  `os.Unsetenv`. **That only keeps the key out of processes the server starts later**: it does not scrub
  `/proc/self/environ` (readable by the same user, and by root) or the container runtime's record of the
  environment (`docker inspect`, the pod spec). Prefer the file.
- `secrets.previous_key_files` lists older keys, used **only to decrypt**: during a rotation and to read a
  restored backup.
- **Back up the key file separately from the database.** Without it the stored secrets cannot be read; a
  backup of the database alone does not expose them.

### File permissions

On Unix the server opens the key file (following symlinks, as Kubernetes secret volumes are symlinks) and
checks the mode of the opened file. It must be a regular file. It is **refused** when other users can
read it or when group or others can write it (the error says `chmod 600`), and **accepted with a warning**
when the group can read it. Ownership is not checked. Working settings:

- **Kubernetes:** `defaultMode: 0400` on the secret volume, or `0440` with `fsGroup`. The default `0644` is
  refused.
- **Docker Swarm:** `secrets: [{source: angelia-key, target: secrets.key, mode: 0400, uid: "65532"}]`. The
  default `0444` is refused.
- **Docker Compose:** file secrets are bind mounts that keep the host file's mode and owner: `chown 65532`
  (the distroless nonroot user) and `chmod 0400` the host file.

### Without a key

Every role checks the key at startup, since all roles share the store.

| State | Outcome |
|---|---|
| No key, no stored secret | Starts. `config check` prints `secrets: no key configured (0 stored)`. |
| No key, stored secrets exist | Refuses to start: `secrets: 3 stored secrets but no key configured; set secrets.key_file`. There is no plaintext fallback. |
| A stored secret is sealed with a key that is not configured | Refuses to start, listing the unknown key ids and their row counts. |
| No key, a secret is written through the API | `422 secrets-key-missing`; nothing is stored. |
| A `*_ref` names a secret that does not exist | A warning when the watch file is applied; the API accepts it without a warning. Each delivery fails before it is sent, and is retried, until the secret is stored. |

The startup check reads SQLite databases; a Postgres store is not checked yet.

### Rotating the key

1. `angelia secrets keygen --out new.key`.
2. Set `secrets.key_file: new.key` and `secrets.previous_key_files: [old.key]`, and restart. With several
   nodes, first roll out `old.key` as the primary and `new.key` as a previous key everywhere, then swap
   them, so that every node can read what any other node writes.
3. `angelia secrets rotate` re-encrypts every stored secret that the primary key did not seal, in one
   transaction (`--batch N` for one transaction per N rows; `--dry-run` to only report). It reads the same
   config, needs the database, and writes an audit log line. A crash rolls back; rerunning is safe. A row
   changed concurrently through the API is retried up to 3 times; one that still loses is reported, and
   the command exits non-zero, to be rerun.
4. `angelia secrets status` prints the number of stored secrets per key id and which ids are configured.
   When the old key has 0 rows, remove it from `previous_key_files`. **Keep the old key file archived** as
   long as backups taken before the rotation are kept: restoring one needs it.

## `angelia serve`

`angelia serve` runs the selected roles in one process.

- `--roles=api,scheduler,worker,dispatcher` (or `all`) overrides `roles` from the config. `all` cannot be combined with other roles.
  - `scheduler` checks due targets through the sources (on its own pool of workers) and commits new versions, events and deliveries.
  - `dispatcher` sends due deliveries through the sinks, with retries and backoff ([ADR 0017](adr/0017-dispatcher-delivery-leases-and-retries.md)).
  - `api` serves the REST API. `worker` does nothing of its own yet: checks run in the `scheduler` role.
- Before any role starts, `serve` checks the [secrets key](#stored-secrets) against the stored secrets and, when a role opens the database (`api`, `scheduler`, `dispatcher`), refuses stored sinks that hold a literal value at a key their kind marks `x-angelia-secret` ([ADR 0024](adr/0024-secrets-encrypted-at-rest.md) §6).
- The process serves `/healthz` and `/readyz` on the `listen` address. Roles that expose HTTP routes mount them on the same port.
  - `/healthz` returns 200 while the process is up.
  - `/readyz` returns 503 until the instance is ready (database reachable, migrations applied) and again as soon as shutdown starts, so load balancers stop sending new work.
  - The container image's HEALTHCHECK runs `angelia healthcheck` (GET `http://127.0.0.1:8080/healthz`, exit 0 on a 2xx answer, 1 otherwise). Use `--url` when `listen` uses another port. See [deploy/compose](../deploy/compose/README.md).
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
