package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// LeaseDueDeliveries implements [store.Store] under the lease lock (ADR 0028
// §3). Writers that take no lease lock (FailPendingDeliveries,
// FailHeldDelivery, CancelHeldDeliveries) can change or delete a selected row
// before it is set in flight: StartDeliveryAttempt re-checks the due
// predicate, and a row that no longer matches is skipped and its new lease
// dropped, instead of being revived or failing the batch.
func (s *Store) LeaseDueDeliveries(ctx context.Context, holder string, now, until time.Time, limit int) ([]store.LeasedDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	if holder == "" || !until.After(now) {
		return nil, fmt.Errorf("store: lease due deliveries: empty holder or until not after now: %w", store.ErrInvalid)
	}
	var out []store.LeasedDelivery
	err := s.withTx(ctx, lockLease, func(q *Queries) error {
		ids, err := q.ListLeasableDueDeliveries(ctx, ListLeasableDueDeliveriesParams{Now: millis(now), MaxRows: maxRows(limit)})
		if err != nil {
			return wrap(err, "list due deliveries", holder)
		}
		s.leaseSelected()
		out = make([]store.LeasedDelivery, 0, len(ids))
		for _, id := range ids {
			name := store.DeliveryLeaseName(id)
			if err := acquire(ctx, q, name, holder, now, until); err != nil {
				return err
			}
			ld, ok, err := startAttempt(ctx, q, id, holder, now)
			if err != nil {
				return err
			}
			if !ok {
				if err := q.ReleaseLease(ctx, ReleaseLeaseParams{Name: name, Holder: holder}); err != nil {
					return wrap(err, "release lease", name)
				}
				continue
			}
			out = append(out, ld)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// startAttempt sets a leased delivery in flight and loads its event,
// subscription and sink. It reports false when the delivery is no longer due.
func startAttempt(ctx context.Context, q *Queries, id, holder string, now time.Time) (store.LeasedDelivery, bool, error) {
	var ld store.LeasedDelivery
	row, err := q.StartDeliveryAttempt(ctx, StartDeliveryAttemptParams{ID: id, Now: millis(now), Holder: holder})
	if errors.Is(err, sql.ErrNoRows) {
		return ld, false, nil
	}
	if err != nil {
		return ld, false, wrap(err, "start delivery attempt", id)
	}
	ld.Delivery = deliveryFromRow(row)
	ev, err := q.GetEvent(ctx, row.EventID)
	if err != nil { // delivery.event_id has a foreign key
		return ld, false, wrap(err, "get event", row.EventID)
	}
	ld.Event = eventFromRow(ev)
	sub, err := q.GetSubscription(ctx, row.SubscriptionID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ld, true, nil
	case err != nil:
		return ld, false, wrap(err, "get subscription", row.SubscriptionID)
	}
	ld.Subscription = new(subscriptionFromRow(sub))
	k, err := q.GetSink(ctx, sub.SinkID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ld, true, nil
	case err != nil:
		return ld, false, wrap(err, "get sink", sub.SinkID)
	}
	ld.Sink = new(sinkFromRow(k))
	return ld, true, nil
}

// HasDueDeliveries implements [store.Store] with a read only.
func (s *Store) HasDueDeliveries(ctx context.Context, now time.Time) (bool, error) {
	ok, err := s.q.HasLeasableDueDeliveries(ctx, millis(now))
	return ok, wrap(err, "check due deliveries", "")
}

// CountDeliveriesByStatus implements [store.Store].
func (s *Store) CountDeliveriesByStatus(ctx context.Context) (map[string]int64, error) {
	rows, err := s.q.CountDeliveriesByStatus(ctx)
	if err != nil {
		return nil, wrap(err, "count deliveries by status", "")
	}
	out := make(map[string]int64, len(rows))
	for _, r := range rows {
		out[r.Status] = r.N
	}
	return out, nil
}

// FinishDelivery implements [store.Store], under the lease lock.
func (s *Store) FinishDelivery(ctx context.Context, f store.DeliveryFinish, holder string) (bool, error) {
	p := FinishLeasedDeliveryParams{
		Status: f.Status, ID: f.ID, Attempt: f.Attempt, LeaseName: store.DeliveryLeaseName(f.ID), Holder: holder,
		LastError: nullString(cleanText(f.LastError)),
	}
	switch f.Status {
	case store.DeliveryPending:
		if f.NextAttemptAt.IsZero() {
			return false, fmt.Errorf("store: finish delivery %s: pending without next attempt: %w", f.ID, store.ErrInvalid)
		}
		p.NextAttemptAt = nullMillis(f.NextAttemptAt)
	case store.DeliveryDelivered, store.DeliveryDead:
	default:
		return false, fmt.Errorf("store: finish delivery %s: status %q: %w", f.ID, f.Status, store.ErrInvalid)
	}
	var held bool
	err := s.withTx(ctx, lockLease, func(q *Queries) error {
		n, err := q.FinishLeasedDelivery(ctx, p)
		if err != nil {
			return wrap(err, "finish delivery", f.ID)
		}
		if n != 1 {
			return nil
		}
		held = true
		return wrap(q.ReleaseLease(ctx, ReleaseLeaseParams{Name: p.LeaseName, Holder: holder}), "release lease", p.LeaseName)
	})
	return held, err
}

// PruneHistory implements [store.Store] like the SQLite engine: batches of
// events in seq order, each one transaction that deletes the terminal
// deliveries of its events, then the events left without deliveries. It
// takes no advisory lock; the event_prune_seq trigger updates one row, so two
// prunes queue on it (ADR 0028 §2).
func (s *Store) PruneHistory(ctx context.Context, before time.Time, batch int) (store.PruneResult, error) {
	batch = pruneBatchSize(batch)
	cutoff := millis(before)
	var res store.PruneResult
	var after int64 // seq of the last event examined; 0 starts from the beginning
	for {
		var upTo int64 // seq of the last event of this batch; 0: no candidates left
		var deliveries, events int64
		err := s.withTx(ctx, lockNone, func(q *Queries) error {
			seqs, err := q.PruneBatchEnd(ctx, PruneBatchEndParams{AfterSeq: after, Before: cutoff, Batch: maxRows(batch)})
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

// HighestPrunedSeq implements [store.Store].
func (s *Store) HighestPrunedSeq(ctx context.Context) (int64, error) {
	return s.stmt().HighestPrunedSeq(ctx)
}

// cleanText replaces invalid UTF-8 and NUL with U+FFFD, so that an error
// text is stored the same on every engine (Postgres TEXT holds neither).
func cleanText(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "\uFFFD"), "\x00", "\uFFFD")
}
