# The `angelia` command-line client

`angelia watch|sink|sub|event|delivery` talk to a running instance through the
[REST API](../api/README.md). The client is generated from the vendored OpenAPI
document (`internal/apiclient`, `go generate ./...`).

```
angelia watch list [--source pypi] [--label team=platform] [--limit N] [--cursor C | --all]
angelia watch get NAME
angelia watch put NAME -f watch.yaml [--dry-run]
angelia watch delete NAME [--dry-run]
angelia watch versions NAME [--prerelease=true|false] [--yanked=true|false]

angelia sink list|get|put|delete        angelia sub list|get|put|delete
angelia event list [--after SEQ] [--watch W] [--type T] [--source S] [--all]
angelia event get ID
angelia event stream [--after SEQ] [--watch W] [--type T] [--source S] [--no-reconnect]
angelia delivery list [--status dead] [--subscription S] [--event ID]
angelia delivery get ID
```

`put` reads a JSON or YAML file (`-f -` for stdin) holding the body of the
matching `PUT` operation. It is idempotent: the change the server made
(`created`, `updated`, `unchanged`) is printed on stderr.

## Connection

Each value comes from the first source that sets it:

| | server | token |
|---|---|---|
| 1. flag | `--server URL` | `--token TOKEN` (visible in process lists; prefer 2 or 3) |
| 2. environment | `ANGELIA_SERVER` | `ANGELIA_TOKEN` |
| 3. client config file | `server:` | `token:` |
| 4. default | `http://localhost:8080` | none |

The client config file is `$XDG_CONFIG_HOME/angelia/config.yaml`, or
`~/.config/angelia/config.yaml`:

```yaml
server: https://angelia.example.com
token: <api token>
```

It is not the instance configuration of `angelia serve` (`--config`). A file
that holds a `token` must not be readable by group or others (`chmod 600`);
otherwise the command fails, as `ssh` does for a private key. The token is
never printed, not even in errors.

## Output

`-o table` (default) prints aligned columns for a list and a `FIELD VALUE`
table for one resource. `-o json` prints the API's JSON: for a list the page
(`items`, `next_cursor`; for events `items`, `next_after`, `has_more`), and for
`event stream` one record per line. Tables go to stdout; hints (the cursor of
the next page, the change a `put` made) go to stderr.

## Shell completion

```
source <(angelia completion bash)     # also zsh, fish, powershell
```
