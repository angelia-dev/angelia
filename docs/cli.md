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

## Dead letters and replay

```
angelia deliveries list --status dead [--subscription S] [--event ID] [--all]
angelia replay --delivery ID
angelia replay --event ID [--status dead,delivered] [--dry-run]
angelia replay --subscription NAME --since T [--until T] [--status dead,delivered] [--dry-run]
```

A delivery that used up its retries is `dead`; `deliveries list --status dead`
is the dead-letter view. `replay` queues deliveries again
(`POST /v1/deliveries/{id}/replay` for `--delivery`, `POST /v1/replays`
otherwise). A replay queues the **existing** delivery again, due now: same id,
same idempotency key, `attempts` keeps counting and the retry budget starts
again from the replay (spec decision 28). A receiver that deduplicates on the
idempotency key may drop a replayed delivery it already accepted.

- `--event` replays the deliveries of one event; `--subscription` with
  `--since` replays those of one subscription whose event `time` is in
  `[since, until)`. The two can be combined.
- Only `dead` deliveries are replayed unless `--status` adds `delivered`.
  `pending` and `in_flight` deliveries, and those whose subscription or sink is
  gone, are skipped; `--delivery` on one of them fails with
  `409 not-replayable`.
- `--since` and `--until` take an RFC 3339 time, `now`, or a duration back from
  now (`24h`, `90m`, `7d`). `--until` defaults to now. Times are sent in UTC,
  cut to milliseconds.
- `--dry-run` shows what would be replayed and queues nothing. The API has no
  dry run for a single delivery, so `--delivery` refuses it (and the other
  selectors).

The output says how many deliveries were queued again and lists them (the
server lists at most 100; the summary line says when the list is cut). With
`-o json` it is the API's `ReplayResult` (`replayed`, `dry_run`, `deliveries`,
`truncated`); `--delivery` prints its one delivery in the same shape.

Replay needs a token with the `write` scope. The server writes one audit
record per replay that ran (not for a dry run or a refused request): `"audit":
true`, `"action": "replay"`, the `principal` (token id) and `request_id`, the
selector as given (`delivery`, or `event`, `subscription`, `since`, `until`
and `status`), the count `deliveries` and the first 100 `ids`.

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
