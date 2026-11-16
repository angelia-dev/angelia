# Dispatch throughput against the worker ceiling (story #258)

`BenchmarkDispatchThroughput` in `internal/dispatch/throughput_bench_test.go` runs `Dispatcher.Run` with the real clock and the default options (8 workers, 1 s poll interval) until every outcome is recorded. `-benchtime 800x` means 800 deliveries per dispatcher, one per event, all due at the start. The fake sink sleeps uniform [0, 250 ms) from a fixed seed (mean 125 ms, as the bench/crash receiver), so the ceiling is 8 × dispatchers ÷ 125 ms: 64/s for one dispatcher, 128/s for two. The two-dispatcher case runs two Postgres store instances on one database, one per process. A wrapper around the dispatcher's `Store` interface, in the test file, counts the polls that reached the lease call and times that call.

```
ANGELIA_TEST_POSTGRES_DSN=postgres://…/…?sslmode=disable \
go test -run '^$' -bench DispatchThroughput -benchtime 800x -count 3 -timeout 60m ./internal/dispatch/
```

Without `ANGELIA_TEST_POSTGRES_DSN` the Postgres cases are skipped. Each run takes about 13 s per case after the fix and 60 s before it. CI runs it once with `-benchtime 1x` as a smoke test (a few seconds); the rate is not checked there, since timing on shared runners is noisy.

Development VM: 4 × Intel Xeon @ 2.10 GHz, Linux amd64, Go 1.27.4, modernc SQLite, Postgres 16 on localhost (TCP). Before: the parent commit `051e248`'s dispatcher with this benchmark; after: this change. Three runs each.

| Case | Before: rate, share of ceiling | After: rate, share of ceiling |
|---|---|---|
| `engine=sqlite/dispatchers=1` | 40.7 / 35.5 / 30.6 per s: 64% / 55% / 48% | 60.2 / 60.3 / 60.3 per s: **94% / 94% / 94%** |
| `engine=postgres/dispatchers=1` | 14.0 / 12.8 / 12.8 per s: 22% / 20% / 20% | 59.2 / 59.1 / 59.2 per s: **93% / 92% / 93%** |
| `engine=postgres/dispatchers=2` | 26.0 / 25.6 / 26.5 per s: 20% / 20% / 21% | 119.7 / 119.8 / 119.8 per s: **94% / 94% / 94%** |

| Case | Before: polls, rows per poll, lease call | After: polls, rows per poll, lease call |
|---|---|---|
| `engine=sqlite/dispatchers=1` | 744 / 723 / 695, 1.08–1.15, 0.71–0.74 ms | 790–793, 1.01, 0.70–0.80 ms |
| `engine=postgres/dispatchers=1` | 464 / 422 / 423, 1.72–1.90, 2.1–2.4 ms | 777–785, 1.02–1.03, 1.57–1.60 ms |
| `engine=postgres/dispatchers=2` | 867 / 844 / 884, 1.81–1.90, 2.3–2.5 ms | 1,568–1,573, 1.02, 1.73–1.79 ms |

Target (decision D22): 90% of the ceiling, 70% floor. Met on all three cases. The ceiling uses the nominal 125 ms mean; the seeded sequence's own mean is 129.9 ms over 800 draws and 127.3 ms over 1,600, which alone caps the share at 96% and 98%. Against that, the dispatcher reaches 95–98%; the rest is the drain at the end and the finish write in each worker's cycle.

## Cause

A finished attempt woke the dispatcher's poll loop only when the pool had been full. In steady state a poll leases about one row, because one attempt has just finished; if another attempt finishes while that poll runs, the pool is not full at that moment, so it sends no wake-up, and the slot waits for the 1 s poll timer. The longer a poll, the more often that happens: a Postgres poll (the `HasDueDeliveries` read, then the lease transaction) takes 2–3 times a SQLite one, so Postgres fell to 20% and SQLite to 50–65%. Benchmark 5 on Postgres saw the same gap (about 23%, [report](benchmark-5-kill9-postgres-2026-11-15.md)). The scheduler had the same rule until #225 (#224).

The fix wakes the loop on every finished attempt, whatever its outcome, and `Run` returns at once when the context is cancelled while a wake-up is ready. The lease call is not the bottleneck at this ceiling: about one row and 1.6–1.8 ms per poll. Batching the lease statements stays an open question of [ADR 0031](../../docs/adr/0031-postgres-concurrent-leasing-skip-locked.md) for remote databases with fast receivers.

Issue: angelia-dev/angelia#258.
