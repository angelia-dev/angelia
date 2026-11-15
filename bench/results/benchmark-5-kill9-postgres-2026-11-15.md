# Benchmark 5: kill -9 during dispatch

**Verdict: PASS.** Target ([architecture.md §6](../../docs/architecture.md#6-language-and-framework-phase-0-assumptions)): zero lost deliveries, duplicates only within the idempotency key, over 100 SIGKILLs during heavy dispatch.

Run 2026-11-15T20:44:50Z, 986.0s in total (warm-up 281.4s, kill loop 201.1s, drain 503.3s).

- Postgres **16.15**, not 17: the only server of the machine. Run with `-replicas 2 -kills 100` and the default workload (`go run ./bench/crash -database postgres -replicas 2`, the DSN in `BENCH_POSTGRES_DSN`); see [bench/crash/README.md](../crash/README.md#on-postgres-with-several-replicas).
- Both replicas, the registry, the receiver and Postgres share one 4-CPU machine, so one clock. Dispatch drained at about 30 requests per second with the backlog the kills left (6,600 deliveries at the freeze), which is why the drain took 503 s; this run does not measure dispatch throughput.

## Checks

| Check | Result | Detail |
|---|---|---|
| SIGKILLs during the run | pass | 100 of 100, each process ended by SIGKILL |
| every kill during dispatch (deliveries pending or in flight) | pass | 100 of 100 kills; 62 cut requests off at the receiver |
| kills during detection | pass | 81 kills found checks committed since the previous kill (want at least 30) |
| drain finished (every target checked after the freeze, every delivery finished) | pass | 0 deliveries pending or in flight at the end |
| database integrity (foreign keys, constraints, indexes) | pass | ok |
| watches and subscriptions match the workload | pass | 300 watches, 5 subscriptions |
| versions recorded match the registry at the freeze | pass | 300 packages: 1960 baseline and 3753 new versions, as published |
| one event per new version | pass | 3753 events for 3753 new versions (1960 baseline versions without events) |
| one delivery per event and subscription | pass | 18765 deliveries = 3753 events x 5 subscriptions |
| no delivery given up (dead) | pass | 18765 delivered, 0 dead, 0 unfinished |
| every release published before the freeze reached every sink (registry) | pass | 18765 of 18765 expected (3753 new versions x 5 sinks) |
| every delivery in the database received at least once (zero lost) | pass | 18765 of 18765 finished deliveries received |
| duplicates only within the idempotency key | pass | 196 duplicate requests over 196 deliveries, each with its delivery's key and body |
| every request signed (Standard Webhooks) | pass | 18961 requests verified |
| no request outside the database's deliveries | pass | none |
| no target checked by two holders at once (from the scheduler logs) | pass | 1026 checks of 300 targets by 86 holders (1 cut off by a kill), none overlapping |

## Counts

| | |
|---|---|
| Versions recorded (baseline, new) | 1960, 3753 |
| Events | 3753 |
| Deliveries (rows) | 18765 |
| Deliveries attempted more than once | 196 |
| Requests received | 18961 |
| Distinct deliveries received | 18765 |
| Duplicate requests (deliveries received twice or more) | 196 (196) |
| Received before the first kill | 207 |
| Expected from the registry: new versions, deliveries | 3753, 18765 |

## Kills

| | |
|---|---|
| Kills | 100 |
| Kills timed from the process start, not waiting for /readyz (startup to ready: see below) | 13 |
| Uptime after ready (drawn) | 0.2s to 3.0s, median 1.8s |
| Restart to ready | median 0.2s, max 0.3s |
| Kills during detection (checks committed since the previous kill) | 81 (81 with new events) |
| Requests received per life | min 0, median 41, max 101 |
| Kills that cut requests off at the receiver | 62 (335 requests) |
| Kills per replica | replica 1: 50, replica 2: 50 |

<details><summary>Every kill</summary>

| # | Replica | Timed from | Restart to ready | Uptime | Checks | Events | Received | Open at kill | Pending after | In flight after |
|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 1 | ready | 0.0s | 1.0s | 28 | 271 | 43 | 2 | 641 | 2 |
| 2 | 1 | start | 0.0s | 0.0s | 0 | 0 | 1 | 1 | 641 | 2 |
| 3 | 2 | ready | 0.3s | 0.3s | 1 | 9 | 13 | 6 | 667 | 8 |
| 4 | 1 | ready | 0.2s | 0.7s | 0 | 0 | 39 | 4 | 624 | 12 |
| 5 | 2 | ready | 0.2s | 2.1s | 5 | 48 | 61 | 0 | 803 | 12 |
| 6 | 1 | ready | 0.3s | 0.8s | 5 | 41 | 28 | 0 | 979 | 12 |
| 7 | 1 | ready | 0.2s | 2.1s | 3 | 31 | 61 | 0 | 1073 | 12 |
| 8 | 1 | ready | 0.2s | 2.7s | 2 | 22 | 85 | 4 | 1093 | 16 |
| 9 | 2 | ready | 0.3s | 0.6s | 3 | 30 | 15 | 8 | 1220 | 20 |
| 10 | 1 | ready | 0.2s | 0.5s | 0 | 0 | 20 | 3 | 1197 | 23 |
| 11 | 1 | start | 0.0s | 0.2s | 0 | 0 | 3 | 0 | 1197 | 23 |
| 12 | 1 | start | 0.0s | 0.1s | 0 | 0 | 0 | 0 | 1197 | 23 |
| 13 | 1 | start | 0.0s | 0.2s | 1 | 10 | 0 | 0 | 1247 | 23 |
| 14 | 1 | ready | 0.2s | 2.3s | 4 | 42 | 49 | 8 | 1389 | 31 |
| 15 | 1 | ready | 0.2s | 2.4s | 3 | 31 | 99 | 1 | 1444 | 32 |
| 16 | 1 | ready | 0.3s | 1.5s | 0 | 0 | 51 | 4 | 1389 | 36 |
| 17 | 2 | ready | 0.3s | 2.1s | 4 | 38 | 45 | 6 | 1516 | 42 |
| 18 | 2 | ready | 0.2s | 3.0s | 2 | 18 | 81 | 0 | 1522 | 42 |
| 19 | 1 | ready | 0.2s | 0.4s | 0 | 0 | 13 | 8 | 1501 | 50 |
| 20 | 2 | ready | 0.3s | 2.2s | 4 | 38 | 77 | 0 | 1614 | 50 |
| 21 | 1 | ready | 0.3s | 2.3s | 3 | 29 | 59 | 7 | 1693 | 58 |
| 22 | 1 | ready | 0.3s | 2.4s | 3 | 30 | 61 | 10 | 1768 | 68 |
| 23 | 1 | ready | 0.3s | 1.2s | 1 | 9 | 30 | 0 | 1783 | 66 |
| 24 | 1 | ready | 0.2s | 1.8s | 6 | 59 | 41 | 0 | 2037 | 66 |
| 25 | 2 | ready | 0.3s | 2.1s | 2 | 21 | 42 | 0 | 2100 | 66 |
| 26 | 2 | start | 0.0s | 0.2s | 0 | 0 | 3 | 8 | 2089 | 74 |
| 27 | 1 | ready | 0.3s | 1.4s | 2 | 21 | 39 | 2 | 2152 | 67 |
| 28 | 1 | ready | 0.2s | 1.8s | 3 | 31 | 48 | 0 | 2259 | 66 |
| 29 | 1 | ready | 0.3s | 0.6s | 1 | 9 | 19 | 0 | 2277 | 66 |
| 30 | 1 | start | 0.0s | 0.2s | 0 | 0 | 6 | 7 | 2263 | 74 |
| 31 | 1 | ready | 0.3s | 2.4s | 2 | 21 | 64 | 8 | 2290 | 74 |
| 32 | 1 | start | 0.0s | 0.1s | 0 | 0 | 3 | 3 | 2290 | 74 |
| 33 | 2 | ready | 0.3s | 2.5s | 3 | 28 | 68 | 5 | 2358 | 78 |
| 34 | 2 | ready | 0.3s | 1.6s | 1 | 9 | 37 | 0 | 2366 | 74 |
| 35 | 2 | ready | 0.3s | 2.7s | 5 | 50 | 94 | 1 | 2521 | 75 |
| 36 | 2 | ready | 0.2s | 3.0s | 4 | 38 | 68 | 5 | 2638 | 79 |
| 37 | 1 | ready | 0.2s | 1.8s | 2 | 19 | 42 | 0 | 2691 | 74 |
| 38 | 2 | ready | 0.2s | 0.6s | 1 | 11 | 22 | 0 | 2724 | 74 |
| 39 | 2 | ready | 0.2s | 0.3s | 1 | 11 | 13 | 4 | 2762 | 78 |
| 40 | 2 | start | 0.0s | 0.1s | 0 | 0 | 6 | 8 | 2751 | 86 |
| 41 | 2 | ready | 0.3s | 1.2s | 2 | 19 | 31 | 8 | 2804 | 86 |
| 42 | 2 | ready | 0.2s | 1.8s | 2 | 19 | 39 | 8 | 2853 | 94 |
| 43 | 1 | ready | 0.3s | 0.3s | 0 | 0 | 25 | 2 | 2824 | 88 |
| 44 | 1 | ready | 0.2s | 1.5s | 2 | 20 | 43 | 3 | 2877 | 90 |
| 45 | 2 | ready | 0.2s | 2.7s | 1 | 10 | 71 | 2 | 2854 | 92 |
| 46 | 2 | ready | 0.2s | 0.3s | 1 | 10 | 14 | 1 | 2889 | 93 |
| 47 | 2 | ready | 0.2s | 1.1s | 3 | 31 | 24 | 8 | 3003 | 101 |
| 48 | 1 | ready | 0.2s | 0.8s | 0 | 0 | 14 | 0 | 2984 | 93 |
| 49 | 1 | ready | 0.2s | 1.8s | 4 | 42 | 66 | 0 | 3128 | 93 |
| 50 | 2 | ready | 0.2s | 2.0s | 1 | 10 | 72 | 1 | 3105 | 94 |
| 51 | 2 | ready | 0.3s | 2.5s | 4 | 39 | 74 | 7 | 3219 | 101 |
| 52 | 2 | ready | 0.2s | 2.1s | 4 | 38 | 64 | 0 | 3340 | 101 |
| 53 | 2 | ready | 0.3s | 2.7s | 7 | 70 | 82 | 16 | 3592 | 117 |
| 54 | 2 | ready | 0.3s | 2.8s | 6 | 57 | 79 | 0 | 3797 | 109 |
| 55 | 1 | ready | 0.2s | 2.7s | 2 | 20 | 76 | 6 | 3815 | 115 |
| 56 | 2 | ready | 0.2s | 0.6s | 0 | 0 | 13 | 0 | 3802 | 111 |
| 57 | 2 | ready | 0.2s | 0.7s | 1 | 11 | 31 | 0 | 3826 | 111 |
| 58 | 2 | start | 0.0s | 0.1s | 0 | 0 | 0 | 0 | 3826 | 111 |
| 59 | 2 | start | 0.0s | 0.3s | 1 | 9 | 0 | 8 | 3863 | 119 |
| 60 | 2 | ready | 0.2s | 2.4s | 2 | 20 | 72 | 8 | 3884 | 127 |
| 61 | 2 | ready | 0.3s | 1.6s | 2 | 20 | 62 | 2 | 3913 | 129 |
| 62 | 2 | start | 0.0s | 0.0s | 0 | 0 | 0 | 2 | 3913 | 129 |
| 63 | 1 | ready | 0.3s | 2.4s | 5 | 52 | 72 | 0 | 4092 | 137 |
| 64 | 1 | ready | 0.2s | 1.2s | 3 | 30 | 30 | 8 | 4198 | 137 |
| 65 | 1 | ready | 0.3s | 2.9s | 1 | 10 | 64 | 0 | 4183 | 129 |
| 66 | 2 | ready | 0.2s | 0.2s | 0 | 0 | 18 | 10 | 4155 | 139 |
| 67 | 2 | ready | 0.2s | 2.7s | 1 | 10 | 69 | 8 | 4128 | 145 |
| 68 | 1 | ready | 0.3s | 2.4s | 1 | 9 | 59 | 11 | 4097 | 156 |
| 69 | 1 | ready | 0.3s | 1.3s | 6 | 60 | 30 | 2 | 4363 | 151 |
| 70 | 1 | ready | 0.3s | 1.9s | 4 | 41 | 101 | 2 | 4465 | 153 |
| 71 | 2 | ready | 0.2s | 1.6s | 1 | 10 | 53 | 0 | 4462 | 151 |
| 72 | 2 | start | 0.0s | 0.3s | 1 | 10 | 0 | 8 | 4504 | 159 |
| 73 | 1 | ready | 0.3s | 1.6s | 1 | 10 | 48 | 0 | 4507 | 157 |
| 74 | 2 | ready | 0.2s | 0.7s | 3 | 29 | 41 | 1 | 4616 | 152 |
| 75 | 2 | ready | 0.2s | 1.4s | 1 | 10 | 46 | 0 | 4624 | 148 |
| 76 | 1 | ready | 0.2s | 1.6s | 5 | 48 | 57 | 8 | 4798 | 156 |
| 77 | 2 | ready | 0.3s | 1.8s | 2 | 19 | 38 | 0 | 4853 | 148 |
| 78 | 1 | ready | 0.3s | 0.9s | 4 | 39 | 31 | 0 | 5017 | 148 |
| 79 | 1 | ready | 0.2s | 0.8s | 1 | 10 | 25 | 0 | 5042 | 148 |
| 80 | 2 | ready | 0.2s | 1.6s | 3 | 30 | 54 | 11 | 5127 | 159 |
| 81 | 2 | ready | 0.2s | 2.1s | 2 | 19 | 57 | 1 | 5175 | 146 |
| 82 | 2 | ready | 0.2s | 1.5s | 1 | 10 | 37 | 0 | 5188 | 145 |
| 83 | 1 | ready | 0.3s | 0.3s | 1 | 11 | 16 | 6 | 5226 | 142 |
| 84 | 1 | ready | 0.2s | 0.9s | 1 | 10 | 18 | 5 | 5253 | 142 |
| 85 | 1 | ready | 0.2s | 1.0s | 0 | 0 | 39 | 3 | 5210 | 139 |
| 86 | 1 | ready | 0.2s | 2.3s | 7 | 71 | 52 | 7 | 5510 | 139 |
| 87 | 1 | ready | 0.2s | 2.7s | 1 | 10 | 84 | 7 | 5475 | 140 |
| 88 | 2 | ready | 0.3s | 2.5s | 4 | 38 | 77 | 0 | 5592 | 132 |
| 89 | 1 | ready | 0.3s | 1.2s | 5 | 47 | 31 | 4 | 5785 | 137 |
| 90 | 1 | start | 0.0s | 0.2s | 0 | 0 | 6 | 7 | 5775 | 145 |
| 91 | 2 | ready | 0.2s | 1.9s | 5 | 49 | 41 | 0 | 5976 | 137 |
| 92 | 1 | ready | 0.2s | 2.1s | 2 | 21 | 60 | 0 | 6029 | 129 |
| 93 | 2 | ready | 0.3s | 2.7s | 1 | 9 | 85 | 2 | 5994 | 123 |
| 94 | 2 | ready | 0.2s | 0.9s | 1 | 9 | 27 | 7 | 6004 | 129 |
| 95 | 1 | ready | 0.2s | 1.8s | 2 | 19 | 35 | 0 | 6062 | 121 |
| 96 | 2 | ready | 0.3s | 0.7s | 2 | 20 | 25 | 0 | 6137 | 121 |
| 97 | 2 | ready | 0.2s | 2.6s | 2 | 20 | 76 | 5 | 6157 | 125 |
| 98 | 2 | ready | 0.2s | 0.2s | 2 | 21 | 12 | 3 | 6248 | 128 |
| 99 | 2 | ready | 0.2s | 2.8s | 5 | 52 | 98 | 4 | 6406 | 132 |
| 100 | 1 | ready | 0.2s | 2.2s | 5 | 53 | 54 | 0 | 6625 | 122 |

</details>

## Parameters

| | |
|---|---|
| Watches (pypi, fake registry) | 300, interval 5m, 3 versions each at start |
| Releases | 36000 per hour over all packages (one every 30.0s per package) |
| Webhook sinks and subscriptions | 5, each matching every event |
| Kills | 100, uptime uniform in [200ms, 3s) after /readyz; 10% in [0, 300ms) after the start |
| Warm-up | watches added in 8 groups, 30s apart; a kill waits for 500 pending deliveries |
| Receiver delay | uniform in [0, 250ms) per request |
| Dispatch starts the kills | after 200 requests received |
| Seed | 12227956151495396084 |

## Environment

| | |
|---|---|
| Machine | single VM, Postgres 16.15 (local server, not 17) and angelia on the same host |
| Mode | 2 × `angelia serve` (every role, its own holder each) as child processes on one Postgres 16.15 (Ubuntu 16.15-0ubuntu0.24.04.1) database, no container limits |
| OS, kernel | linux/amd64, 6.18.44-fc-v470 |
| CPU | Intel(R) Xeon(R) Processor @ 2.10GHz, 4 CPUs |
| Memory | 15.7 GiB |
| Go | harness go1.27.4, angelia go1.27.4 |
| Commit | 37e6c783c62abd9f2260cd11faf603d11f344c95 |
