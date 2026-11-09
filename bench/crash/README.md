# Benchmark 5: kill -9 during dispatch

Phase 0 benchmark 5 ([architecture.md §6](../../docs/architecture.md#6-language-and-framework-phase-0-assumptions),
story angelia-dev/angelia#109): **zero lost deliveries, duplicates only within the idempotency key**,
while angelia is killed with SIGKILL 100 times during heavy dispatch.

```
make bench-crash                                    # 100 kills, about 12 minutes, Linux, no Docker
BENCH_KILLS=10 make bench-crash                     # a short smoke run
BENCH_ENV_NOTE="CI runner, 4 vCPU" make bench-crash  # describe the machine in the report
```

It needs Linux (angelia runs as a child process; the receiver listens on `127.0.0.2`) and nothing
else: no Docker, no network. The result does not depend on the machine's speed, so it runs anywhere.
Exit status: 0 pass, 1 fail, 2 error, 3 incomplete.

## What a run does

1. Starts, in the harness process, the [fake registry](../fakereg/README.md) (pypi only: `-n` packages,
   3 versions each, then `-releases-per-hour` new releases spread over them) on `127.0.0.1` and the
   **recording webhook receiver** on `127.0.0.2`.
2. Writes `angelia.yaml` (SQLite in `data/`, both addresses in `http.allow_networks`, plain http to the
   receiver's host in `http.allow_http_hosts`, both hosts at the highest `http.host_limits`, 100 rps) and
   `watches.yaml`: one pypi watch per package (interval 5m, the minimum), `-subscriptions` webhook sinks
   (default 5), each with one subscription that matches every event, so every event is 5 deliveries.
   Each sink signs with a fresh Standard Webhooks secret that only the harness and angelia's environment
   (`BENCH_WEBHOOK_SECRET`) hold; it is never written down.
3. **Warm-up.** Starts `angelia serve` with all roles. The first check of each package is the baseline
   (no events); the next, one interval later, finds 8 releases per package: 2,400 events, 12,000
   deliveries at once. The kills start once the receiver has had `-min-flow` (200) requests. This takes
   about 5 minutes.
4. **Kill loop.** 100 times: wait until `-min-pending` (500) deliveries are pending, wait a random time
   (uniform in 0.2 s to 3 s, from `-seed`), send SIGKILL, check the process ended by that signal, read
   the backlog (deliveries pending or in flight) from the database, and start angelia again. angelia's
   rate limit for the receiver's host (`http.host_limits`: 100 rps, burst 200) cannot send 500
   deliveries within 3 s, so every kill lands while deliveries wait; on a machine that dispatches faster
   than the check cycles produce, the loop pauses until the next cycle (at most `-busy-timeout`). The
   scheduler keeps checking between kills, so kills also land in its commits. The receiver answers each
   request after up to 250 ms, so many kills cut requests off in the middle.
5. **Drain.** Stops the last process with SIGTERM and starts angelia with `--roles=api,dispatcher`
   only (no new events) until no delivery is pending or in flight. Deliveries in flight at a kill
   are sent again when their lease expires (2 minutes, [ADR 0017](../../docs/adr/0017-dispatcher-delivery-leases-and-retries.md)),
   so this takes 2 to 3 minutes.
6. **Verify.** Reads the database after a graceful stop and compares it with every request the
   receiver recorded (below), then writes `report.md` and `report.json` to `bin/bench/crash/`, next to
   `angelia.log` (every process, appended), the configuration and the database. Commit the report to
   [bench/results/](../results/); delete the rest.

## What is checked

| Check | Fails when |
|---|---|
| SIGKILLs during the run | fewer than `-kills`, or a process ended for another reason (incomplete) |
| Every kill during dispatch | a kill found no delivery pending or in flight (incomplete: the run was not heavy enough) |
| Versions recorded without gaps | a package's versions are not `1.0.0` to `1.k.0` (a version lost before it was recorded) |
| One event per new version | a version first seen after the baseline has no `release.published` event, or two (the transactional outbox) |
| One delivery per event and subscription | an event lacks a delivery row for a subscription |
| Drain finished every delivery | a delivery is still pending or in flight at the end (incomplete: neither delivered nor lost) |
| No delivery given up (dead) | a delivery is dead, for example after 8 attempts cut off by kills |
| **Every delivery received at least once (zero lost)** | the receiver never got a delivery that is delivered or dead |
| **Duplicates only within the idempotency key** | a request's `webhook-id` is not `<event id>:<subscription id>` of its event and sink, or a duplicate has another body |
| Every request signed | a Standard Webhooks signature does not verify |
| No request outside the database's deliveries | a request names an event or sink the database does not have |

A failed delivery check is the verdict **fail**: stop and report it with the examples the report lists
before changing angelia. Duplicates are expected (at-least-once delivery: a request answered just before
a kill is sent again after the lease) and counted.

## Flags

`go run ./bench/crash -h` lists them; `BENCH_CRASH_FLAGS` passes them through `make`. The main ones:
`-kills` (100), `-min-uptime` / `-max-uptime` (200ms / 3s), `-n` (300 packages), `-subscriptions` (5),
`-releases-per-hour` (28,800: one release per package every 37.5 s), `-receiver-delay` (250ms), `-min-pending` (500),
`-seed` (random; the report records it, to repeat the kill times), `-drain-timeout` (10m).
Raise `-releases-per-hour` if the report shows kills waiting for work; lower it if the drain is long.

## Known limits

- **One host, one database**: SQLite and one process, as the default deployment. A Postgres run with
  several dispatchers is for the HA story.
- **Kill points** are random in time, not chosen in the code: across 100 kills they land in the
  scheduler's commit, the dispatcher's lease and finish transactions and the sink call, but a given
  run does not prove every instruction was hit. The report says how many kills cut requests off at the
  receiver.
- **SIGKILL is not a power cut**: the kernel keeps what the process wrote. SQLite runs with
  `synchronous=NORMAL` in WAL mode, which is durable across a process crash but may lose the last
  transactions on a power loss.
- The receiver records a request once it has read the body, just before it answers. A request cut
  off later counts as received and is sent again: a duplicate, never a hidden loss.
