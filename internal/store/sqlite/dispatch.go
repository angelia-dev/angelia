package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// LeaseDueDeliveries implements [store.Store]. Like LeaseDueTargets it runs
// in one immediate transaction, so SQLite's write lock serializes callers.
func (s *Store) LeaseDueDeliveries(ctx context.Context, holder string, now, until time.Time, limit int) ([]store.LeasedDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	if holder == "" || !until.After(now) {
		return nil, fmt.Errorf("store: lease due deliveries: empty holder or until not after now: %w", store.ErrInvalid)
	}
	var out []store.LeasedDelivery
	err := s.withTx(ctx, func(q *Queries) error {
		ids, err := q.ListLeasableDueDeliveries(ctx, ListLeasableDueDeliveriesParams{Now: nullMillis(now), MaxRows: int64(limit)})
		if err != nil {
			return wrap(err, "list due deliveries", holder)
		}
		out = make([]store.LeasedDelivery, 0, len(ids))
		for _, id := range ids {
			name := store.DeliveryLeaseName(id)
			if err := q.DeleteExpiredLease(ctx, DeleteExpiredLeaseParams{Name: name, ExpiresAt: millis(now)}); err != nil {
				return wrap(err, "expire lease", name)
			}
			n, err := q.AcquireLease(ctx, AcquireLeaseParams{Name: name, Holder: holder, ExpiresAt: millis(until)})
			if err != nil {
				return wrap(err, "acquire lease", name)
			}
			if n != 1 { // the select excluded live leases, so this is a bug
				return fmt.Errorf("store: lease %s held by another holder: %w", name, store.ErrConflict)
			}
			ld, err := startAttempt(ctx, q, id)
			if err != nil {
				return err
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
// subscription and sink.
func startAttempt(ctx context.Context, q *Queries, id string) (store.LeasedDelivery, error) {
	var ld store.LeasedDelivery
	row, err := q.StartDeliveryAttempt(ctx, id)
	if err != nil {
		return ld, wrap(err, "start delivery attempt", id)
	}
	ld.Delivery = deliveriesFromRows([]Delivery{row})[0]
	ev, err := q.GetEvent(ctx, row.EventID)
	if err != nil { // delivery.event_id has a foreign key
		return ld, wrap(err, "get event", row.EventID)
	}
	ld.Event = store.Event{
		Seq: ev.Seq, ID: ev.ID, WatchID: ev.WatchID, Type: ev.Type,
		Payload: json.RawMessage(ev.PayloadJson), CreatedAt: fromMillis(ev.CreatedAt),
	}
	sub, err := q.GetSubscription(ctx, row.SubscriptionID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ld, nil
	case err != nil:
		return ld, wrap(err, "get subscription", row.SubscriptionID)
	}
	ld.Subscription = &subscriptionsFromRows([]Subscription{sub})[0]
	k, err := q.GetSink(ctx, sub.SinkID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ld, nil
	case err != nil:
		return ld, wrap(err, "get sink", sub.SinkID)
	}
	ld.Sink = &store.Sink{ID: k.ID, Kind: k.Kind, ConfigEncrypted: k.ConfigJsonEncrypted, Name: k.Name.String, ManagedBy: k.ManagedBy}
	return ld, nil
}

// HasDueDeliveries implements [store.Store].
func (s *Store) HasDueDeliveries(ctx context.Context, now time.Time) (bool, error) {
	ok, err := s.q.HasLeasableDueDeliveries(ctx, nullMillis(now))
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

// FinishDelivery implements [store.Store].
func (s *Store) FinishDelivery(ctx context.Context, f store.DeliveryFinish, holder string) (bool, error) {
	p := FinishLeasedDeliveryParams{
		Status: f.Status, ID: f.ID, Attempt: f.Attempt, LeaseName: store.DeliveryLeaseName(f.ID), Holder: holder,
		LastError: sql.NullString{String: f.LastError, Valid: f.LastError != ""},
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
	err := s.withTx(ctx, func(q *Queries) error {
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
