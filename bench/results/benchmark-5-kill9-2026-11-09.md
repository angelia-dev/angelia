# Benchmark 5: kill -9 during dispatch

**Verdict: PASS.** Target ([architecture.md §6](../../docs/architecture.md#6-language-and-framework-phase-0-assumptions)): zero lost deliveries, duplicates only within the idempotency key, over 100 SIGKILLs during heavy dispatch.

Run 2026-11-09T21:43:09Z, 812.8s in total (warm-up 285.1s, kill loop 195.0s, drain 332.5s).

## Checks

| Check | Result | Detail |
|---|---|---|
| SIGKILLs during the run | pass | 100 of 100, each process ended by SIGKILL |
| every kill during dispatch (deliveries pending or in flight) | pass | 100 of 100 kills; 78 cut requests off at the receiver |
| kills during detection | pass | 82 kills found checks committed since the previous kill (want at least 30) |
| drain finished (every target checked after the freeze, every delivery finished) | pass | 0 deliveries pending or in flight at the end |
| database integrity (integrity_check, foreign_key_check) | pass | ok |
| watches and subscriptions match the workload | pass | 300 watches, 5 subscriptions |
| versions recorded match the registry at the freeze | pass | 300 packages: 1953 baseline and 3747 new versions, as published |
| one event per new version | pass | 3747 events for 3747 new versions (1953 baseline versions without events) |
| one delivery per event and subscription | pass | 18735 deliveries = 3747 events x 5 subscriptions |
| no delivery given up (dead) | pass | 18735 delivered, 0 dead, 0 unfinished |
| every release published before the freeze reached every sink (registry) | pass | 18735 of 18735 expected (3747 new versions x 5 sinks) |
| every delivery in the database received at least once (zero lost) | pass | 18735 of 18735 finished deliveries received |
| duplicates only within the idempotency key | pass | 553 duplicate requests over 536 deliveries, each with its delivery's key and body |
| every request signed (Standard Webhooks) | pass | 19288 requests verified |
| no request outside the database's deliveries | pass | none |

## Counts

| | |
|---|---|
| Versions recorded (baseline, new) | 1953, 3747 |
| Events | 3747 |
| Deliveries (rows) | 18735 |
| Deliveries attempted more than once | 536 |
| Requests received | 19288 |
| Distinct deliveries received | 18735 |
| Duplicate requests (deliveries received twice or more) | 553 (536) |
| Received before the first kill | 252 |
| Expected from the registry: new versions, deliveries | 3747, 18735 |

## Kills

| | |
|---|---|
| Kills | 100 |
| Kills timed from the process start, not waiting for /readyz (startup to ready: see below) | 10 |
| Uptime after ready (drawn) | 0.2s to 3.0s, median 1.6s |
| Restart to ready | median 0.1s, max 0.2s |
| Kills during detection (checks committed since the previous kill) | 82 (82 with new events) |
| Requests received per life | min 0, median 79, max 171 |
| Kills that cut requests off at the receiver | 78 (565 requests) |

<details><summary>Every kill</summary>

| # | Timed from | Restart to ready | Uptime | Checks | Events | Received | Open at kill | Pending after | In flight after |
|---|---|---|---|---|---|---|---|---|---|
| 1 | ready | 0.0s | 3.0s | 45 | 432 | 73 | 7 | 823 | 7 |
| 2 | ready | 0.1s | 0.4s | 0 | 0 | 22 | 8 | 794 | 15 |
| 3 | ready | 0.1s | 2.4s | 1 | 9 | 148 | 8 | 684 | 23 |
| 4 | ready | 0.1s | 0.3s | 1 | 11 | 21 | 8 | 714 | 31 |
| 5 | ready | 0.1s | 2.1s | 4 | 43 | 103 | 0 | 827 | 31 |
| 6 | ready | 0.1s | 2.8s | 3 | 32 | 119 | 8 | 860 | 39 |
| 7 | ready | 0.1s | 1.4s | 1 | 10 | 98 | 8 | 808 | 47 |
| 8 | ready | 0.1s | 2.8s | 2 | 20 | 127 | 7 | 777 | 55 |
| 9 | ready | 0.1s | 2.6s | 3 | 29 | 162 | 7 | 757 | 62 |
| 10 | ready | 0.1s | 1.5s | 3 | 29 | 100 | 8 | 795 | 70 |
| 11 | ready | 0.1s | 2.2s | 1 | 10 | 136 | 8 | 704 | 78 |
| 12 | ready | 0.1s | 1.3s | 2 | 18 | 74 | 8 | 716 | 86 |
| 13 | ready | 0.1s | 1.0s | 1 | 9 | 67 | 4 | 692 | 90 |
| 14 | ready | 0.1s | 1.0s | 0 | 0 | 61 | 8 | 623 | 98 |
| 15 | ready | 0.1s | 1.6s | 4 | 39 | 59 | 0 | 760 | 98 |
| 16 | ready | 0.2s | 0.6s | 1 | 9 | 40 | 8 | 757 | 106 |
| 17 | ready | 0.1s | 1.5s | 2 | 18 | 65 | 0 | 787 | 106 |
| 18 | ready | 0.1s | 2.4s | 1 | 9 | 111 | 0 | 721 | 106 |
| 19 | ready | 0.1s | 2.2s | 4 | 38 | 133 | 6 | 773 | 113 |
| 20 | ready | 0.1s | 1.6s | 2 | 19 | 96 | 8 | 767 | 121 |
| 21 | ready | 0.1s | 1.3s | 1 | 9 | 75 | 8 | 733 | 129 |
| 22 | ready | 0.1s | 1.3s | 2 | 21 | 75 | 7 | 758 | 136 |
| 23 | ready | 0.1s | 1.8s | 1 | 11 | 116 | 8 | 693 | 144 |
| 24 | ready | 0.1s | 1.0s | 2 | 21 | 53 | 8 | 737 | 152 |
| 25 | ready | 0.1s | 1.1s | 2 | 20 | 64 | 0 | 774 | 152 |
| 26 | ready | 0.1s | 1.6s | 4 | 40 | 102 | 8 | 864 | 160 |
| 27 | ready | 0.1s | 1.3s | 1 | 9 | 26 | 8 | 878 | 168 |
| 28 | ready | 0.1s | 1.1s | 0 | 0 | 47 | 0 | 834 | 168 |
| 29 | ready | 0.1s | 2.6s | 1 | 9 | 112 | 0 | 767 | 168 |
| 30 | ready | 0.1s | 2.5s | 4 | 39 | 110 | 0 | 852 | 168 |
| 31 | ready | 0.1s | 2.5s | 2 | 20 | 147 | 7 | 799 | 175 |
| 32 | ready | 0.1s | 1.0s | 1 | 9 | 54 | 8 | 784 | 183 |
| 33 | ready | 0.1s | 1.0s | 4 | 41 | 59 | 7 | 924 | 191 |
| 34 | ready | 0.1s | 3.0s | 4 | 38 | 171 | 7 | 937 | 199 |
| 35 | ready | 0.1s | 0.3s | 1 | 10 | 18 | 8 | 964 | 207 |
| 36 | ready | 0.1s | 2.4s | 6 | 62 | 147 | 8 | 1120 | 215 |
| 37 | ready | 0.1s | 2.2s | 5 | 50 | 88 | 0 | 1283 | 215 |
| 38 | ready | 0.1s | 0.5s | 0 | 0 | 27 | 8 | 1249 | 223 |
| 39 | ready | 0.1s | 1.3s | 1 | 10 | 81 | 8 | 1213 | 231 |
| 40 | start | 0.0s | 0.2s | 0 | 0 | 7 | 8 | 1204 | 239 |
| 41 | ready | 0.1s | 0.8s | 0 | 0 | 51 | 8 | 1148 | 247 |
| 42 | ready | 0.1s | 0.3s | 1 | 9 | 13 | 8 | 1171 | 255 |
| 43 | ready | 0.1s | 1.1s | 0 | 0 | 65 | 8 | 1099 | 263 |
| 44 | ready | 0.1s | 1.9s | 1 | 10 | 53 | 8 | 1088 | 271 |
| 45 | start | 0.0s | 0.1s | 0 | 0 | 5 | 3 | 1088 | 271 |
| 46 | ready | 0.1s | 1.4s | 2 | 20 | 85 | 7 | 1096 | 278 |
| 47 | ready | 0.1s | 2.5s | 3 | 30 | 123 | 0 | 1123 | 278 |
| 48 | ready | 0.1s | 0.2s | 2 | 18 | 18 | 6 | 1189 | 285 |
| 49 | start | 0.0s | 0.2s | 0 | 0 | 10 | 8 | 1177 | 293 |
| 50 | ready | 0.1s | 2.0s | 1 | 10 | 110 | 1 | 1120 | 294 |
| 51 | ready | 0.2s | 2.1s | 4 | 37 | 80 | 0 | 1225 | 294 |
| 52 | ready | 0.1s | 1.7s | 1 | 9 | 105 | 8 | 1157 | 302 |
| 53 | ready | 0.1s | 1.8s | 1 | 10 | 106 | 7 | 1097 | 309 |
| 54 | ready | 0.1s | 2.7s | 0 | 0 | 103 | 8 | 986 | 317 |
| 55 | start | 0.0s | 0.3s | 1 | 9 | 20 | 8 | 1011 | 325 |
| 56 | start | 0.0s | 0.3s | 0 | 0 | 19 | 7 | 994 | 333 |
| 57 | ready | 0.1s | 1.7s | 1 | 10 | 104 | 8 | 934 | 341 |
| 58 | ready | 0.1s | 1.6s | 3 | 31 | 65 | 0 | 1027 | 341 |
| 59 | ready | 0.1s | 2.5s | 1 | 10 | 121 | 0 | 956 | 341 |
| 60 | ready | 0.1s | 0.8s | 1 | 10 | 52 | 8 | 946 | 349 |
| 61 | start | 0.0s | 0.2s | 5 | 48 | 13 | 6 | 1177 | 354 |
| 62 | ready | 0.1s | 1.1s | 1 | 11 | 67 | 8 | 1158 | 362 |
| 63 | ready | 0.1s | 2.4s | 2 | 20 | 117 | 0 | 1143 | 362 |
| 64 | ready | 0.1s | 1.6s | 1 | 11 | 97 | 7 | 1094 | 369 |
| 65 | ready | 0.1s | 1.3s | 3 | 32 | 72 | 4 | 1180 | 373 |
| 66 | ready | 0.1s | 1.0s | 2 | 20 | 64 | 7 | 1209 | 381 |
| 67 | ready | 0.1s | 2.9s | 4 | 40 | 170 | 8 | 1234 | 389 |
| 68 | ready | 0.1s | 1.2s | 1 | 9 | 72 | 8 | 1203 | 397 |
| 69 | ready | 0.1s | 2.7s | 4 | 38 | 117 | 0 | 1277 | 397 |
| 70 | ready | 0.1s | 2.3s | 5 | 49 | 111 | 0 | 1411 | 397 |
| 71 | ready | 0.1s | 2.5s | 8 | 79 | 89 | 8 | 1709 | 405 |
| 72 | ready | 0.1s | 2.9s | 3 | 29 | 110 | 1 | 1742 | 406 |
| 73 | ready | 0.1s | 0.6s | 1 | 10 | 34 | 7 | 1751 | 413 |
| 74 | ready | 0.1s | 0.7s | 0 | 0 | 41 | 8 | 1704 | 421 |
| 75 | ready | 0.1s | 1.9s | 4 | 40 | 93 | 0 | 1827 | 406 |
| 76 | ready | 0.1s | 1.8s | 4 | 41 | 90 | 0 | 1954 | 394 |
| 77 | ready | 0.1s | 2.8s | 4 | 38 | 122 | 8 | 2018 | 398 |
| 78 | ready | 0.1s | 2.4s | 4 | 43 | 144 | 8 | 2090 | 398 |
| 79 | ready | 0.1s | 2.0s | 1 | 11 | 78 | 0 | 2077 | 390 |
| 80 | ready | 0.1s | 2.4s | 5 | 49 | 83 | 8 | 2239 | 390 |
| 81 | ready | 0.1s | 2.8s | 3 | 29 | 169 | 0 | 2225 | 383 |
| 82 | ready | 0.1s | 0.9s | 0 | 0 | 54 | 8 | 2171 | 383 |
| 83 | ready | 0.1s | 1.4s | 1 | 11 | 26 | 8 | 2192 | 391 |
| 84 | start | 0.0s | 0.1s | 0 | 0 | 3 | 5 | 2192 | 391 |
| 85 | ready | 0.1s | 1.1s | 4 | 38 | 54 | 5 | 2332 | 388 |
| 86 | ready | 0.1s | 0.9s | 0 | 0 | 53 | 8 | 2280 | 388 |
| 87 | ready | 0.1s | 1.9s | 1 | 9 | 125 | 8 | 2204 | 387 |
| 88 | ready | 0.1s | 3.0s | 1 | 10 | 125 | 7 | 2134 | 383 |
| 89 | ready | 0.1s | 1.8s | 5 | 50 | 85 | 0 | 2300 | 383 |
| 90 | ready | 0.1s | 1.3s | 2 | 18 | 19 | 8 | 2363 | 391 |
| 91 | ready | 0.1s | 1.6s | 0 | 0 | 88 | 8 | 2272 | 399 |
| 92 | start | 0.0s | 0.2s | 2 | 19 | 9 | 8 | 2358 | 407 |
| 93 | ready | 0.1s | 1.7s | 3 | 31 | 93 | 7 | 2423 | 407 |
| 94 | ready | 0.1s | 1.8s | 1 | 11 | 102 | 1 | 2389 | 395 |
| 95 | ready | 0.1s | 2.4s | 4 | 41 | 99 | 0 | 2505 | 385 |
| 96 | ready | 0.1s | 1.4s | 1 | 10 | 79 | 8 | 2476 | 385 |
| 97 | ready | 0.1s | 0.7s | 0 | 0 | 32 | 8 | 2445 | 385 |
| 98 | ready | 0.1s | 0.4s | 1 | 11 | 24 | 8 | 2469 | 393 |
| 99 | start | 0.0s | 0.1s | 1 | 10 | 7 | 9 | 2510 | 401 |
| 100 | start | 0.0s | 0.0s | 0 | 0 | 0 | 9 | 2510 | 401 |

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
| Seed | 15300236336851518209 |

## Environment

| | |
|---|---|
| Machine | cloud agent sandbox: Linux VM (Firecracker), 4 vCPU, 15.7 GiB, no other load |
| Mode | process (angelia as a child process, no container limits) |
| OS, kernel | linux/amd64, 6.18.44-fc-v310 |
| CPU | Intel(R) Xeon(R) Processor @ 2.10GHz, 4 CPUs |
| Memory | 15.7 GiB |
| Go | harness go1.27.3, angelia go1.27.3 |
| Commit | 2028a9b1dc93322bfadbc0c1723b57317564eb1d |
