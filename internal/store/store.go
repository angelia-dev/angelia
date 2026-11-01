package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var (
	// ErrNotFound is returned when a requested row does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrConflict is returned when a write violates a uniqueness constraint.
	ErrConflict = errors.New("store: conflict")
	// ErrInvalidRef is returned when a write references a row that does not
	// exist, or a delete removes a row that other config rows still reference
	// (foreign key violation).
	ErrInvalidRef = errors.New("store: invalid reference")
	// ErrInvalid is returned when a value fails validation before any write.
	ErrInvalid = errors.New("store: invalid value")
)

// Delivery statuses. The schema rejects any other value.
const (
	DeliveryPending   = "pending"
	DeliveryInFlight  = "in_flight"
	DeliveryDelivered = "delivered"
	DeliveryDead      = "dead"
)

// DefaultEventLimit is used by ListEventsAfter when limit <= 0.
const DefaultEventLimit = 100

// DefaultPruneBatch is the number of events PruneHistory deletes per
// transaction when batch <= 0.
const DefaultPruneBatch = 500

// Tx is the set of writes that must commit together when a check finds new
// versions: the known set, the events and deliveries (transactional outbox,
// ADR 0004) and the target's fetch state.
type Tx interface {
	// AddVersion records a version in the known set. It reports false, without
	// error, when (TargetID, Raw) is already known.
	AddVersion(ctx context.Context, v Version) (bool, error)
	// AppendEvent writes the event and its deliveries. A delivery with an
	// empty EventID gets e.ID; a different EventID is ErrInvalid.
	AppendEvent(ctx context.Context, e Event, ds []Delivery) error
	// UpdateTarget stores FetchState, LastCheckedAt, NextCheckAt and
	// ErrorCount of t. Kind and Coordinates are immutable.
	UpdateTarget(ctx context.Context, t Target) error
}

// Store is the persistence contract. Every engine must pass the same tests.
// Its Tx methods each run in their own transaction.
type Store interface {
	Tx
	// InTx runs fn in one transaction: it commits if fn returns nil and rolls
	// every write back otherwise.
	InTx(ctx context.Context, fn func(tx Tx) error) error
	CreateTarget(ctx context.Context, t Target) error
	GetTarget(ctx context.Context, id string) (Target, error)
	CreateWatch(ctx context.Context, w Watch) error
	GetWatch(ctx context.Context, id string) (Watch, error)
	// DeleteWatch deletes a watch. Its events are kept (immutable log).
	DeleteWatch(ctx context.Context, id string) error
	ListVersions(ctx context.Context, targetID string) ([]Version, error)
	CreateSink(ctx context.Context, s Sink) error
	// DeleteSink deletes a sink. It is ErrInvalidRef while a subscription
	// uses it; deliveries do not block it.
	DeleteSink(ctx context.Context, id string) error
	CreateSubscription(ctx context.Context, s Subscription) error
	// DeleteSubscription deletes a subscription. Its deliveries are kept.
	DeleteSubscription(ctx context.Context, id string) error
	// ListEventsAfter returns up to limit events with a Seq greater than
	// afterSeq, in Seq (commit) order. afterSeq 0 starts from the beginning;
	// limit <= 0 means DefaultEventLimit.
	ListEventsAfter(ctx context.Context, afterSeq int64, limit int) ([]Event, error)
	ListDeliveries(ctx context.Context, eventID string) ([]Delivery, error)
	// PruneHistory deletes events created before the given time, with their
	// deliveries, in transactions of at most batch events (DefaultPruneBatch
	// when batch <= 0). An event is kept while any of its deliveries is
	// pending or in flight, and so are its deliveries. Config rows are never
	// touched. Callers pass before = now - retention.
	PruneHistory(ctx context.Context, before time.Time, batch int) (PruneResult, error)
	// AcquireLease takes or renews the named lease for holder until the given
	// time. It reports false when another holder owns an unexpired lease.
	AcquireLease(ctx context.Context, name, holder string, now, until time.Time) (bool, error)
	ReleaseLease(ctx context.Context, name, holder string) error
	Close() error
}

// PruneResult counts the rows PruneHistory deleted.
type PruneResult struct {
	Events     int64
	Deliveries int64
}

// Target is one upstream object, shared by every watch on it.
type Target struct {
	ID            string
	Kind          string
	Coordinates   map[string]string
	FetchState    json.RawMessage // opaque to the core (architecture.md §3.3)
	LastCheckedAt time.Time       // zero: never checked
	NextCheckAt   time.Time
	ErrorCount    int64
}

// Watch is a user's interest in a target.
type Watch struct {
	ID           string
	TargetID     string
	Policy       json.RawMessage
	Labels       map[string]string
	BaselineDone bool
}

// Version is one entry of a target's known version set.
type Version struct {
	TargetID    string
	Raw         string
	Normalized  string
	PublishedAt time.Time // zero: unknown
	Prerelease  bool
	Yanked      bool
	FirstSeenAt time.Time
}

// Event is an immutable release event. WatchID may name a deleted watch.
type Event struct {
	// Seq is the pull-API cursor, assigned by the store on append and
	// increasing in commit order; it is ignored on write. ID (a ULID) stays
	// the public event id.
	Seq       int64
	ID        string
	WatchID   string
	Type      string
	Payload   json.RawMessage
	CreatedAt time.Time
}

// Sink is a delivery destination. ConfigEncrypted is stored as given;
// encryption at rest is the caller's job (internal/secrets).
type Sink struct {
	ID              string
	Kind            string
	ConfigEncrypted []byte
}

// Subscription routes matching events to a sink.
type Subscription struct {
	ID       string
	Filter   json.RawMessage
	SinkID   string
	Template string
}

// Delivery is the per-subscription delivery state of one event.
// SubscriptionID may name a deleted subscription.
type Delivery struct {
	ID             string
	EventID        string
	SubscriptionID string
	Status         string // one of the Delivery* constants
	Attempts       int64
	NextAttemptAt  time.Time // zero: not scheduled
	LastError      string
}
