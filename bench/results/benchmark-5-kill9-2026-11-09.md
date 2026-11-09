# Benchmark 5: kill -9 during dispatch

**Verdict: PASS.** Target ([architecture.md §6](../../docs/architecture.md#6-language-and-framework-phase-0-assumptions)): zero lost deliveries, duplicates only within the idempotency key, over 100 SIGKILLs during heavy dispatch.

Run 2026-11-09T20:51:19Z, 605.6s in total (warm-up 277.2s, kill loop 177.8s, drain 150.3s).

## Checks

| Check | Result | Detail |
|---|---|---|
| SIGKILLs during the run | pass | 100 of 100, each process ended by SIGKILL |
| every kill during dispatch (deliveries pending or in flight) | pass | 100 of 100 kills; 67 cut requests off at the receiver |
| drain finished every delivery | pass | 0 deliveries pending or in flight at the end |
| versions recorded without gaps | pass | 300 packages |
| one event per new version | pass | 2414 events for 2414 new versions (904 baseline versions without events) |
| one delivery per event and subscription | pass | 12070 deliveries = 2414 events x 5 subscriptions |
| no delivery given up (dead) | pass | 12070 delivered, 0 dead, 0 unfinished |
| every delivery received at least once (zero lost) | pass | 12070 of 12070 finished deliveries received |
| duplicates only within the idempotency key | pass | 462 duplicate requests over 455 deliveries, each with its delivery's key and body |
| every request signed (Standard Webhooks) | pass | 12532 requests verified |
| no request outside the database's deliveries | pass | none |

## Counts

| | |
|---|---|
| Versions recorded (baseline, new) | 904, 2414 |
| Events | 2414 |
| Deliveries (rows) | 12070 |
| Deliveries attempted more than once | 458 |
| Requests received | 12532 |
| Distinct deliveries received | 12070 |
| Duplicate requests (deliveries received twice or more) | 462 (455) |
| Received before the first kill | 246 |

## Kills

| | |
|---|---|
| Kills | 100 |
| Uptime after ready (drawn) | 0.2s to 3.0s, median 1.7s |
| Restart to ready | median 0.1s, max 0.2s |
| Requests received per life | min 10, median 60, max 144 |
| Kills that cut requests off at the receiver | 67 (421 requests) |

<details><summary>Every kill</summary>

| # | Restart to ready | Uptime | Received | Open at kill | Pending after | In flight after |
|---|---|---|---|---|---|---|
| 1 | 0.0s | 1.9s | 123 | 8 | 763 | 8 |
| 2 | 0.1s | 2.9s | 144 | 0 | 1146 | 8 |
| 3 | 0.1s | 1.3s | 74 | 3 | 1379 | 11 |
| 4 | 0.1s | 1.6s | 80 | 0 | 1611 | 11 |
| 5 | 0.1s | 0.6s | 36 | 7 | 1643 | 18 |
| 6 | 0.1s | 0.6s | 42 | 8 | 1860 | 26 |
| 7 | 0.1s | 0.5s | 28 | 8 | 1985 | 34 |
| 8 | 0.1s | 1.4s | 28 | 7 | 2137 | 42 |
| 9 | 0.1s | 0.2s | 14 | 8 | 2237 | 50 |
| 10 | 0.1s | 0.3s | 18 | 8 | 2213 | 58 |
| 11 | 0.1s | 1.9s | 66 | 0 | 2458 | 58 |
| 12 | 0.1s | 2.6s | 71 | 0 | 3007 | 58 |
| 13 | 0.1s | 1.6s | 75 | 0 | 3287 | 58 |
| 14 | 0.1s | 1.9s | 68 | 7 | 3567 | 65 |
| 15 | 0.1s | 0.2s | 12 | 7 | 3791 | 72 |
| 16 | 0.1s | 2.3s | 85 | 7 | 4340 | 80 |
| 17 | 0.1s | 0.9s | 53 | 6 | 4287 | 87 |
| 18 | 0.1s | 3.0s | 119 | 7 | 4807 | 94 |
| 19 | 0.1s | 2.5s | 93 | 2 | 5448 | 96 |
| 20 | 0.1s | 1.3s | 34 | 0 | 5774 | 96 |
| 21 | 0.1s | 1.2s | 60 | 6 | 6123 | 103 |
| 22 | 0.1s | 2.9s | 133 | 0 | 6616 | 103 |
| 23 | 0.1s | 1.5s | 49 | 0 | 6977 | 103 |
| 24 | 0.1s | 2.6s | 54 | 0 | 7598 | 103 |
| 25 | 0.1s | 2.5s | 84 | 2 | 7982 | 106 |
| 26 | 0.1s | 1.8s | 48 | 6 | 8182 | 114 |
| 27 | 0.1s | 2.6s | 84 | 2 | 8788 | 116 |
| 28 | 0.1s | 0.3s | 11 | 6 | 8897 | 123 |
| 29 | 0.1s | 2.2s | 77 | 0 | 9172 | 123 |
| 30 | 0.1s | 0.6s | 19 | 0 | 9238 | 123 |
| 31 | 0.1s | 1.3s | 42 | 0 | 9641 | 123 |
| 32 | 0.1s | 3.0s | 97 | 0 | 9719 | 123 |
| 33 | 0.1s | 1.6s | 30 | 7 | 9682 | 130 |
| 34 | 0.1s | 1.3s | 60 | 6 | 9616 | 137 |
| 35 | 0.1s | 2.0s | 95 | 8 | 9514 | 145 |
| 36 | 0.1s | 2.4s | 83 | 7 | 9424 | 152 |
| 37 | 0.1s | 2.5s | 86 | 7 | 9336 | 159 |
| 38 | 0.1s | 1.3s | 63 | 6 | 9268 | 166 |
| 39 | 0.1s | 1.0s | 40 | 0 | 9229 | 166 |
| 40 | 0.1s | 1.0s | 54 | 1 | 9174 | 167 |
| 41 | 0.1s | 2.2s | 73 | 0 | 9100 | 167 |
| 42 | 0.1s | 2.6s | 81 | 7 | 9011 | 175 |
| 43 | 0.1s | 0.4s | 26 | 6 | 8982 | 182 |
| 44 | 0.1s | 2.5s | 82 | 2 | 8900 | 189 |
| 45 | 0.1s | 0.5s | 30 | 7 | 8863 | 196 |
| 46 | 0.1s | 2.5s | 82 | 3 | 8778 | 203 |
| 47 | 0.1s | 2.4s | 78 | 7 | 8695 | 210 |
| 48 | 0.1s | 2.9s | 104 | 6 | 8587 | 217 |
| 49 | 0.1s | 2.4s | 57 | 0 | 8532 | 217 |
| 50 | 0.1s | 2.3s | 71 | 8 | 8453 | 225 |
| 51 | 0.1s | 1.3s | 68 | 6 | 8382 | 232 |
| 52 | 0.1s | 3.0s | 118 | 0 | 8267 | 232 |
| 53 | 0.1s | 1.5s | 35 | 5 | 8227 | 239 |
| 54 | 0.1s | 2.3s | 79 | 7 | 8141 | 246 |
| 55 | 0.1s | 1.8s | 43 | 8 | 8093 | 254 |
| 56 | 0.1s | 2.3s | 61 | 0 | 8034 | 254 |
| 57 | 0.1s | 0.7s | 33 | 7 | 7994 | 262 |
| 58 | 0.1s | 1.6s | 66 | 0 | 7931 | 262 |
| 59 | 0.1s | 1.5s | 74 | 8 | 7849 | 270 |
| 60 | 0.1s | 0.4s | 19 | 7 | 7824 | 278 |
| 61 | 0.1s | 0.9s | 38 | 0 | 7789 | 278 |
| 62 | 0.1s | 2.6s | 136 | 2 | 7651 | 280 |
| 63 | 0.1s | 2.3s | 97 | 0 | 7555 | 280 |
| 64 | 0.1s | 0.6s | 34 | 7 | 7514 | 287 |
| 65 | 0.1s | 1.1s | 31 | 0 | 7485 | 287 |
| 66 | 0.1s | 2.6s | 82 | 7 | 7396 | 294 |
| 67 | 0.1s | 2.5s | 90 | 8 | 7309 | 294 |
| 68 | 0.2s | 1.9s | 55 | 5 | 7250 | 299 |
| 69 | 0.1s | 2.9s | 81 | 0 | 7173 | 296 |
| 70 | 0.1s | 2.4s | 78 | 6 | 7112 | 280 |
| 71 | 0.1s | 1.8s | 91 | 0 | 7045 | 256 |
| 72 | 0.1s | 1.1s | 57 | 7 | 6982 | 264 |
| 73 | 0.1s | 0.9s | 49 | 7 | 6928 | 272 |
| 74 | 0.1s | 2.1s | 62 | 7 | 6861 | 279 |
| 75 | 0.1s | 0.3s | 14 | 7 | 6843 | 286 |
| 76 | 0.1s | 2.0s | 62 | 8 | 6772 | 294 |
| 77 | 0.1s | 3.0s | 119 | 0 | 6671 | 280 |
| 78 | 0.1s | 0.4s | 20 | 8 | 6651 | 280 |
| 79 | 0.1s | 2.7s | 51 | 8 | 6601 | 281 |
| 80 | 0.1s | 0.6s | 35 | 8 | 6560 | 289 |
| 81 | 0.1s | 1.9s | 57 | 5 | 6509 | 289 |
| 82 | 0.1s | 0.5s | 28 | 6 | 6477 | 296 |
| 83 | 0.1s | 1.2s | 71 | 7 | 6404 | 302 |
| 84 | 0.1s | 0.4s | 19 | 7 | 6380 | 309 |
| 85 | 0.1s | 0.4s | 25 | 7 | 6353 | 317 |
| 86 | 0.1s | 2.8s | 73 | 0 | 6288 | 310 |
| 87 | 0.1s | 0.7s | 40 | 6 | 6241 | 317 |
| 88 | 0.1s | 1.6s | 81 | 4 | 6156 | 325 |
| 89 | 0.1s | 1.2s | 18 | 0 | 6140 | 325 |
| 90 | 0.1s | 2.7s | 102 | 7 | 6032 | 333 |
| 91 | 0.1s | 2.0s | 56 | 7 | 5971 | 337 |
| 92 | 0.1s | 2.0s | 65 | 6 | 5911 | 337 |
| 93 | 0.1s | 1.6s | 48 | 0 | 5864 | 337 |
| 94 | 0.1s | 0.9s | 30 | 0 | 5843 | 328 |
| 95 | 0.1s | 2.2s | 115 | 0 | 5728 | 328 |
| 96 | 0.1s | 1.7s | 42 | 0 | 5686 | 328 |
| 97 | 0.1s | 0.2s | 10 | 6 | 5670 | 335 |
| 98 | 0.1s | 0.3s | 11 | 7 | 5653 | 342 |
| 99 | 0.1s | 0.7s | 34 | 0 | 5620 | 342 |
| 100 | 0.1s | 2.3s | 46 | 0 | 5574 | 342 |

</details>

## Parameters

| | |
|---|---|
| Watches (pypi, fake registry) | 300, interval 5m, 3 versions each at start |
| Releases | 28800 per hour over all packages (one every 37.5s per package) |
| Webhook sinks and subscriptions | 5, each matching every event |
| Kills | 100, uptime after ready uniform in [200ms, 3s) |
| Receiver delay | uniform in [0, 250ms) per request |
| Dispatch starts the kills | after 200 requests received |
| Seed | 7287773271034903366 |

## Environment

| | |
|---|---|
| Machine | cloud agent sandbox: Linux VM (Firecracker), 4 vCPU, 15.7 GiB, no other load |
| Mode | process (angelia as a child process, no container limits) |
| OS, kernel | linux/amd64, 6.18.44-fc-v310 |
| CPU | Intel(R) Xeon(R) Processor @ 2.10GHz, 4 CPUs |
| Memory | 15.7 GiB |
| Go | harness go1.27.3, angelia go1.27.3 |
| Commit | 0c58f2d27da32f23fec47d560b23e19cc4c0b880 |
