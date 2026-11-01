package sqlite

import (
	"context"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// PruneHistory implements [store.Store]. It walks the event log in seq order,
// batch events at a time. Each batch is one transaction that deletes the
// terminal deliveries of its events, then the events that have no deliveries
// left. Events and deliveries that are still pending or in flight are kept, so
// the write lock is held only for one batch.
func (s *Store) PruneHistory(ctx context.Context, before time.Time, batch int) (store.PruneResult, error) {
	if batch <= 0 {
		batch = store.DefaultPruneBatch
	}
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
	}
}
