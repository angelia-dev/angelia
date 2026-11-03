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

## `angelia serve`

`angelia serve` runs the selected roles in one process.

- `--roles=api,scheduler,worker,dispatcher` (or `all`) overrides `roles` from the config. `all` cannot be combined with other roles.
- The process serves `/healthz` and `/readyz` on the `listen` address. Roles that expose HTTP routes mount them on the same port.
  - `/healthz` returns 200 while the process is up.
  - `/readyz` returns 503 until the instance is ready (database reachable, migrations applied) and again as soon as shutdown starts, so load balancers stop sending new work.
- The same address serves Prometheus metrics at `/metrics` (see [operations/metrics.md](operations/metrics.md)); `--metrics=false` turns it off.
- On SIGTERM or SIGINT the roles are cancelled and get `--drain-timeout` (default 25s) in total to finish or release in-flight work, then the process exits 0. If they do not stop in time, or a role fails or exits on its own, the process exits non-zero. Keep the drain timeout below the Kubernetes `terminationGracePeriodSeconds` (30s by default).
