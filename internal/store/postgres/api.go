package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// The reads and writes of the REST API ([store.APIStore], story #71) and the
// named consumers (story #79).

// allStatuses is the status filter that keeps every delivery.
var allStatuses = []string{store.DeliveryPending, store.DeliveryInFlight, store.DeliveryDelivered, store.DeliveryDead}

// InAPITx implements [store.APIStore], under the config lock.
func (s *Store) InAPITx(ctx context.Context, fn func(tx store.APITx) error) error {
	return s.withTx(ctx, lockConfig, func(q *Queries) error { return fn(s.ops(q, kindConfig)) })
}

// ListNamedWatches implements [store.APIReader].
func (s *Store) ListNamedWatches(ctx context.Context, afterName, afterID string, limit int) ([]store.Watch, error) {
	rows, err := s.q.ListNamedWatchesPage(ctx, ListNamedWatchesPageParams{AfterName: afterName, AfterID: afterID, MaxRows: maxRows(limit)})
	if err != nil {
		return nil, wrap(err, "list named watches", afterName)
	}
	return watchesFromRows(rows)
}

// ListNamedSinks implements [store.APIReader].
func (s *Store) ListNamedSinks(ctx context.Context, afterName, afterID string, limit int) ([]store.Sink, error) {
	rows, err := s.q.ListNamedSinksPage(ctx, ListNamedSinksPageParams{AfterName: afterName, AfterID: afterID, MaxRows: maxRows(limit)})
	if err != nil {
		return nil, wrap(err, "list named sinks", afterName)
	}
	return sinksFromRows(rows), nil
}

// ListNamedSubscriptions implements [store.APIReader].
func (s *Store) ListNamedSubscriptions(ctx context.Context, afterName, afterID string, limit int) ([]store.Subscription, error) {
	rows, err := s.q.ListNamedSubscriptionsPage(ctx, ListNamedSubscriptionsPageParams{AfterName: afterName, AfterID: afterID, MaxRows: maxRows(limit)})
	if err != nil {
		return nil, wrap(err, "list named subscriptions", afterName)
	}
	return subscriptionsFromRows(rows), nil
}

// WatchByName implements [store.APIReader].
func (s *Store) WatchByName(ctx context.Context, name string) (store.Watch, error) {
	return s.stmt().WatchByName(ctx, name)
}

// SinkByName implements [store.APIReader].
func (s *Store) SinkByName(ctx context.Context, name string) (store.Sink, error) {
	return s.stmt().SinkByName(ctx, name)
}

// SubscriptionByName implements [store.APIReader].
func (s *Store) SubscriptionByName(ctx context.Context, name string) (store.Subscription, error) {
	return s.stmt().SubscriptionByName(ctx, name)
}

// SubscriptionsBySink implements [store.APIReader].
func (s *Store) SubscriptionsBySink(ctx context.Context, sinkID string) ([]store.Subscription, error) {
	return s.stmt().SubscriptionsBySink(ctx, sinkID)
}

// GetSink implements [store.APIReader].
func (s *Store) GetSink(ctx context.Context, id string) (store.Sink, error) {
	return s.stmt().GetSink(ctx, id)
}

// GetSubscription implements [store.APIReader].
func (s *Store) GetSubscription(ctx context.Context, id string) (store.Subscription, error) {
	return s.stmt().GetSubscription(ctx, id)
}

// GetDelivery implements [store.APIReader].
func (s *Store) GetDelivery(ctx context.Context, id string) (store.Delivery, error) {
	return s.stmt().GetDelivery(ctx, id)
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
	return ok, wrap(err, "has events after", strconv.FormatInt(seq, 10))
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
		StatusesJson: jsonList(statuses), SubscriptionIdsJson: string(jsonList(q.SubscriptionIDs)),
		EventID: q.EventID, BeforeSeq: before, BeforeID: q.BeforeID, MaxRows: maxRows(q.Limit),
	})
	if err != nil {
		return nil, wrap(err, "list deliveries page", "")
	}
	out := make([]store.DeliveryRow, len(rows))
	for i, r := range rows {
		out[i] = store.DeliveryRow{Delivery: deliveryFromRow(r.Delivery), EventSeq: r.EventSeq}
	}
	return out, nil
}

func (o txOps) WatchByName(ctx context.Context, name string) (store.Watch, error) {
	r, err := o.q.GetWatchByName(ctx, nullString(name))
	if err != nil {
		return store.Watch{}, wrap(err, "watch by name", name)
	}
	return watchFromRow(r)
}

func (o txOps) SinkByName(ctx context.Context, name string) (store.Sink, error) {
	r, err := o.q.GetSinkByName(ctx, nullString(name))
	if err != nil {
		return store.Sink{}, wrap(err, "sink by name", name)
	}
	return sinkFromRow(r), nil
}

func (o txOps) SubscriptionByName(ctx context.Context, name string) (store.Subscription, error) {
	r, err := o.q.GetSubscriptionByName(ctx, nullString(name))
	if err != nil {
		return store.Subscription{}, wrap(err, "subscription by name", name)
	}
	return subscriptionFromRow(r), nil
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
	return sinkFromRow(r), nil
}

func (o txOps) GetSubscription(ctx context.Context, id string) (store.Subscription, error) {
	r, err := o.q.GetSubscription(ctx, id)
	if err != nil {
		return store.Subscription{}, wrap(err, "get subscription", id)
	}
	return subscriptionFromRow(r), nil
}

func (o txOps) GetDelivery(ctx context.Context, id string) (store.Delivery, error) {
	r, err := o.q.GetDelivery(ctx, id)
	if err != nil {
		return store.Delivery{}, wrap(err, "get delivery", id)
	}
	return deliveryFromRow(r), nil
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
		out[i] = store.DeliveryRow{Delivery: deliveryFromRow(r.Delivery), EventSeq: r.EventSeq, EventPayload: []byte(r.PayloadJson)}
	}
	return out, nil
}

func (o txOps) ReplayDelivery(ctx context.Context, id string, now time.Time) error {
	n, err := o.q.ReplayDelivery(ctx, ReplayDeliveryParams{Now: nullMillis(now), ID: id})
	return deleted(n, err, "replay delivery", id)
}

// ListConsumers implements [store.APIReader].
func (s *Store) ListConsumers(ctx context.Context, afterName, afterID string, limit int) ([]store.Consumer, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list consumers: limit %d: %w", limit, store.ErrInvalid)
	}
	rows, err := s.q.ListConsumersPage(ctx, ListConsumersPageParams{AfterName: afterName, AfterID: afterID, MaxRows: maxRows(limit)})
	if err != nil {
		return nil, wrap(err, "list consumers", afterName)
	}
	out := make([]store.Consumer, len(rows))
	for i, r := range rows {
		out[i] = consumerFromRow(r)
	}
	return out, nil
}

// ConsumerByName implements [store.APIReader].
func (s *Store) ConsumerByName(ctx context.Context, name string) (store.Consumer, error) {
	return s.stmt().ConsumerByName(ctx, name)
}

// MaxEventSeq implements [store.APIReader].
func (s *Store) MaxEventSeq(ctx context.Context) (int64, error) {
	return s.stmt().MaxEventSeq(ctx)
}

// MaxEventSeq implements [store.APITx]: the larger of the newest event and
// the highest pruned seq.
func (o txOps) MaxEventSeq(ctx context.Context) (int64, error) {
	n, err := o.q.MaxEventSeq(ctx)
	if err != nil {
		return 0, wrap(err, "max event seq", "")
	}
	return n, nil
}

// HighestPrunedSeq implements [store.APITx].
func (o txOps) HighestPrunedSeq(ctx context.Context) (int64, error) {
	n, err := o.q.HighestPrunedSeq(ctx)
	if err != nil {
		return 0, wrap(err, "highest pruned seq", "")
	}
	return n, nil
}

func (o txOps) ConsumerByName(ctx context.Context, name string) (store.Consumer, error) {
	r, err := o.q.GetConsumerByName(ctx, name)
	if err != nil {
		return store.Consumer{}, wrap(err, "consumer by name", name)
	}
	return consumerFromRow(r), nil
}

func (o txOps) CreateConsumer(ctx context.Context, c store.Consumer) error {
	if c.ID == "" || c.Name == "" || c.CreatedAt.IsZero() || c.AckedSeq < 0 {
		return fmt.Errorf("store: consumer %s: empty or negative field: %w", c.Name, store.ErrInvalid)
	}
	filter, err := rawJSON(c.Filter, false)
	if err != nil {
		return fmt.Errorf("store: consumer %s: filter: %w", c.Name, err)
	}
	updated := c.UpdatedAt
	if updated.IsZero() {
		updated = c.CreatedAt
	}
	return wrap(o.q.CreateConsumer(ctx, CreateConsumerParams{
		ID: c.ID, Name: c.Name, FilterJson: filter, AckedSeq: c.AckedSeq, AckedAt: nullMillis(c.AckedAt),
		CreatedAt: millis(c.CreatedAt), UpdatedAt: millis(updated),
	}), "create consumer", c.Name)
}

func (o txOps) UpdateConsumer(ctx context.Context, c store.Consumer) error {
	if c.UpdatedAt.IsZero() {
		return fmt.Errorf("store: consumer %s: zero updated_at: %w", c.Name, store.ErrInvalid)
	}
	filter, err := rawJSON(c.Filter, false)
	if err != nil {
		return fmt.Errorf("store: consumer %s: filter: %w", c.Name, err)
	}
	n, err := o.q.UpdateConsumerFilter(ctx, UpdateConsumerFilterParams{FilterJson: filter, UpdatedAt: millis(c.UpdatedAt), ID: c.ID})
	return deleted(n, err, "update consumer", c.ID)
}

func (o txOps) DeleteConsumer(ctx context.Context, id string) error {
	n, err := o.q.DeleteConsumer(ctx, id)
	return deleted(n, err, "delete consumer", id)
}

func (o txOps) AckConsumer(ctx context.Context, name string, seq int64, now time.Time) (store.Consumer, error) {
	if seq < 0 {
		return store.Consumer{}, fmt.Errorf("store: ack consumer %s: negative seq %d: %w", name, seq, store.ErrInvalid)
	}
	r, err := o.q.AckConsumer(ctx, AckConsumerParams{Seq: seq, Now: millis(now), Name: name})
	if err != nil {
		return store.Consumer{}, wrap(err, "ack consumer", name)
	}
	return consumerFromRow(r), nil
}

func consumerFromRow(r Consumer) store.Consumer {
	return store.Consumer{
		ID: r.ID, Name: r.Name, Filter: json.RawMessage(r.FilterJson), AckedSeq: r.AckedSeq,
		AckedAt: fromNullMillis(r.AckedAt), CreatedAt: fromMillis(r.CreatedAt), UpdatedAt: fromMillis(r.UpdatedAt),
	}
}
