# ADR 0042: The exec sink runs only commands the operator defined in angelia.yaml, with the event on stdin, a clean environment and a killed process group

- **Status:** Accepted (2026-11-25: the maintainer accepted decisions D100 to D104 as recommended;
  review follow-ups of the same day folded in: group kill at every exit, retryable unknown command,
  `/proc` exposure of Angelia's environment)
- **Date:** 2026-11-25
- **Story:** angelia-dev/angelia#92

## Context

[PRD F7](../prd.md#6-functional-requirements) lists `exec` as a Must sink, and
[architecture.md §3.5](../architecture.md#3-component-view) lists it among the v1 built-ins: "run
my script on a new release" is the simplest integration a homelab or a CI runner host has.

Running a local command for each event is a security decision. The command runs as the Angelia
user, which can read the database and the secrets key. Whoever decides what runs can therefore
read every stored secret. Today a token with the `write` scope can create sinks through the API
(`PUT /v1/sinks/{name}`), and the watch file and the admin UI create them too. None of those
should be able to choose a program, an argument or an environment variable.

The `Sink` contract ([internal/sink/sink.go](../../internal/sink/sink.go)) fits unchanged:
`Deliver` gets the rendered message (`Request.Rendered`), the identifiers of the delivery and a
context bounded by the dispatcher's attempt timeout (`dispatch.DefaultDeliverTimeout`, 15 s). An
error is retried unless it is `sink.Permanent`, and its text is stored in `delivery.last_error`,
which every `read` token can see.

The Angelia process environment holds `ANGELIA_SECRETS_KEY` (or its file path), source tokens and
webhook secrets ([install.md](../install.md), `EnvironmentFile=`). The official image is
distroless with no shell. Builds are `CGO_ENABLED=0` for linux, darwin and windows.

## Decision

### 1. Who decides what runs (D100)

Commands are defined **only** in the instance configuration, `angelia.yaml`:

```yaml
exec:
  commands:
    deploy-hook:
      argv: [/usr/local/bin/deploy-hook, --env, staging]
      dir: /var/lib/angelia     # optional, absolute, default /
      env: [DEPLOY_TOKEN]       # optional, names copied from Angelia's environment
      timeout: 10s              # optional, 1s to 12s
      max_concurrent: 1         # optional, 1 to 8
```

A sink's configuration is only `{"command": "<name>"}` (spec `ExecSinkConfig`, spec 0.14.0). It
is set from the watch file, the API (`write`) or the UI, and an unknown name is refused. The
`exec` kind is **registered only when at least one command is defined**: that is the explicit
opt-in. Without one, `kind: exec` is an unknown kind everywhere. There is no environment
variable override of `exec.commands`, as for `http.host_limits`.

A `write` caller can decide which events reach an approved command and what a template puts on
its stdin, nothing more. This is the same trust split as `http.allow_networks`: only the person
who edits `angelia.yaml` and restarts the process widens what Angelia may do.

### 2. How the command starts (D101)

`argv` is a list; `argv[0]` is an **absolute, clean path** (no `PATH` lookup). It is started
directly with `os/exec`: no shell, and no placeholder expansion in any argument. A script that
needs a shell names it (`argv: [/bin/sh, /etc/angelia/hooks/deploy.sh]`). `dir` is absolute and
defaults to `/`. `argv` has at most 64 entries.

`config check` and `serve` with the `dispatcher` role check that every `argv[0]` exists, is a
regular file and is executable, and fail naming the command otherwise.

### 3. What the command receives (D102)

- **stdin** is exactly `Rendered.Body` (the CloudEvents JSON in structured mode, or the
  subscription's template output), then EOF. A command that does not read stdin does not block
  the delivery.
- The **environment is clean**, not passed on: `PATH=/usr/local/bin:/usr/bin:/bin`; each name
  of the command's `env` list that is set in Angelia's environment (never an `ANGELIA_` name,
  the rule of every `*_env` key); and six variables with Angelia-generated values only:
  `ANGELIA_EVENT_ID`, `ANGELIA_EVENT_TYPE`, `ANGELIA_DELIVERY_ID`, `ANGELIA_IDEMPOTENCY_KEY`,
  `ANGELIA_ATTEMPT`, `ANGELIA_CONTENT_TYPE`.
- Nothing from the event (package names, versions, notes) is put in `argv` or the environment.
  stdin is untrusted input to the command, and the docs say so.
- A clean environment does **not** hide Angelia's own environment from a command running as the
  same user: it could read `/proc/<angelia pid>/environ` (the start-up environment, which still
  holds `ANGELIA_SECRETS_KEY` after `serve` unsets it). On Linux, `serve` therefore calls
  `prctl(PR_SET_DUMPABLE, 0)` first thing, which makes its `/proc/<pid>/environ`, `mem` and fds
  owned by root, so another process of the same user cannot read them (it also disables core
  dumps). macOS has no such switch (`ps -E` shows a same-user process's environment). The docs
  recommend running the work as another user (the command hands off to a service) and systemd
  `ProtectProc=invisible`.

### 4. Limits, process control and outcomes (D103)

- `timeout` defaults to `10s`, from `1s` to `12s`: the 15 s attempt budget minus a 2 s kill
  grace and a margin. The effective timeout is `min(timeout, ctx deadline − 2 s)`; with less
  than 1 s left the command is not started and the attempt is retried.
- The child gets its **own process group** (`SysProcAttr.Setpgid`). On timeout or context
  cancellation (shutdown, lease end) Angelia sends `SIGTERM` to the group, then `SIGKILL` to the
  group after 2 s (`exec.Cmd.Cancel`, a timer, and `WaitDelay` for the leader), so a grandchild
  that holds a pipe open cannot hang the delivery.
- **Once the leader exits, whatever its exit status, the rest of its group gets `SIGKILL`**: a
  run leaves nothing behind. On Linux this happens before the leader is reaped (`waitid` with
  `WNOWAIT`), so the group id, the leader's pid, cannot have been reused; on macOS just after
  `Wait`. Long work is handed off to a service, never to a background child. In a container,
  Angelia should run with `--init` (or tini) so that orphans are reaped.
- On Linux the direct child also gets `Pdeathsig: SIGKILL`, so it dies if Angelia crashes; it
  covers the direct child only. Linux sends it when the *thread* that started the child exits, so
  start and wait run on one locked OS thread (`runtime.LockOSThread`).
- The outcome is decided by whether `Cancel` fired (a flag set inside it), not by the context's
  state afterwards: a command that exited on its own is judged by its exit status even if the
  deadline passed while it was ending.
- stdout and stderr are each read into a 64 KiB capped buffer; the rest is drained and
  discarded.
- `max_concurrent`, default 1, from 1 to 8, bounds the running instances of a command, shared by
  every sink that names it. A wait cut by the context is retried.
- **Outcomes**: exit 0 is delivered (`Result{StatusCode: 0}`). Any other exit status, a signal or
  a timeout is retried like an HTTP 5xx. A command that cannot be started (missing, not
  executable) is permanent. A sink configuration that is malformed is permanent; one naming a
  command this node does not define is retried (bounded by the dispatcher's attempts), since the
  fix is on the node. `last_error` holds only `exec <name>: exit status 3`,
  `exec <name>: timed out after 10s`, `exec <name>: killed by signal 9` or
  `exec <name>: could not start: permission denied`, **never output**. On failure the last 1 KiB
  of stderr is logged at `warn` with the sink and the command name; stdout is never logged.

### 5. Platforms, containers and several dispatchers (D104)

Linux and macOS only, by build tags (`unix` for the runner, `!unix` for a stub). On other
platforms `exec.New` returns "exec sinks are not supported on <GOOS>", the kind is never
registered, and `config check` and `serve` fail when `exec.commands` is set. The distroless image
is unchanged: the docs show a derived image and recommend a webhook sink to a sidecar for
container setups. With several dispatcher nodes every node must have every command: `serve`
refuses to start the `dispatcher` role when a configured command is missing, so a bad node fails
fast.

## Threat cases

| Case | Answer |
|---|---|
| A `write` token (or a stolen UI session) wants to run code | It can only name a command from `angelia.yaml`. Arguments, path and environment are not part of the sink configuration; unknown names are refused at write time. |
| A malicious release (package name, version, notes) | It reaches the command only on stdin, as JSON or template output, never in `argv` or the environment. The docs tell command authors to treat stdin as untrusted. |
| The command reads Angelia's secrets from its environment | The command's environment is clean; only names the operator listed are copied, never `ANGELIA_*`. That alone does not stop a same-user process from reading `/proc/<angelia pid>/environ`: on Linux `serve` sets itself non-dumpable (`PR_SET_DUMPABLE` 0), which makes that file unreadable to it; on macOS nothing does. The command still runs as the Angelia user and can read what that user can read (the database file, a key file): the docs recommend handing the work to a service running as another user, systemd `ProtectProc=invisible`, and the unit's sandbox. |
| A runaway or forking command | Its process group is sent `SIGTERM` then `SIGKILL` at the timeout, and `SIGKILL` once the leader exits in any case; output is capped at 64 KiB per stream; `max_concurrent` bounds parallel runs. A child that leaves the group (`setsid`) is out of reach; the docs say so and recommend `--init` in containers. |
| Shutdown or lease end during a run | The context is cancelled, the group is killed the same way, and the attempt is retried (a possible duplicate, hence `ANGELIA_IDEMPOTENCY_KEY`). |
| Output leaks secrets into the API | Output is never stored in `last_error`; only stderr's last 1 KiB reaches the log, at `warn`. |
| Several dispatcher nodes | Every node must define every command. Each node checks every `argv[0]` at start and refuses to run the dispatcher role without it; a node that does not define a command a sink names retries the delivery. |
| The command makes network calls | Angelia's SSRF guard does not apply to it; the docs say so. |

## Consequences

### Positive

- The Must of F7 ships without new trust: no API caller can cause new code to run.
- One representation of the event, the one webhooks get, parsed with `jq` or any JSON library.
- No interface change, no dependency (`os/exec` and `syscall` only, `prctl` and `waitid` as raw
  system calls), no migration.

### Negative

- A command must finish within 12 s; longer work must be handed off (enqueue a job, call a
  service) and the command exit.
- Not on Windows, and not useful in the official image without a derived image.
- The command runs with Angelia's user and file access; Angelia does not switch users, chroot or
  use namespaces.

### Open questions

- A longer timeout through an optional `AttemptTimeout` sink hook honoured by the dispatcher
  (D103 option b), if users ask.
- Stored secrets (`*_ref`) for commands.
- `internal/sink/exec` should be added to the sensitive paths of `graft.yaml` by the maintainer.

## Alternatives considered

- **Not in 1.0** (D100 a): safest, but F7 says Must.
- **`admin` scope for exec sinks** (D100 c): a per-kind scope rule the API does not have, and
  still remote code execution for an admin token.
- **`argv` in the sink config with an allowlist of executables** (D100 d): allowing `/bin/sh` or
  `python3` allows anything.
- **`argv` freely in the sink config** (D100 e): any `write` token becomes remote code execution.
- **A `command` string run with `/bin/sh -c`** (D101 b): quoting bugs and injection, and no shell
  in the image.
- **Placeholders such as `{{.Version}}` in `argv`** (D101 c): untrusted registry strings in
  arguments (option injection such as a version `--exec=…`).
- **Event fields flattened into environment variables** (D102 b): untrusted strings where sloppy
  scripts expand them, and a second representation of the event schema.
- **A temporary file path in `argv`** (D102 c): needs a writable directory and cleanup.
- **Inherit Angelia's environment** (D102 d): leaks the secrets key and every token.
- **Exit code classes** (D103 c): more to document for little gain.
- **Windows with job objects** (D104 b): a new direct dependency and Windows CI.
- **A second image with a shell** (D104 c): a larger attack surface and a second image to sign.

## References

- Issue angelia-dev/angelia#92 (decisions D100 to D104), spec `ExecSinkConfig` (spec 0.14.0).
- [docs/exec.md](../exec.md), [ADR 0017](0017-dispatcher-delivery-leases-and-retries.md),
  [ADR 0023](0023-webhook-sink-standard-webhooks.md).
