package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// The reads and writes of the REST API ([store.APIStore], story #71).

var _ store.APIStore = (*Store)(nil)

// allStatuses is the status filter that keeps every delivery.
var allStatuses = []string{store.DeliveryPending, store.DeliveryInFlight, store.DeliveryDelivered, store.DeliveryDead}

// InAPITx implements [store.APIStore].
func (s *Store) InAPITx(ctx context.Context, fn func(tx store.APITx) error) error {
	return s.withTx(ctx, func(q *Queries) error { return fn(txOps{q: q}) })
}

// ListNamedWatches implements [store.APIReader].
func (s *Store) ListNamedWatches(ctx context.Context, afterName, afterID string, limit int) ([]store.Watch, error) {
	rows, err := s.q.ListNamedWatchesPage(ctx, ListNamedWatchesPageParams{
		AfterName: sql.NullString{String: afterName, Valid: true}, AfterID: afterID, MaxRows: int64(limit),
	})
	if err != nil {
		return nil, wrap(err, "list named watches", afterName)
	}
	return watchesFromRows(rows)
}

// ListNamedSinks implements [store.APIReader].
func (s *Store) ListNamedSinks(ctx context.Context, afterName, afterID string, limit int) ([]store.Sink, error) {
	rows, err := s.q.ListNamedSinksPage(ctx, ListNamedSinksPageParams{
		AfterName: sql.NullString{String: afterName, Valid: true}, AfterID: afterID, MaxRows: int64(limit),
	})
	if err != nil {
		return nil, wrap(err, "list named sinks", afterName)
	}
	return sinksFromRows(rows), nil
}

// ListNamedSubscriptions implements [store.APIReader].
func (s *Store) ListNamedSubscriptions(ctx context.Context, afterName, afterID string, limit int) ([]store.Subscription, error) {
	rows, err := s.q.ListNamedSubscriptionsPage(ctx, ListNamedSubscriptionsPageParams{
		AfterName: sql.NullString{String: afterName, Valid: true}, AfterID: afterID, MaxRows: int64(limit),
	})
	if err != nil {
		return nil, wrap(err, "list named subscriptions", afterName)
	}
	return subscriptionsFromRows(rows), nil
}

// WatchByName implements [store.APIReader].
func (s *Store) WatchByName(ctx context.Context, name string) (store.Watch, error) {
	return txOps{q: s.q}.WatchByName(ctx, name)
}

// SinkByName implements [store.APIReader].
func (s *Store) SinkByName(ctx context.Context, name string) (store.Sink, error) {
	return txOps{q: s.q}.SinkByName(ctx, name)
}

// SubscriptionByName implements [store.APIReader].
func (s *Store) SubscriptionByName(ctx context.Context, name string) (store.Subscription, error) {
	return txOps{q: s.q}.SubscriptionByName(ctx, name)
}

// SubscriptionsBySink implements [store.APIReader].
func (s *Store) SubscriptionsBySink(ctx context.Context, sinkID string) ([]store.Subscription, error) {
	return txOps{q: s.q}.SubscriptionsBySink(ctx, sinkID)
}

// GetSink implements [store.APIReader].
func (s *Store) GetSink(ctx context.Context, id string) (store.Sink, error) {
	return txOps{q: s.q}.GetSink(ctx, id)
}

// GetSubscription implements [store.APIReader].
func (s *Store) GetSubscription(ctx context.Context, id string) (store.Subscription, error) {
	return txOps{q: s.q}.GetSubscription(ctx, id)
}

// GetDelivery implements [store.APIReader].
func (s *Store) GetDelivery(ctx context.Context, id string) (store.Delivery, error) {
	return txOps{q: s.q}.GetDelivery(ctx, id)
}

// GetEvent implements [store.APIReader].
func (s *Store) GetEvent(ctx context.Context, id string) (store.Event, error) {
	r, err := s.q.GetEvent(ctx, id)
	if err != nil {
		return store.Event{}, wrap(err, "get event", id)
	}
	return eventFromRow(r), nil
}

// HasEventsAfter implements [store.APIReader].
func (s *Store) HasEventsAfter(ctx context.Context, seq int64) (bool, error) {
	ok, err := s.q.HasEventsAfter(ctx, seq)
	return ok, wrap(err, "has events after", fmt.Sprint(seq))
}

// ListDeliveryPage implements [store.APIReader].
func (s *Store) ListDeliveryPage(ctx context.Context, q store.DeliveryQuery) ([]store.DeliveryRow, error) {
	statuses := q.Statuses
	if len(statuses) == 0 {
		statuses = allStatuses
	}
	before := q.BeforeSeq
	if before <= 0 {
		before = 1<<63 - 1
	}
	rows, err := s.q.ListDeliveriesPage(ctx, ListDeliveriesPageParams{
		// A nil SubscriptionIDs encodes as null: every subscription.
		StatusesJson: jsonList(statuses), SubscriptionIdsJson: jsonList(q.SubscriptionIDs),
		EventID: q.EventID, BeforeSeq: before, BeforeID: q.BeforeID, MaxRows: int64(q.Limit),
	})
	if err != nil {
		return nil, wrap(err, "list deliveries page", "")
	}
	out := make([]store.DeliveryRow, len(rows))
	for i, r := range rows {
		out[i] = store.DeliveryRow{Delivery: deliveriesFromRows([]Delivery{r.Delivery})[0], EventSeq: r.EventSeq}
	}
	return out, nil
}

func watchesFromRows(rows []Watch) ([]store.Watch, error) {
	out := make([]store.Watch, len(rows))
	for i, r := range rows {
		var err error
		if out[i], err = watchFromRow(r); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func sinksFromRows(rows []Sink) []store.Sink {
	out := make([]store.Sink, len(rows))
	for i, r := range rows {
		out[i] = store.Sink{ID: r.ID, Kind: r.Kind, ConfigEncrypted: r.ConfigJsonEncrypted, Name: r.Name.String, ManagedBy: r.ManagedBy}
	}
	return out
}

func (o txOps) WatchByName(ctx context.Context, name string) (store.Watch, error) {
	r, err := o.q.GetWatchByName(ctx, nullName(name))
	if err != nil {
		return store.Watch{}, wrap(err, "watch by name", name)
	}
	ws, err := watchesFromRows([]Watch{r})
	if err != nil {
		return store.Watch{}, err
	}
	return ws[0], nil
}

func (o txOps) SinkByName(ctx context.Context, name string) (store.Sink, error) {
	r, err := o.q.GetSinkByName(ctx, nullName(name))
	if err != nil {
		return store.Sink{}, wrap(err, "sink by name", name)
	}
	return sinksFromRows([]Sink{r})[0], nil
}

func (o txOps) SubscriptionByName(ctx context.Context, name string) (store.Subscription, error) {
	r, err := o.q.GetSubscriptionByName(ctx, nullName(name))
	if err != nil {
		return store.Subscription{}, wrap(err, "subscription by name", name)
	}
	return subscriptionsFromRows([]Subscription{r})[0], nil
}

func (o txOps) SubscriptionsBySink(ctx context.Context, sinkID string) ([]store.Subscription, error) {
	rows, err := o.q.ListSubscriptionsBySink(ctx, sinkID)
	if err != nil {
		return nil, wrap(err, "subscriptions by sink", sinkID)
	}
	return subscriptionsFromRows(rows), nil
}

func (o txOps) GetSink(ctx context.Context, id string) (store.Sink, error) {
	r, err := o.q.GetSink(ctx, id)
	if err != nil {
		return store.Sink{}, wrap(err, "get sink", id)
	}
	return sinksFromRows([]Sink{r})[0], nil
}

func (o txOps) GetSubscription(ctx context.Context, id string) (store.Subscription, error) {
	r, err := o.q.GetSubscription(ctx, id)
	if err != nil {
		return store.Subscription{}, wrap(err, "get subscription", id)
	}
	return subscriptionsFromRows([]Subscription{r})[0], nil
}

func (o txOps) GetDelivery(ctx context.Context, id string) (store.Delivery, error) {
	r, err := o.q.GetDelivery(ctx, id)
	if err != nil {
		return store.Delivery{}, wrap(err, "get delivery", id)
	}
	return deliveriesFromRows([]Delivery{r})[0], nil
}

func (o txOps) ListReplayCandidates(ctx context.Context, q store.ReplayQuery) ([]store.DeliveryRow, error) {
	statuses := q.Statuses
	if len(statuses) == 0 {
		statuses = allStatuses
	}
	rows, err := o.q.ListReplayCandidates(ctx, ListReplayCandidatesParams{
		StatusesJson: jsonList(statuses), EventID: q.EventID, SubscriptionID: q.SubscriptionID,
	})
	if err != nil {
		return nil, wrap(err, "list replay candidates", "")
	}
	out := make([]store.DeliveryRow, len(rows))
	for i, r := range rows {
		out[i] = store.DeliveryRow{
			Delivery: deliveriesFromRows([]Delivery{r.Delivery})[0], EventSeq: r.EventSeq, EventPayload: []byte(r.PayloadJson),
		}
	}
	return out, nil
}

func (o txOps) ReplayDelivery(ctx context.Context, id string, now time.Time) error {
	n, err := o.q.ReplayDelivery(ctx, ReplayDeliveryParams{Now: nullMillis(now), ID: id})
	return deleted(n, err, "replay delivery", id)
}

// jsonList encodes a list for json_each; nil encodes as null.
func jsonList(v []string) string {
	b, _ := json.Marshal(v) // a []string always encodes
	return string(b)
}
