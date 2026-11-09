# Benchmark 5: kill -9 during dispatch

Phase 0 benchmark 5 ([architecture.md §6](../../docs/architecture.md#6-language-and-framework-phase-0-assumptions),
story angelia-dev/angelia#109): **zero lost deliveries, duplicates only within the idempotency key**,
while angelia is killed with SIGKILL 100 times during heavy dispatch.

```
make bench-crash                                    # 100 kills, about 17 minutes, Linux, no Docker
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
3. **Warm-up.** Starts `angelia serve` with all roles and adds the watches in `-stages` (8) groups,
   `-stage-every` (30 s) apart, with a graceful restart for each group, so that the baseline checks,
   and so every later check cycle, are spread over 3.5 minutes instead of all at once. The first check
   of a package is the baseline (no events); the next, one interval (5 minutes) later, finds about 10
   new releases. The kills start once the receiver has had `-min-flow` (200) requests, about 5 minutes
   after the start.
4. **Kill loop.** 100 times: start angelia (except for the first kill), then
   - for about `-from-start` (10%) of the kills, chosen at random when the previous kill left at least
     `-min-pending` deliveries pending: kill a random time after the **start of the process**, uniform
     in 0 to `-from-start-max` (300 ms), before or around `/readyz`: in the migrations, the watch file
     apply and the first leases;
   - for the others: wait for `/readyz` and for `-min-pending` (500) pending deliveries, then kill a
     random time later, uniform in 0.2 s to 3 s. angelia's rate limit for the receiver's host
     (`http.host_limits`: 100 rps, burst 200) cannot send 500 deliveries within 3 s, so the kill lands
     while deliveries wait; on a machine that dispatches faster than the check cycles produce, the
     loop pauses until the next cycle (at most `-busy-timeout`).

   Each kill is a SIGKILL; the harness checks the process ended by that signal, then reads from the
   database the backlog (deliveries pending or in flight), the targets checked since the previous kill
   and the events committed since then. A kill whose process committed checks is a **kill during
   detection**; because the check cycles are spread, detection runs through the whole loop, and the
   run is incomplete with fewer than `-min-detection-kills` (30). The receiver answers each request
   after up to 250 ms, so many kills cut requests off in the middle of the sink call. The seed
   (`-seed`, recorded in the report) repeats the kill times.
5. **Freeze and drain.** When the loop ends, the fake registry's clock stops: nothing is published
   after that. angelia then runs with **every role** until every target's last check ended at least
   10 s after the freeze and no delivery is pending or in flight. Targets leased by a killed scheduler
   wait for that lease (5 minutes), deliveries in flight at a kill for theirs (2 minutes,
   [ADR 0017](../../docs/adr/0017-dispatcher-delivery-leases-and-retries.md)); neither lease can be
   shortened by configuration, and a target is checked again only at its interval, so the drain takes
   5 to 7 minutes. Then it stops angelia with SIGTERM.
6. **Verify.** Compares three sources: the fake registry (the versions of each package at the freeze
   and in the first response angelia got, its baseline), angelia's database (read after the stop, with
   `PRAGMA integrity_check` and `foreign_key_check`) and every request the receiver recorded (with the
   package and version in its body). It writes `report.md` and `report.json` to `bin/bench/crash/`,
   next to `angelia.log` (every process, appended), the configuration and the database. Commit the
   report to [bench/results/](../results/); delete the rest.

A run takes about 17 minutes: 5 of warm-up, 4 to 5 of kills, 6 to 7 of drain.

## What is checked

| Check | Fails when |
|---|---|
| SIGKILLs during the run | fewer than `-kills`, or a process ended for another reason (incomplete) |
| Every kill during dispatch | a kill found no delivery pending or in flight (incomplete: the run was not heavy enough) |
| Kills during detection | fewer than `-min-detection-kills` kills found checks committed since the previous kill (incomplete) |
| Drain finished | a target was not checked after the freeze, or a delivery is still pending or in flight (incomplete: the registry checks cannot be judged) |
| Database integrity | `PRAGMA integrity_check` is not `ok`, or `foreign_key_check` reports a row |
| Watches and subscriptions match the workload | the database lacks a watch or subscription of the workload, or has more |
| Versions recorded match the registry at the freeze | a package's versions are not exactly the registry's at the freeze, or its baseline is not the registry's first response (a version lost before it was recorded) |
| One event per new version | a version first seen after the baseline has no `release.published` event, or two (the transactional outbox) |
| One delivery per event and subscription | an event lacks a delivery row for a subscription |
| No delivery given up (dead) | a delivery is dead, for example after 8 attempts cut off by kills |
| **Every release published before the freeze reached every sink (registry)** | for a version the registry published after the package's baseline and before the freeze, a sink never got a request with that package and version; judged from the registry and the requests only, without the database |
| **Every delivery in the database received at least once (zero lost)** | the receiver never got a delivery that is delivered or dead |
| **Duplicates only within the idempotency key** | a request's `webhook-id` is not `<event id>:<subscription id>` of its event and sink, or a duplicate has another body |
| Every request signed | a Standard Webhooks signature does not verify |
| No request outside the database's deliveries | a request names an event or sink the database does not have |

A failed delivery check is the verdict **fail**: stop and report it with the examples the report lists
before changing angelia. Duplicates are expected (at-least-once delivery: a request answered just before
a kill is sent again after the lease) and counted.

## Flags

`go run ./bench/crash -h` lists them; `BENCH_CRASH_FLAGS` passes them through `make`. The main ones:
`-kills` (100), `-min-uptime` / `-max-uptime` (200ms / 3s), `-n` (300 packages), `-subscriptions` (5),
`-releases-per-hour` (36,000: one release per package every 30 s), `-receiver-delay` (250ms), `-min-pending` (500),
`-from-start` (0.1), `-from-start-max` (300ms), `-stages` (8), `-stage-every` (30s), `-min-detection-kills` (30),
`-seed` (random; the report records it, to repeat the kill times), `-drain-timeout` (15m).
Raise `-releases-per-hour` if the report shows kills waiting for work; lower it if the drain is long.

## Known limits

- **One host, one database**: SQLite and one process, as the default deployment. A Postgres run with
  several dispatchers is for the HA story.
- **Kill points** are random in time, not chosen in the code: across 100 kills they land in the
  startup, the scheduler's checks and commits, the dispatcher's lease and finish transactions and the
  sink call, but a given run does not prove every instruction was hit. The report says how many kills
  came from the start, found detection running and cut requests off at the receiver.
- **Kills during detection** counts processes that committed at least one check; a kill in the middle
  of a check's transaction is likely across many such kills but not observed directly.
- **The drain checks every target once more after the freeze**, and the last check must end 10 s
  after it, since a check records its end time; against the fake registry a check takes milliseconds.
- **SIGKILL is not a power cut**: the kernel keeps what the process wrote. SQLite runs with
  `synchronous=NORMAL` in WAL mode, which is durable across a process crash but may lose the last
  transactions on a power loss.
- The receiver records a request after its delay, just before it answers. A request cut
  off later counts as received and is sent again: a duplicate, never a hidden loss.
