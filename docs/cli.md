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
angelia apply -f config/ --prune -l team=platform       # also delete watches that left the files
```

- **Format.** Apply reads the [watch file](watch-file.md) format of
  `angelia serve --watches` unchanged: `version: 1` and the lists `sinks`,
  `watches`, `subscriptions`. The format is not deprecated and prints no
  warning, and apply needs no new key (no `kind:`). A file that apply accepts is
  also a valid `--watches` file. Each file holds exactly one YAML document, as
  the watch file requires. To split a configuration, use several files: `-f`
  takes a file, a directory (its `.yaml`, `.yml` and `.json` files, sorted, not
  recursive) or `-` for stdin (one document), and may be repeated. Names must
  be unique per kind across all files. Apply checks the structure, names and
  label types before it sends any request; the server then validates each
  object.
- **Order.** One `PUT` per object: every sink, then every watch, then every
  subscription, each in file order. `--prune` deletes watches afterwards. The
  sequence is not atomic: a failure stops the run, and the objects before it
  stay applied. A run that failed does not prune. Run apply again after a fix to
  continue. The error names the object and the server's reason.
- **Idempotent.** `PUT` replaces the whole object, and the server answers
  `unchanged` when nothing differs, so applying the same files twice changes
  nothing.
- **Report.** Each object gets one line,
  `kind/name created|updated|unchanged|deleted|kept|skipped|failed`, and a
  count line ends the report. An `updated` line is followed by the changed
  fields, as dotted paths with the old and new value
  (`interval: "1h" -> "30m"`). They are compared on what the API returns, with
  every default filled in. `-o json` prints the same report as
  `{"dry_run", "changes": [{"kind", "name", "change", "diff", "note", "error"}]}`.
- **`--dry-run`** sends every `PUT` with `?dry_run=true`. The server validates
  each object and says what it would do, and nothing is written. A dry run
  reports every failure instead of stopping at the first. It has two limits:
  - A subscription on a sink that the same run would create is reported
    without server validation, because the sink does not exist yet. Its line
    says so.
  - Deletions are computed from the lists, without a request.
- **Labels.** A watch's `labels` come from the file; `watch list --label k=v` finds them.

### Prune scope

`--prune` needs a label scope, `-l key=value` (repeatable; every pair must
match). **It deletes watches only:** a watch is deleted when it is managed by
the API (`managed_by: api`), carries every pair of the scope in its `labels`,
and is not in the files. Objects loaded by `angelia serve --watches`
(`managed_by: file`) are never touched.

Apply never deletes sinks or subscriptions. In API v1 they carry no owner labels.
A subscription's `filter.labels` say which events it wants, not who owns it, so
another team's subscription can match your scope. Apply reports the likely
leftovers as `kept`, each with the command that deletes it:

- each API subscription not in the files whose `filter.labels` carry the scope;
- each API sink not in the files that no subscription uses.

Delete them with `angelia sub delete NAME` and `angelia sink delete NAME` if
they are yours. Owner `labels` on sinks and subscriptions are a planned
(additive) change of the API specification; with them, prune can cover all
three kinds.

Safety rules:

- A real `--prune` run is refused when the files hold no watch in the scope.
  Without this rule, an empty file or a wrong `-f` or `-l` would delete every
  watch in the scope. `--prune-allow-empty` overrides the check. A dry run only
  warns.
- Apply warns on stderr about a watch in the files that lacks the scope's
  labels: it is applied, but a prune with that scope would never delete it.
- Apply reads the lists once, before the `PUT`s. Right before deleting a watch,
  it reads the watch again and keeps it (`kept`) if it is no longer managed by
  the API or left the scope. A short window between that read and the
  `DELETE` remains, because API v0 has no `If-Match`. Do not run two applies
  with overlapping scopes at the same time.
- A watch already gone is reported as `deleted (already gone)`. On any other
  failure the prune stops: the failed watch is `failed`, the rest are `skipped`,
  and the error lists every watch that was not deleted.

Sinks need a token with the `admin` scope, and a sink's `secret_env` must be
listed in the instance's `api.allowed_env`. Watches and subscriptions need
`write`. An example is [examples/apply.yaml](../examples/apply.yaml).

### Moving from `serve --watches` to `apply`

A name is unique per kind across owners, and the API refuses to change
file-managed objects. So `apply` on objects that `serve --watches` still loads
fails with `409 managed-by-file`, or `name taken` at the next startup. To move
objects to the API:

1. Remove them from the `--watches` file (or drop `--watches`) and restart
   `angelia serve`. The startup apply deletes the file-managed rows:
   - pending deliveries of the removed subscriptions become `dead` with
     `subscription removed`;
   - a watch's target and its known versions are deleted when no other watch
     uses the target;
   - events and deliveries already recorded stay as history.
2. Run `angelia apply` with the same objects right away. They are created anew,
   with new ids. Events of a new watch carry the new id in `data.watch.id`.
   Subscriptions start with no deliveries.

**Version baseline.** A new watch starts with `baseline_done` false, and a
watch's first check is always a baseline ([policy.md](policy.md)): the versions
known at that check are recorded without events. When the old target was
deleted in step 1, a version published between that restart and the new
watch's first check is recorded as part of the baseline and emits no event.
Keep the gap between the restart and the apply short.

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
