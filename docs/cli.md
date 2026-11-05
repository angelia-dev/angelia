# The `angelia` command-line client

`angelia watch|sink|sub|event|delivery|apply` talk to a running instance through the
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
angelia apply -f FILE|DIR|- [--dry-run] [--prune -l key=value]
```

`put` reads a JSON or YAML file (`-f -` for stdin) holding the body of the
matching `PUT` operation. It is idempotent: the change the server made
(`created`, `updated`, `unchanged`) is printed on stderr.

## Apply

`angelia apply` is config as code: it makes the instance match watch files.

```
angelia apply -f watches.yaml --dry-run                 # show the plan, write nothing
angelia apply -f watches.yaml                           # create and update
angelia apply -f config/ --prune -l team=platform       # also delete what left the files
```

- **Format.** The [watch file](watch-file.md) of `angelia serve --watches`, as it
  is: `version: 1` and the lists `sinks`, `watches`, `subscriptions`. Nothing is
  deprecated and no `kind:` key is needed. `-f` takes a file, a directory (its
  `.yaml`, `.yml` and `.json` files, sorted, not recursive) or `-` for stdin,
  and may be repeated. A file may hold several YAML documents, each a watch
  file. Names must be unique per kind across all of them. The structure is
  checked before any request; each object is then validated by the server.
- **Order.** One `PUT` per object: every sink, then every watch, then every
  subscription, each in file order; `--prune` deletes afterwards, in reverse
  order (subscriptions, watches, sinks). The sequence is not atomic: a failure
  stops the run, and the objects before it stay applied. Running apply again
  after a fix continues.
- **Idempotent.** `PUT` replaces the whole object, and the server answers
  `unchanged` when nothing differs, so applying the same files twice changes
  nothing.
- **Report.** One line per object, `kind/name created|updated|unchanged|deleted|failed`,
  then a count. An `updated` line is followed by the changed fields, as dotted
  paths with the old and new value (`interval: "1h" -> "30m"`), compared on what
  the API returns, with every default filled in. `-o json` prints the same as
  `{"dry_run", "changes": [{"kind", "name", "change", "diff", "note", "error"}]}`.
- **`--dry-run`** sends every `PUT` with `?dry_run=true`, so the server validates
  each object and says what it would do, and writes nothing. It reports every
  failure instead of stopping at the first. Two limits: a subscription on a sink
  that the run would create is reported without server validation (the sink does
  not exist yet; the line says so), and deletions are computed from the lists,
  without a request.
- **Labels.** A watch's `labels` come from the file; `watch list --label k=v` finds them.

### Prune scope

`--prune` needs a label scope, `-l key=value` (repeatable; every pair must
match). Only objects created through the API (`managed_by: api`) and absent
from the files are deleted; objects loaded by `angelia serve --watches`
(`managed_by: file`) are never touched. In API v1 only watches carry labels, so
the scope reaches sinks and subscriptions through the watches:

| Object | Deleted when |
|---|---|
| watch | its `labels` carry the scope |
| subscription | its `filter.labels` carry the scope: it can only route events of watches in the scope |
| sink | at least one subscription in the scope used it before the apply, and no subscription uses it after |

A sink that no subscription uses is never pruned: nothing ties it to the scope.
Delete it with `angelia sink delete`. Apply warns on stderr about a watch or
subscription in the files that lacks the scope's labels, because a later prune
with that scope would never delete it.

Sinks need a token with the `admin` scope, and a sink's `secret_env` must be
listed in the instance's `api.allowed_env`; watches and subscriptions need
`write`. An example is [examples/apply.yaml](../examples/apply.yaml).

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
otherwise the command fails (the idea of ssh's private key check, but weaker:
only the mode of the file is checked, not its owner or its directory). The
check runs even when a flag or variable supplies the token. The token is never
printed, not even in errors, and an invalid server URL is never echoed.

### Plain http

The token is sent as a bearer header, so with `http://` it travels in clear
text. The client refuses to send a token over `http://` to a host that is not
loopback (`localhost`, `127.0.0.0/8`, `::1`) and names `--insecure-http`, which
allows it for one command. Use an `https://` server URL instead.

## Event stream

`event stream` reconnects without limit after a connection error, a clean end
of stream, 5xx and 429 answers (honouring `Retry-After`), resuming after the
last event seen, and prints one `reconnecting in ...` line on stderr each time.
The wait is the server's `retry:` hint, clamped to 100 ms-5 min. A connection
silent for 45 s (three keep-alives) is dropped and reopened. Other 4xx answers
(401, 403, 410) end the command. Ctrl-C stops it. `--no-reconnect` exits when
the stream ends.

`--all` of `event list` and the other lists stops with an error when the server
repeats a cursor or `next_after` does not grow.

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
