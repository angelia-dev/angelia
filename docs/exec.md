# Exec sink

The `exec` sink runs a local command once per delivery, with the event on its standard input. Use it
to hook a script into a release: restart a service, refresh a cache, open a ticket. Design and threat
cases: [ADR 0042](adr/0042-exec-sink.md).

Only the person who edits `angelia.yaml` decides what runs. A sink in the watch file, the API or the
admin UI can only **name** a command defined there; it can never set a path, an argument or an
environment variable. Without a command in `angelia.yaml` the `exec` kind does not exist at all.

Linux and macOS only. In the official container image it needs a derived image (see
[Containers](#containers)).

## Configuration

Define the commands in the instance configuration (`angelia.yaml`):

```yaml
exec:
  commands:
    deploy-hook:
      argv: [/usr/local/bin/deploy-hook, --env, staging]
      dir: /var/lib/angelia            # optional, absolute, default /
      env: [DEPLOY_TOKEN]              # optional, names copied from Angelia's environment
      timeout: 10s                     # optional, 1s to 12s
      max_concurrent: 1                # optional, 1 to 8
```

| Key | Default | Allowed |
|---|---|---|
| `exec.commands.<name>` | none | a name like a sink name, `^[a-z0-9][a-z0-9._-]{0,62}$` |
| `argv` | required | 1 to 64 entries; `argv[0]` is an absolute, clean path (no `PATH` lookup). Started directly: no shell, and no placeholder is expanded in any argument |
| `dir` | `/` | the working directory, an absolute path |
| `env` | empty | names of environment variables of the Angelia process copied to the command when set; never a name starting with `ANGELIA_`; each name once |
| `timeout` | `10s` | `1s` to `12s` |
| `max_concurrent` | `1` | `1` to `8`: runs of this command at the same time, shared by every sink naming it |

Unknown keys are errors, and there is no environment-variable override of `exec.commands`.
`angelia config check` and `angelia serve` (with the `dispatcher` role) fail when an `argv[0]` does not
exist, is not a regular file or is not executable, naming the command. `config check` warns when a
variable listed in `env` is not set. A script that needs a shell names one:
`argv: [/bin/sh, /etc/angelia/hooks/deploy.sh]`.

Then name the command from a sink, in the [watch file](watch-file.md#24-sinks) or through the API
(`PUT /v1/sinks/{name}`):

```yaml
version: 1
sinks:
  - name: deploy
    kind: exec
    config:
      command: deploy-hook
watches:
  - name: requests
    source: pypi
    coordinates:
      name: requests
subscriptions:
  - name: deploy-on-release
    sink: deploy
```

`config` has exactly one key, `command`. A name that is not under `exec.commands` is an error (`422`
through the API), and so is `kind: exec` when `angelia.yaml` defines no command.

## What the command receives

- **Standard input**: the message, then end of file. By default it is the CloudEvents JSON of the event
  in structured mode, the body a webhook gets; with a subscription `template` it is the template's
  output ([templates.md](templates.md)). A command that does not read it is fine.
- **Arguments**: exactly `argv`, nothing added.
- **Environment**: a clean one, not Angelia's:

  | Variable | Value |
  |---|---|
  | `PATH` | `/usr/local/bin:/usr/bin:/bin` (or Angelia's own `PATH` when `env` lists `PATH`) |
  | each name in `env` | its value in Angelia's environment, when set |
  | `ANGELIA_EVENT_ID` | the event's id |
  | `ANGELIA_EVENT_TYPE` | the event's type, such as `dev.angelia.release.published.v1` |
  | `ANGELIA_DELIVERY_ID` | the delivery's id |
  | `ANGELIA_IDEMPOTENCY_KEY` | `<event id>:<subscription id>`, the same on every attempt and replay |
  | `ANGELIA_ATTEMPT` | the attempt number, from 1 |
  | `ANGELIA_CONTENT_TYPE` | the media type of standard input, such as `application/cloudevents+json; charset=utf-8` |

Nothing from the event (package name, version, notes) is put in the arguments or the environment.

An example hook that reads the event with `jq`:

```sh
#!/bin/sh
# /usr/local/bin/deploy-hook --env staging
set -eu
event=$(cat)
package=$(printf '%s' "$event" | jq -r .data.package)
version=$(printf '%s' "$event" | jq -r .data.version)
# Several attempts of one delivery share ANGELIA_IDEMPOTENCY_KEY: skip a key already handled.
marker="/var/lib/angelia/hooks/$ANGELIA_IDEMPOTENCY_KEY"
[ -e "$marker" ] && exit 0
echo "deploying $package $version to $2" >&2
curl -fsS -H "Authorization: Bearer $DEPLOY_TOKEN" --data-binary "$event" https://deploy.example.com/hooks/release
touch "$marker"
```

## Outcomes and retries

| The command | The delivery | `last_error` |
|---|---|---|
| exits 0 | delivered | none |
| exits with another status | retried with backoff, like an HTTP 5xx | `exec deploy-hook: exit status 3` |
| is killed by a signal | retried | `exec deploy-hook: killed by signal 9` |
| runs past its timeout | killed, retried | `exec deploy-hook: timed out after 10s` |
| is cut by shutdown or the end of the lease | killed, retried | `exec deploy-hook: canceled: …` |
| cannot be started (missing, not executable) | dead at once | `exec deploy-hook: could not start: permission denied` |
| is not defined on this dispatcher node | retried, until the node defines it or the attempts run out | `exec: command "deploy-hook" is not defined under exec.commands in angelia.yaml` |

Retries follow the dispatcher's schedule ([ADR 0017](adr/0017-dispatcher-delivery-leases-and-retries.md)):
8 attempts, then the delivery is dead and can be replayed. Delivery is at least once, so a command can run
more than once for one delivery (a retry after a timeout, an attempt cut at shutdown): use
`ANGELIA_IDEMPOTENCY_KEY` to drop duplicates.

The output never reaches `last_error`, which every `read` token can see. On a failure, the last 1 KiB of
standard error is logged at `warn` with the sink and the command name; standard output is never logged.

## Limits

- **Timeout**: from `1s` to `12s`, because the dispatcher gives one attempt 15 s. The command never runs past
  the attempt's deadline minus 2 s; with less than 1 s left it is not started and the attempt is retried. A
  command that needs longer should hand the work off to a service (enqueue a job, call an HTTP endpoint) and
  exit. A background child does not work: see the next item.
- **Process group**: the command runs in its own process group. At the timeout, or when the attempt is cut
  (shutdown, lease end), the whole group gets `SIGTERM`, then `SIGKILL` 2 s later, so a child process that
  keeps a pipe open cannot hang the delivery. **When the command exits, whatever its status, everything left
  in its group is killed with `SIGKILL`**: a run leaves nothing behind, so `nohup … &` or a background job
  dies with it. A process that leaves the group (`setsid`, a daemon) is out of reach; do not do that, hand
  the work to a service instead.
- **If Angelia crashes**: on Linux the command itself (the direct child only, not its own children) is killed
  with it. In a container, run Angelia with `--init` (`docker run --init`, or tini as the entrypoint) so that
  a process left behind is reaped instead of staying a zombie under PID 1.
- **Output**: standard output and standard error are each kept to their last 64 KiB; the rest is read and
  discarded.
- **Concurrency**: `max_concurrent` runs of a command at a time; a delivery waiting for a free slot when its
  attempt ends is retried.

## Security model

- **Who decides what runs**: only `angelia.yaml`, read at start. A `write` token can choose which events reach
  an approved command and what a template puts on its standard input, nothing more.
- **The command runs as the Angelia user**, with its file access: it can read the database file and the secrets
  key file if that user can. Keep hooks small and owned by root (`root:root`, mode `0755`), so that the
  Angelia user cannot change them.
- **Standard input is untrusted**: package names, versions and release notes come from public registries. Parse
  it as JSON; never pass a field to `sh -c`, `eval` or an option position without quoting and `--`.
- **Environment**: the command gets a clean environment, not Angelia's (the secrets key, tokens, webhook
  secrets). List a secret the command needs, such as `DEPLOY_TOKEN`, in `env`. This does not stop a process of
  the same user from reading Angelia's own environment through `/proc/<pid>/environ`. On Linux, `angelia serve`
  marks itself non-dumpable at start (`prctl(PR_SET_DUMPABLE, 0)`): its `/proc/<pid>/environ` becomes owned by
  root and unreadable to the command (core dumps of Angelia are off as a consequence). On macOS a process of the
  same user can see Angelia's environment (`ps -E`). Where that matters, keep the command a thin trigger that
  hands the work to a service running as **another user**, and under systemd add `ProtectProc=invisible`.
- **Network**: Angelia's SSRF guard (`http.allow_networks`) applies to Angelia's own requests only, not to what
  the command does.
- **systemd**: the command inherits the unit's sandbox ([install.md](install.md#as-a-systemd-service)):
  `ProtectSystem=strict` makes the file system read-only except the state directory, `PrivateTmp`,
  `NoNewPrivileges`, no capabilities, `RestrictAddressFamilies`. Add `ReadWritePaths=` for a directory the hook
  writes to.

## Containers

The official image is distroless: no shell, no `jq`, no `curl`. An exec sink there needs a derived image with
the command, preferably a static binary:

```dockerfile
FROM ghcr.io/angelia-dev/angelia:latest
COPY --chmod=0755 deploy-hook /usr/local/bin/deploy-hook
```

Run it with `--init` (`docker run --init …`, `init: true` in Compose, or tini as the entrypoint): Angelia is
then not PID 1, and any process a command leaves behind is reaped.

In a container setup a [webhook sink](webhooks.md) to a small receiver in a sidecar container is usually the
better choice: the receiver gets the same JSON, signed, and runs with its own tools and permissions.

## Several dispatcher nodes

Every process running the `dispatcher` role runs commands, so **every dispatcher node must define every
command** of `exec.commands`, with its program at the same path. `angelia serve` refuses to start the
dispatcher role when a defined command's program is missing, so a misconfigured node fails at once. A node
whose `angelia.yaml` does not define a command that a sink names retries the delivery (`last_error` names the
command), so another node, or this one after its configuration is fixed, can deliver it within the
dispatcher's attempts.
