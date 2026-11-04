package store

import (
	"context"
	"time"
)

// APIStore is what the REST API (internal/api, story #71) needs beyond
// [Store]: reads by name and by page, and the writes of replay. It is a
// separate interface so that the engine contract of [Store] stays as it is;
// every engine that serves the API implements both.
//
// Names are unique per owner only until #185 makes them unique per resource
// type, so the ByName lookups return every row with the name, ordered by
// ManagedBy then ID.
type APIStore interface {
	Store
	APIReader
	// InAPITx runs fn in one transaction like InConfigTx, with the API's
	// lookups and replay writes too.
	InAPITx(ctx context.Context, fn func(tx APITx) error) error
}

// APIReader holds the reads of the API. On the store they each run outside a
// transaction, so a read takes no write lock.
type APIReader interface {
	// ListNamedWatches returns up to limit watches that have a name, ordered
	// by (Name, ID), after the position (afterName, afterID); "" and "" start
	// at the beginning.
	ListNamedWatches(ctx context.Context, afterName, afterID string, limit int) ([]Watch, error)
	// ListNamedSinks and ListNamedSubscriptions are ListNamedWatches for
	// sinks and subscriptions.
	ListNamedSinks(ctx context.Context, afterName, afterID string, limit int) ([]Sink, error)
	ListNamedSubscriptions(ctx context.Context, afterName, afterID string, limit int) ([]Subscription, error)
	WatchesByName(ctx context.Context, name string) ([]Watch, error)
	SinksByName(ctx context.Context, name string) ([]Sink, error)
	SubscriptionsByName(ctx context.Context, name string) ([]Subscription, error)
	// SubscriptionsBySink returns the subscriptions that deliver to a sink,
	// ordered by name then ID.
	SubscriptionsBySink(ctx context.Context, sinkID string) ([]Subscription, error)
	// GetSink, GetSubscription, GetEvent and GetDelivery return ErrNotFound
	// for an unknown id.
	GetSink(ctx context.Context, id string) (Sink, error)
	GetSubscription(ctx context.Context, id string) (Subscription, error)
	GetEvent(ctx context.Context, id string) (Event, error)
	GetDelivery(ctx context.Context, id string) (Delivery, error)
	// HasEventsAfter reports whether an event with a Seq greater than seq
	// exists.
	HasEventsAfter(ctx context.Context, seq int64) (bool, error)
	// ListDeliveryPage returns one page of deliveries, newest event first.
	ListDeliveryPage(ctx context.Context, q DeliveryQuery) ([]DeliveryRow, error)
}

// APITx is the unit of work of one API write.
type APITx interface {
	ConfigTx
	WatchesByName(ctx context.Context, name string) ([]Watch, error)
	SinksByName(ctx context.Context, name string) ([]Sink, error)
	SubscriptionsByName(ctx context.Context, name string) ([]Subscription, error)
	SubscriptionsBySink(ctx context.Context, sinkID string) ([]Subscription, error)
	GetSink(ctx context.Context, id string) (Sink, error)
	GetSubscription(ctx context.Context, id string) (Subscription, error)
	GetDelivery(ctx context.Context, id string) (Delivery, error)
	// ListReplayCandidates returns the deliveries a bulk replay selects,
	// oldest event first; the caller filters on the event time.
	ListReplayCandidates(ctx context.Context, q ReplayQuery) ([]DeliveryRow, error)
	// ReplayDelivery queues a dead or delivered delivery again, due at now:
	// it becomes pending with NextAttemptAt now and no hold. Attempts and
	// LastError are kept (spec docs/api.md decision 28). ErrNotFound when the
	// delivery does not exist or is neither dead nor delivered.
	ReplayDelivery(ctx context.Context, id string, now time.Time) error
}

// DeliveryQuery selects a page of deliveries. Zero values select everything.
type DeliveryQuery struct {
	// Statuses keeps the deliveries in one of these states; empty keeps all.
	Statuses []string
	// SubscriptionIDs keeps the deliveries of these subscriptions; nil keeps
	// all, an empty non-nil slice keeps none.
	SubscriptionIDs []string
	// EventID keeps the deliveries of one event; "" keeps all.
	EventID string
	// BeforeSeq and BeforeID are the position of the last delivery of the
	// previous page; a zero BeforeSeq starts at the newest event.
	BeforeSeq int64
	BeforeID  string
	Limit     int
}

// ReplayQuery selects the deliveries a bulk replay considers: those in one
// of Statuses, of the event EventID (when not ""), of the subscription
// SubscriptionID (when not "").
type ReplayQuery struct {
	Statuses       []string
	EventID        string
	SubscriptionID string
}

// DeliveryRow is a delivery with the position and payload of its event.
type DeliveryRow struct {
	Delivery
	EventSeq int64
	// EventPayload is the stored envelope of the event; only
	// ListReplayCandidates fills it.
	EventPayload []byte
}
