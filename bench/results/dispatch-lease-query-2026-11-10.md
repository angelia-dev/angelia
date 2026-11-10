# Dispatcher lease query against the backlog (story #232)

`BenchmarkListLeasableDueDeliveries` in `internal/store/sqlite/lease_bench_test.go` runs the dispatcher's lease query (`ListLeasableDueDeliveries`, 16 rows: one poll for 16 free workers) against a SQLite store holding N due pending deliveries. The backlog is written by events fanning out to 100 subscriptions each (100 rows share one `next_attempt_at`), with the 16 oldest deliveries in flight under a live lease and N/10 more held until after now.

```
go test -run '^$' -bench ListLeasableDueDeliveries -benchtime 2s -count 3 ./internal/store/sqlite/
```

Intel Xeon @ 2.10GHz, 4 threads, Linux amd64, Go 1.27.3, modernc SQLite. Three runs each, time per query.

| Due pending | Before | After |
|---:|---:|---:|
| 1,000 | 0.89 / 1.06 / 1.00 ms | 0.21 / 0.24 / 0.22 ms |
| 10,000 | 10.5 / 9.5 / 9.4 ms | 0.28 / 0.31 / 0.26 ms |
| 100,000 | 113 / 85 / 111 ms | 0.25 / 0.23 / 0.22 ms |

Before: the plan was a `MULTI-INDEX OR` over the three kinds of due row followed by `USE TEMP B-TREE FOR ORDER BY`, so every lease read and sorted the whole due backlog (cost linear in the backlog; about 10 ms at 10k matches the 10.7k-pending figure from benchmark 5).

After: the query is a `UNION ALL` of three branches (pending with NULL `next_attempt_at`, pending and due, in flight), each reading the new index `delivery_status_next_attempt_at_id (status, next_attempt_at, id)` in order and stopping at the limit; the only sort is the merge of at most 3 x 16 rows. The cost per lease no longer depends on the backlog.

Postgres: there is no Postgres store yet (#85), so nothing was measured there; migration 00018 carries the note for its twin.
