# Instance configuration

Settings of one Angelia process. Watches and subscriptions are separate (`angelia apply`).
Precedence, lowest to highest: built-in default, YAML file, `ANGELIA_*` environment variable.
Check a configuration with `angelia config check [--config FILE]`.

> **For the maintainer to confirm:** the defaults, allowed values and env spellings below were
> chosen conservatively where the issue (#44) did not specify them. They become a public contract
> once released.

| Key | Env override | Default | Allowed |
|---|---|---|---|
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
- The config file is optional; select it with `--config` or `ANGELIA_CONFIG`.
- Unknown keys are errors reported as `<file>: line N: ...`.
