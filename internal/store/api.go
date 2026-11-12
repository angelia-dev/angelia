package store

import (
	"context"
	"encoding/json"
	"time"
)

// APIStore is what the REST API (internal/api, story #71) needs beyond
// [Store]: reads by name and by page, and the writes of replay. It is a
// separate interface so that the engine contract of [Store] stays as it is;
// every engine that serves the API implements both.
type APIStore interface {
	Store
	APIReader
	// SecretStore holds the stored secrets of /v1/secrets (story #75).
	SecretStore
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
	// WatchByName, SinkByName and SubscriptionByName return the row with the
	// name, whoever owns it (names are unique per resource type), or
	// ErrNotFound.
	WatchByName(ctx context.Context, name string) (Watch, error)
	SinkByName(ctx context.Context, name string) (Sink, error)
	SubscriptionByName(ctx context.Context, name string) (Subscription, error)
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
	// ListConsumers returns up to limit consumers ordered by (Name, ID),
	// after the position (afterName, afterID); "" and "" start at the
	// beginning. ErrInvalid when limit is not positive.
	ListConsumers(ctx context.Context, afterName, afterID string, limit int) ([]Consumer, error)
	// ConsumerByName returns the consumer with the name, or ErrNotFound.
	ConsumerByName(ctx context.Context, name string) (Consumer, error)
	// MaxEventSeq returns the head of the event log: the highest Seq ever
	// committed, pruned events included, that is
	// max(highest Seq in the log, HighestPrunedSeq). It is 0 on a store that
	// never held an event, and it never goes down, so a fully pruned log
	// still reports the Seq of its last event. `start: latest` of a consumer
	// and the upper limit of an ack are measured against it.
	MaxEventSeq(ctx context.Context) (int64, error)
}

// APITx is the unit of work of one API write.
type APITx interface {
	ConfigTx
	// SecretTx writes stored secrets in the same transaction as the rows
	// that reference them (a delete checks the referrers first).
	SecretTx
	WatchByName(ctx context.Context, name string) (Watch, error)
	SinkByName(ctx context.Context, name string) (Sink, error)
	SubscriptionByName(ctx context.Context, name string) (Subscription, error)
	SubscriptionsBySink(ctx context.Context, sinkID string) ([]Subscription, error)
	GetSink(ctx context.Context, id string) (Sink, error)
	GetSubscription(ctx context.Context, id string) (Subscription, error)
	GetDelivery(ctx context.Context, id string) (Delivery, error)
	ConsumerByName(ctx context.Context, name string) (Consumer, error)
	// MaxEventSeq and HighestPrunedSeq are the reads of [APIReader] and
	// [Store] inside the transaction, so a create reads its start position
	// (`latest` is MaxEventSeq, `earliest` is HighestPrunedSeq) in the
	// transaction that inserts the consumer and cannot race a prune.
	MaxEventSeq(ctx context.Context) (int64, error)
	HighestPrunedSeq(ctx context.Context) (int64, error)
	// CreateConsumer inserts c, with the AckedSeq and AckedAt it carries
	// (the API sets the start position there). ErrConflict when the name or
	// id exists, ErrInvalid for an empty id or name, a zero CreatedAt or a
	// Filter that is not JSON.
	CreateConsumer(ctx context.Context, c Consumer) error
	// UpdateConsumer replaces the filter of the consumer c.ID and sets its
	// UpdatedAt; every other field, AckedSeq included, is kept. ErrNotFound
	// when the consumer does not exist.
	UpdateConsumer(ctx context.Context, c Consumer) error
	// DeleteConsumer removes the consumer; ErrNotFound when it does not
	// exist.
	DeleteConsumer(ctx context.Context, id string) error
	// AckConsumer moves the cursor of the consumer called name to seq when
	// that is higher than AckedSeq, in one statement (max(acked_seq, seq)),
	// so concurrent acks never move it back. A lower or equal seq changes
	// nothing, AckedAt included; no ack changes UpdatedAt. It returns the
	// consumer as it is afterwards; ErrNotFound when it does not exist,
	// ErrInvalid for a negative seq. Whether seq exists in the log is the
	// caller's check (against MaxEventSeq).
	AckConsumer(ctx context.Context, name string, seq int64, now time.Time) (Consumer, error)
	// ListReplayCandidates returns the deliveries a bulk replay selects,
	// oldest event first; the caller filters on the event time.
	ListReplayCandidates(ctx context.Context, q ReplayQuery) ([]DeliveryRow, error)
	// ReplayDelivery queues a dead or delivered delivery again, due at now:
	// it becomes pending with NextAttemptAt now and no hold. Attempts and
	// LastError are kept, and ReplayBaseAttempts becomes Attempts, so the
	// retry budget starts again (spec docs/api.md decision 28). ErrNotFound when the
	// delivery does not exist or is neither dead nor delivered.
	ReplayDelivery(ctx context.Context, id string, now time.Time) error
}

// Consumer is a named server-side cursor over the event log (spec
// docs/api.md, story #79).
type Consumer struct {
	// ID is a ULID, immutable.
	ID string
	// Name is unique.
	Name string
	// Filter is the stored event filter ({watch, type, source}) as JSON;
	// empty means "{}" (no filter).
	Filter json.RawMessage
	// AckedSeq is the highest acknowledged event Seq. It is 0 only on a log
	// that never lost an event: `start: earliest` stores the highest pruned
	// Seq (oldest Seq - 1), the position just before the oldest event.
	AckedSeq int64
	// AckedAt is the time of the last ack that moved AckedSeq; zero when no
	// ack did.
	AckedAt   time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
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
