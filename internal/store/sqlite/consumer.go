package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// Named consumers (story #79-B): the cursor table of the pull API.

// ListConsumers implements [store.APIReader].
func (s *Store) ListConsumers(ctx context.Context, afterName, afterID string, limit int) ([]store.Consumer, error) {
	rows, err := s.q.ListConsumersPage(ctx, ListConsumersPageParams{
		AfterName: afterName, AfterID: afterID, MaxRows: int64(limit),
	})
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
	return txOps{q: s.q}.ConsumerByName(ctx, name)
}

// MaxEventSeq implements [store.APIReader]: the larger of the newest event in
// the log and the highest pruned seq (event_prune, migration 00013).
func (s *Store) MaxEventSeq(ctx context.Context) (int64, error) {
	n, err := s.q.MaxEventSeq(ctx)
	if err != nil {
		return 0, wrap(err, "max event seq", "")
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
	err = o.q.CreateConsumer(ctx, CreateConsumerParams{
		ID: c.ID, Name: c.Name, FilterJson: filter, AckedSeq: c.AckedSeq, AckedAt: nullMillis(c.AckedAt),
		CreatedAt: millis(c.CreatedAt), UpdatedAt: millis(updated),
	})
	return wrap(err, "create consumer", c.Name)
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
	if err != nil {
		return wrap(err, "update consumer", c.Name)
	}
	if n == 0 {
		return fmt.Errorf("store: update consumer %s: %w", c.ID, store.ErrNotFound)
	}
	return nil
}

func (o txOps) DeleteConsumer(ctx context.Context, id string) error {
	n, err := o.q.DeleteConsumer(ctx, id)
	if err != nil {
		return wrap(err, "delete consumer", id)
	}
	if n == 0 {
		return fmt.Errorf("store: delete consumer %s: %w", id, store.ErrNotFound)
	}
	return nil
}

func (o txOps) AckConsumer(ctx context.Context, name string, seq int64, now time.Time) (store.Consumer, error) {
	if seq < 0 {
		return store.Consumer{}, fmt.Errorf("store: ack consumer %s: negative seq %d: %w", name, seq, store.ErrInvalid)
	}
	r, err := o.q.AckConsumer(ctx, AckConsumerParams{
		Seq: seq, Now: sql.NullInt64{Int64: millis(now), Valid: true}, Name: name,
	})
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
