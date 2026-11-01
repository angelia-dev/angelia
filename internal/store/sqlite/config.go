package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// InConfigTx implements [store.Store].
func (s *Store) InConfigTx(ctx context.Context, fn func(tx store.ConfigTx) error) error {
	return s.withTx(ctx, func(q *Queries) error { return fn(txOps{q: q}) })
}

// The single-statement config writes of [store.Store] run on the shared
// operations in their own statement.

// CreateTarget implements [store.Store].
func (s *Store) CreateTarget(ctx context.Context, t store.Target) error {
	return txOps{q: s.q}.CreateTarget(ctx, t)
}

// GetTarget implements [store.Store].
func (s *Store) GetTarget(ctx context.Context, id string) (store.Target, error) {
	return txOps{q: s.q}.GetTarget(ctx, id)
}

// CreateWatch implements [store.Store].
func (s *Store) CreateWatch(ctx context.Context, w store.Watch) error {
	return txOps{q: s.q}.CreateWatch(ctx, w)
}

// GetWatch implements [store.Store].
func (s *Store) GetWatch(ctx context.Context, id string) (store.Watch, error) {
	r, err := s.q.GetWatch(ctx, id)
	if err != nil {
		return store.Watch{}, wrap(err, "get watch", id)
	}
	return watchFromRow(r)
}

// DeleteWatch implements [store.Store].
func (s *Store) DeleteWatch(ctx context.Context, id string) error {
	return txOps{q: s.q}.DeleteWatch(ctx, id)
}

// CreateSink implements [store.Store].
func (s *Store) CreateSink(ctx context.Context, k store.Sink) error {
	return txOps{q: s.q}.CreateSink(ctx, k)
}

// DeleteSink implements [store.Store].
func (s *Store) DeleteSink(ctx context.Context, id string) error {
	return txOps{q: s.q}.DeleteSink(ctx, id)
}

// CreateSubscription implements [store.Store].
func (s *Store) CreateSubscription(ctx context.Context, sub store.Subscription) error {
	return txOps{q: s.q}.CreateSubscription(ctx, sub)
}

// DeleteSubscription implements [store.Store].
func (s *Store) DeleteSubscription(ctx context.Context, id string) error {
	return txOps{q: s.q}.DeleteSubscription(ctx, id)
}

func owner(managedBy string) string {
	if managedBy == "" {
		return store.ManagedByAPI
	}
	return managedBy
}

func nullName(n string) sql.NullString { return sql.NullString{String: n, Valid: n != ""} }

func (o txOps) CreateTarget(ctx context.Context, t store.Target) error {
	coords, err := json.Marshal(nonNil(t.Coordinates)) // sorted keys: canonical
	if err != nil {
		return fmt.Errorf("store: target %s coordinates: %w", t.ID, err)
	}
	fs, err := rawJSON(t.FetchState, false)
	if err != nil {
		return fmt.Errorf("store: target %s fetch state: %w", t.ID, err)
	}
	if t.NextCheckAt.IsZero() {
		return fmt.Errorf("store: target %s: zero next_check_at: %w", t.ID, store.ErrInvalid)
	}
	return wrap(o.q.CreateTarget(ctx, CreateTargetParams{
		ID: t.ID, Kind: t.Kind, CoordinatesJson: string(coords), FetchStateJson: fs,
		LastCheckedAt: nullMillis(t.LastCheckedAt), NextCheckAt: millis(t.NextCheckAt), ErrorCount: t.ErrorCount,
		IntervalMs: nullInterval(t.Interval),
	}), "create target", t.ID)
}

func nullInterval(d time.Duration) sql.NullInt64 {
	return sql.NullInt64{Int64: d.Milliseconds(), Valid: d > 0}
}

func targetFromRow(r Target) (store.Target, error) {
	t := store.Target{
		ID: r.ID, Kind: r.Kind, FetchState: json.RawMessage(r.FetchStateJson),
		LastCheckedAt: fromNullMillis(r.LastCheckedAt), NextCheckAt: fromMillis(r.NextCheckAt), ErrorCount: r.ErrorCount,
	}
	if r.IntervalMs.Valid {
		t.Interval = time.Duration(r.IntervalMs.Int64) * time.Millisecond
	}
	if err := json.Unmarshal([]byte(r.CoordinatesJson), &t.Coordinates); err != nil {
		return store.Target{}, fmt.Errorf("store: target %s coordinates: %w", r.ID, err)
	}
	return t, nil
}

func (o txOps) GetTarget(ctx context.Context, id string) (store.Target, error) {
	r, err := o.q.GetTarget(ctx, id)
	if err != nil {
		return store.Target{}, wrap(err, "get target", id)
	}
	return targetFromRow(r)
}

func (o txOps) FindTarget(ctx context.Context, kind string, coordinates map[string]string) (store.Target, error) {
	coords, err := json.Marshal(nonNil(coordinates))
	if err != nil {
		return store.Target{}, fmt.Errorf("store: find target coordinates: %w", err)
	}
	r, err := o.q.GetTargetByCoordinates(ctx, GetTargetByCoordinatesParams{Kind: kind, CoordinatesJson: string(coords)})
	if err != nil {
		return store.Target{}, wrap(err, "find target", kind+string(coords))
	}
	return targetFromRow(r)
}

func (o txOps) SetTargetInterval(ctx context.Context, id string, interval time.Duration, nextCheckAt time.Time) error {
	if interval <= 0 || nextCheckAt.IsZero() {
		return fmt.Errorf("store: target %s: interval and next_check_at must be set: %w", id, store.ErrInvalid)
	}
	n, err := o.q.SetTargetInterval(ctx, SetTargetIntervalParams{IntervalMs: nullInterval(interval), NextCheckAt: millis(nextCheckAt), ID: id})
	return deleted(n, err, "set target interval", id)
}

func (o txOps) DeleteTargetIfUnused(ctx context.Context, id string) (bool, error) {
	// The known set goes first: the target delete is a no-op while a watch
	// uses it, and then its versions must stay.
	ws, err := o.q.ListWatchesByTarget(ctx, id)
	if err != nil {
		return false, wrap(err, "delete target", id)
	}
	if len(ws) > 0 {
		return false, nil
	}
	if err := o.q.DeleteTargetVersions(ctx, id); err != nil {
		return false, wrap(err, "delete target versions", id)
	}
	n, err := o.q.DeleteTargetIfUnused(ctx, id)
	if err != nil {
		return false, wrap(err, "delete target", id)
	}
	return n == 1, nil
}

func (o txOps) CreateWatch(ctx context.Context, w store.Watch) error {
	labels, err := json.Marshal(nonNil(w.Labels))
	if err != nil {
		return fmt.Errorf("store: watch %s labels: %w", w.ID, err)
	}
	policy, err := rawJSON(w.Policy, false)
	if err != nil {
		return fmt.Errorf("store: watch %s policy: %w", w.ID, err)
	}
	return wrap(o.q.CreateWatch(ctx, CreateWatchParams{
		ID: w.ID, TargetID: w.TargetID, PolicyJson: policy, LabelsJson: string(labels), BaselineDone: boolInt(w.BaselineDone),
		Name: nullName(w.Name), ManagedBy: owner(w.ManagedBy),
	}), "create watch", w.ID)
}

func (o txOps) UpdateWatch(ctx context.Context, w store.Watch) error {
	labels, err := json.Marshal(nonNil(w.Labels))
	if err != nil {
		return fmt.Errorf("store: watch %s labels: %w", w.ID, err)
	}
	policy, err := rawJSON(w.Policy, false)
	if err != nil {
		return fmt.Errorf("store: watch %s policy: %w", w.ID, err)
	}
	n, err := o.q.UpdateWatchConfig(ctx, UpdateWatchConfigParams{
		TargetID: w.TargetID, PolicyJson: policy, LabelsJson: string(labels), ID: w.ID,
	})
	return deleted(n, err, "update watch", w.ID)
}

func (o txOps) ResetWatchBaseline(ctx context.Context, id string) error {
	n, err := o.q.ResetWatchBaseline(ctx, id)
	return deleted(n, err, "reset watch baseline", id)
}

func (o txOps) DeleteWatch(ctx context.Context, id string) error {
	n, err := o.q.DeleteWatch(ctx, id)
	return deleted(n, err, "delete watch", id)
}

func (o txOps) ListManagedWatches(ctx context.Context, managedBy string) ([]store.Watch, error) {
	rows, err := o.q.ListWatchesManagedBy(ctx, managedBy)
	if err != nil {
		return nil, wrap(err, "list watches managed by", managedBy)
	}
	out := make([]store.Watch, len(rows))
	for i, r := range rows {
		if out[i], err = watchFromRow(r); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func sinkBytes(b []byte) []byte {
	if b == nil {
		return []byte{} // the column is NOT NULL
	}
	return b
}

func (o txOps) CreateSink(ctx context.Context, k store.Sink) error {
	return wrap(o.q.CreateSink(ctx, CreateSinkParams{
		ID: k.ID, Kind: k.Kind, ConfigJsonEncrypted: sinkBytes(k.ConfigEncrypted), Name: nullName(k.Name), ManagedBy: owner(k.ManagedBy),
	}), "create sink", k.ID)
}

func (o txOps) UpdateSink(ctx context.Context, k store.Sink) error {
	n, err := o.q.UpdateSink(ctx, UpdateSinkParams{Kind: k.Kind, ConfigJsonEncrypted: sinkBytes(k.ConfigEncrypted), ID: k.ID})
	return deleted(n, err, "update sink", k.ID)
}

func (o txOps) DeleteSink(ctx context.Context, id string) error {
	n, err := o.q.DeleteSink(ctx, id)
	return deleted(n, err, "delete sink", id)
}

func (o txOps) ListManagedSinks(ctx context.Context, managedBy string) ([]store.Sink, error) {
	rows, err := o.q.ListSinksManagedBy(ctx, managedBy)
	if err != nil {
		return nil, wrap(err, "list sinks managed by", managedBy)
	}
	out := make([]store.Sink, len(rows))
	for i, r := range rows {
		out[i] = store.Sink{ID: r.ID, Kind: r.Kind, ConfigEncrypted: r.ConfigJsonEncrypted, Name: r.Name.String, ManagedBy: r.ManagedBy}
	}
	return out, nil
}

func (o txOps) CreateSubscription(ctx context.Context, sub store.Subscription) error {
	filter, err := rawJSON(sub.Filter, false)
	if err != nil {
		return fmt.Errorf("store: subscription %s filter: %w", sub.ID, err)
	}
	policy, err := rawJSON(sub.Policy, false)
	if err != nil {
		return fmt.Errorf("store: subscription %s policy: %w", sub.ID, err)
	}
	return wrap(o.q.CreateSubscription(ctx, CreateSubscriptionParams{
		ID: sub.ID, FilterJson: filter, SinkID: sub.SinkID, Template: sub.Template, Name: nullName(sub.Name), ManagedBy: owner(sub.ManagedBy),
		PolicyJson: policy,
	}), "create subscription", sub.ID)
}

func (o txOps) UpdateSubscription(ctx context.Context, sub store.Subscription) error {
	filter, err := rawJSON(sub.Filter, false)
	if err != nil {
		return fmt.Errorf("store: subscription %s filter: %w", sub.ID, err)
	}
	policy, err := rawJSON(sub.Policy, false)
	if err != nil {
		return fmt.Errorf("store: subscription %s policy: %w", sub.ID, err)
	}
	n, err := o.q.UpdateSubscription(ctx, UpdateSubscriptionParams{
		FilterJson: filter, SinkID: sub.SinkID, Template: sub.Template, PolicyJson: policy, ID: sub.ID,
	})
	return deleted(n, err, "update subscription", sub.ID)
}

func (o txOps) DeleteSubscription(ctx context.Context, id string) error {
	n, err := o.q.DeleteSubscription(ctx, id)
	return deleted(n, err, "delete subscription", id)
}

func (o txOps) ListManagedSubscriptions(ctx context.Context, managedBy string) ([]store.Subscription, error) {
	rows, err := o.q.ListSubscriptionsManagedBy(ctx, managedBy)
	if err != nil {
		return nil, wrap(err, "list subscriptions managed by", managedBy)
	}
	return subscriptionsFromRows(rows), nil
}

func (o txOps) ListSubscriptions(ctx context.Context) ([]store.Subscription, error) {
	rows, err := o.q.ListSubscriptions(ctx)
	if err != nil {
		return nil, wrap(err, "list subscriptions", "")
	}
	return subscriptionsFromRows(rows), nil
}

func subscriptionsFromRows(rows []Subscription) []store.Subscription {
	out := make([]store.Subscription, len(rows))
	for i, r := range rows {
		out[i] = store.Subscription{
			ID: r.ID, Filter: json.RawMessage(r.FilterJson), SinkID: r.SinkID, Template: r.Template,
			Policy: json.RawMessage(r.PolicyJson), Name: r.Name.String, ManagedBy: r.ManagedBy,
		}
	}
	return out
}

func (o txOps) FailPendingDeliveries(ctx context.Context, subscriptionID, reason string) (int64, error) {
	n, err := o.q.FailPendingDeliveries(ctx, FailPendingDeliveriesParams{
		LastError: sql.NullString{String: reason, Valid: true}, SubscriptionID: subscriptionID,
	})
	return n, wrap(err, "fail pending deliveries", subscriptionID)
}

func (o txOps) ListHeldDeliveries(ctx context.Context, subscriptionID string, now time.Time) ([]store.Delivery, error) {
	rows, err := o.q.ListHeldDeliveries(ctx, ListHeldDeliveriesParams{SubscriptionID: subscriptionID, HeldUntil: sql.NullInt64{Int64: millis(now), Valid: true}})
	if err != nil {
		return nil, wrap(err, "list held deliveries", subscriptionID)
	}
	return deliveriesFromRows(rows), nil
}

func (o txOps) ReleaseHeldDelivery(ctx context.Context, id string, now time.Time) error {
	n, err := o.q.ReleaseHeldDelivery(ctx, ReleaseHeldDeliveryParams{Now: nullMillis(now), ID: id})
	return deleted(n, err, "release held delivery", id)
}

func (o txOps) RescheduleHeldDelivery(ctx context.Context, id string, until, now time.Time) error {
	n, err := o.q.RescheduleHeldDelivery(ctx, RescheduleHeldDeliveryParams{Until: nullMillis(until), ID: id, Now: nullMillis(now)})
	return deleted(n, err, "reschedule held delivery", id)
}

func (o txOps) FailHeldDelivery(ctx context.Context, id, reason string, now time.Time) error {
	n, err := o.q.FailHeldDelivery(ctx, FailHeldDeliveryParams{Reason: sql.NullString{String: reason, Valid: true}, ID: id, Now: nullMillis(now)})
	return deleted(n, err, "fail held delivery", id)
}

var _ store.ConfigTx = txOps{}
