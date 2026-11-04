package sqlite

import (
	"context"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// PruneHistory implements [store.Store]. It walks the event log in seq order,
// batch events at a time (see [pruneBatchSize] for the bounds). Each batch is
// one transaction that deletes the terminal deliveries of its events, then the
// events that have no deliveries left. Events and deliveries that are still
// pending or in flight are kept, so the write lock is held only for one batch.
//
// Age is the event's created_at. The delivery table has no timestamp of when a
// delivery reached its terminal state (next_attempt_at is the next schedule,
// not that moment), so a delivered or dead delivery is pruned together with its
// event, even if it finished more recently than the cutoff. For example, a
// delivery that died yesterday on an event older than the cutoff is deleted in
// this run. Keeping a longer dead-letter window needs a delivery timestamp,
// which is a migration and not part of this function.
func (s *Store) PruneHistory(ctx context.Context, before time.Time, batch int) (store.PruneResult, error) {
	return s.pruneHistory(ctx, before, batch, nil)
}

// pruneHistory is PruneHistory with a hook. afterBatch, when not nil, runs after
// each batch has committed and before the next one starts, so tests can run
// writers between batches. No transaction is open while it runs.
func (s *Store) pruneHistory(ctx context.Context, before time.Time, batch int, afterBatch func()) (store.PruneResult, error) {
	batch = pruneBatchSize(batch)
	cutoff := millis(before)
	var res store.PruneResult
	var after int64 // seq of the last event examined; 0 starts from the beginning
	for {
		var upTo int64 // seq of the last event of this batch; 0: no candidates left
		var deliveries, events int64
		err := s.withTx(ctx, func(q *Queries) error {
			seqs, err := q.PruneBatchEnd(ctx, PruneBatchEndParams{AfterSeq: after, Before: cutoff, Batch: int64(batch)})
			if err != nil {
				return wrap(err, "select prune batch", "")
			}
			if len(seqs) == 0 {
				return nil
			}
			upTo = seqs[len(seqs)-1]
			deliveries, err = q.PruneDeliveries(ctx, PruneDeliveriesParams{AfterSeq: after, UpToSeq: upTo, Before: cutoff})
			if err != nil {
				return wrap(err, "prune deliveries", "")
			}
			events, err = q.PruneEvents(ctx, PruneEventsParams{AfterSeq: after, UpToSeq: upTo, Before: cutoff})
			return wrap(err, "prune events", "")
		})
		if err != nil {
			return res, err
		}
		if upTo == 0 {
			return res, nil
		}
		res.Deliveries += deliveries
		res.Events += events
		after = upTo
		if afterBatch != nil {
			afterBatch()
		}
	}
}

// pruneBatchSize returns the events per prune transaction: DefaultPruneBatch
// for n <= 0, MaxPruneBatch for n above it, and n otherwise.
func pruneBatchSize(n int) int {
	switch {
	case n <= 0:
		return store.DefaultPruneBatch
	case n > store.MaxPruneBatch:
		return store.MaxPruneBatch
	}
	return n
}

// HighestPrunedSeq implements [store.Store]. The event_prune_seq trigger
// (migration 00013) records it on every event delete, in the deleting
// transaction.
func (s *Store) HighestPrunedSeq(ctx context.Context) (int64, error) {
	seq, err := s.q.HighestPrunedSeq(ctx)
	if err != nil {
		return 0, wrap(err, "highest pruned seq", "")
	}
	return seq, nil
}
