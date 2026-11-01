// Package scheduler is the "scheduler" role (architecture.md §3.2, ADR 0009).
//
// It leases due targets from the store, checks each through a [Checker] on
// a bounded worker pool and records when to check it next:
//
//   - success: next = now + interval ± jitter (10% by default), error count
//     reset to zero, fetch state replaced by the one the check returned;
//   - error: next = now + backoff ± jitter, where backoff doubles from
//     BackoffBase with every consecutive error and is capped at 24h before
//     jitter applies; the fetch state is kept. A check that exceeds
//     Options.CheckTimeout counts as an error.
//
// The unit of work is the target, not the watch, so any number of watches
// on one upstream object cost one fetch per interval.
//
// Each check holds a lease named [store.TargetLeaseName] in the store's
// generic lease table. [store.Store.LeaseDueTargets] selects and leases due
// targets in one transaction (on Postgres: FOR UPDATE SKIP LOCKED), so two
// workers never check the same target at once, in one process or several.
// A running check renews its lease every TTL/3; a crashed worker's leases
// expire after the TTL and the target is picked up again. The final target
// write is fenced by the lease ([store.Store.UpdateTargetLeased]). Leases
// make a duplicate check rare, not impossible, so the [Checker]'s commit
// must be idempotent.
//
// Time and randomness are injected ([Clock], Options.Rand) so jitter,
// backoff and lease expiry are tested without sleeping.
package scheduler
