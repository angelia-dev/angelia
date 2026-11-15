# Benchmark 5: kill -9 during dispatch

**Verdict: PASS.** Target ([architecture.md §6](../../docs/architecture.md#6-language-and-framework-phase-0-assumptions)): zero lost deliveries, duplicates only within the idempotency key, over 100 SIGKILLs during heavy dispatch.

Run 2026-11-15T21:35:07Z, 1053.9s in total (warm-up 284.2s, kill loop 228.0s, drain 541.4s).

- Postgres **16.15**, not 17: the only server of the machine. Run with `-replicas 2 -kills 100 -registry-delay-min 500ms -registry-delay-max 2s` and the default workload otherwise (`go run ./bench/crash -database postgres ...`, the DSN in `BENCH_POSTGRES_DSN`); see [bench/crash/README.md](../crash/README.md#on-postgres-with-several-replicas). The harness was built from the commit below plus the uncommitted review fixes (they are in the next commit).
- **71 of 1,103 checks were cut off by a kill** (no finish line in the log; judged until their lease expiry), and none overlapped another check of the same target. The registry delay (0.5 to 2 s per request) makes a check last about a second, so kills land inside checks. The first run of this benchmark, without a delay, had 1 of 1,026.
- The kill-loop rows ("Kills during detection", "cut requests off", the backlog after each kill) count the whole cluster: with replicas, the surviving replica keeps checking and dispatching during and after a kill.
- Both replicas, the registry, the receiver and Postgres share one 4-CPU machine, so one clock. The drain took 541 s because it waits for a full check cycle (leases of killed replicas, then every target checked after the freeze), and that cycle finds the releases published since each target's last check: 3,702 deliveries were pending at the freeze and about 14,000 requests were received during the drain. Dispatch ran at about 26 requests per second in both the kill loop and the drain. This run does not measure dispatch throughput.

## Checks

| Check | Result | Detail |
|---|---|---|
| SIGKILLs during the run | pass | 100 of 100, each process ended by SIGKILL |
| every kill during dispatch (deliveries pending or in flight) | pass | 100 of 100 kills; 51 cut requests off at the receiver |
| kills during detection | pass | 69 kills found checks committed since the previous kill (want at least 30) |
| drain finished (every target checked after the freeze, every delivery finished) | pass | 0 deliveries pending or in flight at the end |
| database integrity (foreign keys, constraints, indexes) | pass | ok |
| watches and subscriptions match the workload | pass | 300 watches, 5 subscriptions |
| versions recorded match the registry at the freeze | pass | 300 packages: 1983 baseline and 4028 new versions, as published |
| one event per new version | pass | 4028 events for 4028 new versions (1983 baseline versions without events) |
| one delivery per event and subscription | pass | 20140 deliveries = 4028 events x 5 subscriptions |
| no delivery given up (dead) | pass | 20140 delivered, 0 dead, 0 unfinished |
| every release published before the freeze reached every sink (registry) | pass | 20140 of 20140 expected (4028 new versions x 5 sinks) |
| every delivery in the database received at least once (zero lost) | pass | 20140 of 20140 finished deliveries received |
| duplicates only within the idempotency key | pass | 158 duplicate requests over 154 deliveries, each with its delivery's key and body |
| every request signed (Standard Webhooks) | pass | 20298 requests verified |
| no request outside the database's deliveries | pass | none |
| no target checked by two holders at once (from the scheduler logs) | pass | 1103 checks of 300 targets in 2 logs by 87 holders (71 cut off by a kill), none overlapping, every database target checked |

## Counts

| | |
|---|---|
| Versions recorded (baseline, new) | 1983, 4028 |
| Events | 4028 |
| Deliveries (rows) | 20140 |
| Deliveries attempted more than once | 154 |
| Requests received | 20298 |
| Distinct deliveries received | 20140 |
| Duplicate requests (deliveries received twice or more) | 158 (154) |
| Received before the first kill | 202 |
| Expected from the registry: new versions, deliveries | 4028, 20140 |

## Kills

| | |
|---|---|
| Kills | 100 |
| Kills timed from the process start, not waiting for /readyz (startup to ready: see below) | 6 |
| Uptime after ready (drawn) | 0.2s to 3.0s, median 1.8s |
| Restart to ready | median 0.2s, max 0.3s |
| Kills during detection (checks committed since the previous kill) | 69 (69 with new events) |
| Requests received per life | min 0, median 51, max 96 |
| Kills that cut requests off at the receiver | 51 (263 requests) |
| Kills per replica | replica 1: 52, replica 2: 48 |

<details><summary>Every kill</summary>

| # | Replica | Timed from | Restart to ready | Uptime | Checks | Events | Received | Open at kill | Pending after | In flight after |
|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 1 | ready | 0.0s | 1.0s | 33 | 323 | 33 | 0 | 652 | 0 |
| 2 | 2 | ready | 0.3s | 2.1s | 3 | 31 | 65 | 0 | 742 | 0 |
| 3 | 1 | ready | 0.2s | 1.2s | 0 | 0 | 39 | 7 | 696 | 6 |
| 4 | 2 | ready | 0.2s | 1.9s | 1 | 11 | 54 | 5 | 692 | 5 |
| 5 | 1 | ready | 0.2s | 1.4s | 3 | 31 | 36 | 2 | 809 | 7 |
| 6 | 1 | ready | 0.2s | 1.0s | 1 | 11 | 26 | 0 | 838 | 5 |
| 7 | 2 | ready | 0.2s | 2.1s | 3 | 32 | 65 | 0 | 933 | 5 |
| 8 | 2 | ready | 0.2s | 2.9s | 5 | 39 | 71 | 0 | 1057 | 5 |
| 9 | 2 | start | 0.0s | 0.0s | 0 | 0 | 0 | 0 | 1057 | 5 |
| 10 | 1 | ready | 0.2s | 1.6s | 0 | 0 | 68 | 10 | 981 | 15 |
| 11 | 1 | ready | 0.2s | 1.8s | 0 | 0 | 34 | 0 | 947 | 9 |
| 12 | 1 | start | 0.0s | 0.3s | 1 | 9 | 12 | 9 | 971 | 18 |
| 13 | 1 | ready | 0.2s | 0.8s | 1 | 10 | 18 | 4 | 999 | 21 |
| 14 | 2 | ready | 0.2s | 1.5s | 1 | 11 | 47 | 5 | 1002 | 21 |
| 15 | 2 | start | 0.0s | 0.2s | 0 | 0 | 4 | 0 | 1002 | 17 |
| 16 | 2 | ready | 0.2s | 2.8s | 1 | 10 | 82 | 7 | 963 | 25 |
| 17 | 2 | ready | 0.3s | 2.7s | 4 | 40 | 75 | 1 | 1074 | 26 |
| 18 | 1 | ready | 0.2s | 1.5s | 0 | 0 | 46 | 0 | 1028 | 26 |
| 19 | 1 | ready | 0.2s | 2.1s | 2 | 20 | 49 | 0 | 1079 | 26 |
| 20 | 1 | ready | 0.2s | 1.3s | 0 | 0 | 51 | 8 | 1020 | 34 |
| 21 | 2 | ready | 0.3s | 1.1s | 1 | 10 | 26 | 8 | 1036 | 42 |
| 22 | 1 | ready | 0.3s | 2.4s | 4 | 40 | 60 | 0 | 1175 | 34 |
| 23 | 1 | start | 0.0s | 0.1s | 0 | 0 | 0 | 8 | 1167 | 42 |
| 24 | 1 | ready | 0.2s | 2.6s | 4 | 40 | 64 | 2 | 1300 | 36 |
| 25 | 2 | ready | 0.2s | 2.6s | 1 | 10 | 70 | 2 | 1279 | 38 |
| 26 | 2 | ready | 0.2s | 1.4s | 1 | 9 | 52 | 2 | 1270 | 40 |
| 27 | 1 | ready | 0.2s | 0.3s | 1 | 10 | 29 | 5 | 1286 | 45 |
| 28 | 2 | ready | 0.2s | 2.8s | 2 | 18 | 84 | 3 | 1289 | 45 |
| 29 | 2 | ready | 0.2s | 2.5s | 3 | 29 | 67 | 0 | 1367 | 44 |
| 30 | 2 | ready | 0.3s | 0.7s | 1 | 10 | 34 | 0 | 1383 | 44 |
| 31 | 2 | ready | 0.2s | 2.3s | 2 | 20 | 62 | 0 | 1421 | 44 |
| 32 | 1 | ready | 0.2s | 2.9s | 1 | 10 | 96 | 2 | 1373 | 46 |
| 33 | 1 | ready | 0.2s | 1.7s | 1 | 10 | 54 | 2 | 1368 | 46 |
| 34 | 2 | ready | 0.2s | 1.3s | 2 | 19 | 40 | 8 | 1410 | 54 |
| 35 | 1 | ready | 0.2s | 1.9s | 4 | 39 | 59 | 0 | 1541 | 46 |
| 36 | 1 | ready | 0.2s | 0.8s | 0 | 0 | 21 | 0 | 1520 | 46 |
| 37 | 2 | ready | 0.2s | 3.0s | 6 | 63 | 93 | 1 | 1741 | 47 |
| 38 | 2 | ready | 0.2s | 2.4s | 1 | 11 | 54 | 2 | 1741 | 48 |
| 39 | 1 | ready | 0.2s | 2.3s | 0 | 0 | 54 | 7 | 1676 | 56 |
| 40 | 2 | ready | 0.3s | 2.7s | 6 | 62 | 49 | 0 | 1934 | 48 |
| 41 | 1 | ready | 0.3s | 2.6s | 2 | 18 | 55 | 8 | 1961 | 56 |
| 42 | 2 | ready | 0.2s | 1.8s | 0 | 0 | 61 | 1 | 1899 | 57 |
| 43 | 1 | ready | 0.3s | 2.5s | 4 | 38 | 94 | 6 | 1989 | 63 |
| 44 | 2 | ready | 0.2s | 0.6s | 1 | 11 | 15 | 0 | 2029 | 57 |
| 45 | 2 | ready | 0.2s | 1.1s | 0 | 0 | 28 | 0 | 2001 | 57 |
| 46 | 2 | ready | 0.2s | 2.5s | 1 | 10 | 64 | 8 | 1979 | 65 |
| 47 | 1 | ready | 0.2s | 2.1s | 6 | 60 | 43 | 0 | 2222 | 65 |
| 48 | 1 | ready | 0.2s | 2.0s | 2 | 20 | 68 | 0 | 2245 | 65 |
| 49 | 2 | ready | 0.2s | 1.5s | 1 | 11 | 65 | 5 | 2230 | 71 |
| 50 | 2 | ready | 0.2s | 2.4s | 2 | 21 | 52 | 6 | 2277 | 76 |
| 51 | 1 | ready | 0.2s | 0.5s | 1 | 10 | 25 | 3 | 2299 | 79 |
| 52 | 1 | ready | 0.2s | 2.0s | 3 | 30 | 55 | 1 | 2393 | 80 |
| 53 | 2 | ready | 0.2s | 1.7s | 3 | 32 | 43 | 8 | 2502 | 87 |
| 54 | 1 | ready | 0.3s | 2.2s | 0 | 0 | 52 | 0 | 2450 | 87 |
| 55 | 1 | ready | 0.2s | 0.6s | 1 | 9 | 35 | 0 | 2460 | 87 |
| 56 | 1 | ready | 0.2s | 2.3s | 3 | 28 | 59 | 0 | 2541 | 87 |
| 57 | 1 | ready | 0.2s | 2.1s | 0 | 0 | 60 | 0 | 2480 | 87 |
| 58 | 2 | ready | 0.2s | 1.5s | 2 | 18 | 51 | 16 | 2503 | 102 |
| 59 | 2 | ready | 0.3s | 2.8s | 6 | 60 | 64 | 0 | 2732 | 95 |
| 60 | 1 | ready | 0.2s | 2.9s | 3 | 30 | 79 | 0 | 2803 | 95 |
| 61 | 2 | ready | 0.2s | 2.9s | 2 | 21 | 94 | 4 | 2811 | 98 |
| 62 | 1 | ready | 0.2s | 1.6s | 3 | 19 | 42 | 0 | 2864 | 97 |
| 63 | 1 | ready | 0.2s | 1.1s | 0 | 0 | 28 | 7 | 2829 | 104 |
| 64 | 1 | start | 0.0s | 0.0s | 0 | 0 | 0 | 7 | 2829 | 104 |
| 65 | 2 | ready | 0.2s | 2.6s | 1 | 19 | 82 | 8 | 2794 | 107 |
| 66 | 1 | ready | 0.2s | 1.7s | 1 | 10 | 83 | 1 | 2755 | 108 |
| 67 | 1 | ready | 0.2s | 2.5s | 3 | 29 | 52 | 0 | 2848 | 107 |
| 68 | 1 | ready | 0.3s | 1.4s | 1 | 10 | 43 | 3 | 2851 | 110 |
| 69 | 2 | ready | 0.2s | 1.6s | 0 | 0 | 56 | 0 | 2799 | 106 |
| 70 | 2 | ready | 0.3s | 3.0s | 8 | 77 | 85 | 1 | 3106 | 99 |
| 71 | 1 | ready | 0.2s | 1.9s | 3 | 29 | 46 | 8 | 3196 | 106 |
| 72 | 1 | ready | 0.3s | 0.9s | 3 | 29 | 24 | 0 | 3317 | 106 |
| 73 | 1 | ready | 0.2s | 0.8s | 3 | 29 | 63 | 1 | 3398 | 107 |
| 74 | 2 | ready | 0.2s | 0.5s | 2 | 19 | 13 | 8 | 3480 | 107 |
| 75 | 1 | ready | 0.2s | 0.8s | 0 | 0 | 31 | 0 | 3449 | 107 |
| 76 | 1 | ready | 0.2s | 0.4s | 0 | 0 | 27 | 5 | 3417 | 112 |
| 77 | 1 | ready | 0.3s | 1.9s | 0 | 0 | 43 | 5 | 3370 | 114 |
| 78 | 1 | ready | 0.2s | 0.4s | 0 | 0 | 14 | 0 | 3356 | 109 |
| 79 | 2 | ready | 0.2s | 2.7s | 1 | 10 | 79 | 0 | 3327 | 109 |
| 80 | 1 | ready | 0.2s | 0.8s | 0 | 0 | 23 | 0 | 3304 | 109 |
| 81 | 1 | ready | 0.2s | 0.7s | 1 | 10 | 26 | 0 | 3336 | 101 |
| 82 | 2 | ready | 0.2s | 0.4s | 0 | 0 | 20 | 5 | 3311 | 106 |
| 83 | 2 | start | 0.0s | 0.2s | 0 | 0 | 5 | 0 | 3311 | 106 |
| 84 | 2 | ready | 0.2s | 2.4s | 2 | 21 | 55 | 0 | 3361 | 106 |
| 85 | 2 | ready | 0.2s | 0.7s | 0 | 0 | 29 | 0 | 3332 | 106 |
| 86 | 2 | ready | 0.2s | 0.8s | 0 | 0 | 35 | 0 | 3297 | 106 |
| 87 | 1 | ready | 0.2s | 0.6s | 0 | 0 | 20 | 6 | 3273 | 110 |
| 88 | 2 | ready | 0.2s | 1.1s | 0 | 0 | 24 | 0 | 3248 | 110 |
| 89 | 1 | ready | 0.2s | 2.9s | 3 | 33 | 80 | 0 | 3330 | 106 |
| 90 | 2 | ready | 0.2s | 0.2s | 0 | 0 | 12 | 3 | 3319 | 105 |
| 91 | 2 | ready | 0.3s | 3.0s | 1 | 11 | 70 | 0 | 3304 | 102 |
| 92 | 2 | ready | 0.2s | 1.3s | 1 | 9 | 47 | 0 | 3294 | 102 |
| 93 | 1 | ready | 0.2s | 2.1s | 2 | 19 | 66 | 0 | 3323 | 102 |
| 94 | 1 | ready | 0.2s | 1.0s | 0 | 0 | 23 | 7 | 3284 | 110 |
| 95 | 1 | ready | 0.3s | 2.5s | 3 | 31 | 58 | 4 | 3372 | 106 |
| 96 | 2 | ready | 0.2s | 2.6s | 1 | 10 | 63 | 0 | 3358 | 106 |
| 97 | 1 | ready | 0.2s | 1.6s | 0 | 0 | 44 | 0 | 3316 | 104 |
| 98 | 2 | ready | 0.3s | 3.0s | 8 | 80 | 90 | 8 | 3618 | 112 |
| 99 | 2 | ready | 0.2s | 2.9s | 2 | 21 | 62 | 0 | 3661 | 112 |
| 100 | 2 | ready | 0.2s | 0.3s | 1 | 10 | 9 | 0 | 3702 | 112 |

</details>

## Parameters

| | |
|---|---|
| Watches (pypi, fake registry) | 300, interval 5m, 3 versions each at start |
| Releases | 36000 per hour over all packages (one every 30.0s per package) |
| Webhook sinks and subscriptions | 5, each matching every event |
| Kills | 100, uptime uniform in [200ms, 3s) after /readyz; 10% in [0, 300ms) after the start |
| Warm-up | watches added in 8 groups, 30s apart; a kill waits for 500 pending deliveries |
| Registry delay | uniform in [500ms, 2s) per request |
| Receiver delay | uniform in [0, 250ms) per request |
| Dispatch starts the kills | after 200 requests received |
| Seed | 8082839333688488424 |

## Environment

| | |
|---|---|
| Machine | single VM, Postgres 16.15 (local server, not 17) and angelia on the same host |
| Mode | 2 × `angelia serve` (every role, its own holder each) as child processes on one Postgres 16.15 (Ubuntu 16.15-0ubuntu0.24.04.1) database, no container limits |
| OS, kernel | linux/amd64, 6.18.44-fc-v470 |
| CPU | Intel(R) Xeon(R) Processor @ 2.10GHz, 4 CPUs |
| Memory | 15.7 GiB |
| Go | harness go1.27.4, angelia go1.27.4 |
| Commit | 9175df5724decffa1777fe3f9df229be3ac47c38 (with uncommitted changes) |
