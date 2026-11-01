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
// transaction when batch <= 0. MaxPruneBatch is the upper bound: a larger batch
// is cut down to it, so one transaction stays short.
const (
	DefaultPruneBatch = 500
	MaxPruneBatch     = 5000
)

// Tx is the unit of work of one check: it reads the known set and the
// target's watches, and writes together the known set, the watches' baseline
// flag, the events and deliveries (transactional outbox, ADR 0004) and the
// target's fetch state.
type Tx interface {
	// ListVersions returns the known set of a target, ordered by
	// FirstSeenAt then Raw.
	ListVersions(ctx context.Context, targetID string) ([]Version, error)
	// ListWatches returns the watches on a target, ordered by ID.
	ListWatches(ctx context.Context, targetID string) ([]Watch, error)
	// AddVersion records a version in the known set. It reports false, without
	// error, when (TargetID, Raw) is already known.
	AddVersion(ctx context.Context, v Version) (bool, error)
	// UpdateVersion stores Yanked and RemovedAt of the known version
	// (TargetID, Raw); its other fields are immutable. ErrNotFound when the
	// version is not known.
	UpdateVersion(ctx context.Context, v Version) error
	// MarkBaselineDone sets the watch's BaselineDone. Marking a watch twice
	// is not an error; ErrNotFound when the watch does not exist.
	MarkBaselineDone(ctx context.Context, watchID string) error
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
	// when batch <= 0, MaxPruneBatch when larger). An event is kept while any
	// of its deliveries is pending or in flight, and so are its deliveries.
	// Delivered and dead deliveries follow their event's age, not the time they
	// finished (see the sqlite engine's PruneHistory). Config rows are never
	// touched. Callers pass before = now - retention.
	PruneHistory(ctx context.Context, before time.Time, batch int) (PruneResult, error)
	// InConfigTx runs fn in one transaction like InTx, with the operations a
	// declarative file apply needs.
	InConfigTx(ctx context.Context, fn func(tx ConfigTx) error) error
	// AcquireLease takes or renews the named lease for holder until the given
	// time. It reports false when another holder owns an unexpired lease.
	AcquireLease(ctx context.Context, name, holder string, now, until time.Time) (bool, error)
	ReleaseLease(ctx context.Context, name, holder string) error
	Close() error
}

// Owners of config rows. A file apply reads and changes only rows it owns.
const (
	ManagedByAPI  = "api"
	ManagedByFile = "file"
)

// ConfigTx is the unit of work of a declarative file apply (docs/watch-file.md
// section 4). Every write belongs to one transaction.
type ConfigTx interface {
	Tx
	// ListManagedWatches, ListManagedSinks and ListManagedSubscriptions return
	// the rows owned by managedBy, ordered by name.
	ListManagedWatches(ctx context.Context, managedBy string) ([]Watch, error)
	ListManagedSinks(ctx context.Context, managedBy string) ([]Sink, error)
	ListManagedSubscriptions(ctx context.Context, managedBy string) ([]Subscription, error)
	// GetTarget returns ErrNotFound for an unknown id.
	GetTarget(ctx context.Context, id string) (Target, error)
	// FindTarget returns the target with this kind and coordinates, or
	// ErrNotFound.
	FindTarget(ctx context.Context, kind string, coordinates map[string]string) (Target, error)
	CreateTarget(ctx context.Context, t Target) error
	// SetTargetInterval stores the interval and the next check time of a target.
	SetTargetInterval(ctx context.Context, id string, interval time.Duration, nextCheckAt time.Time) error
	// DeleteTargetIfUnused deletes the target and its known versions when no
	// watch uses it, and reports whether it did.
	DeleteTargetIfUnused(ctx context.Context, id string) (bool, error)
	CreateWatch(ctx context.Context, w Watch) error
	// UpdateWatch stores TargetID, Policy, Labels and BaselineDone.
	UpdateWatch(ctx context.Context, w Watch) error
	DeleteWatch(ctx context.Context, id string) error
	CreateSink(ctx context.Context, s Sink) error
	// UpdateSink stores Kind and ConfigEncrypted.
	UpdateSink(ctx context.Context, s Sink) error
	DeleteSink(ctx context.Context, id string) error
	CreateSubscription(ctx context.Context, s Subscription) error
	// UpdateSubscription stores Filter, SinkID and Template.
	UpdateSubscription(ctx context.Context, s Subscription) error
	DeleteSubscription(ctx context.Context, id string) error
	// DeletePendingDeliveries deletes the pending deliveries of a
	// subscription and returns how many. In-flight, delivered and dead rows stay.
	DeletePendingDeliveries(ctx context.Context, subscriptionID string) (int64, error)
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
	// Interval is the check interval, the smallest interval among the watches
	// that use the target. Zero means not set.
	Interval time.Duration
}

// Watch is a user's interest in a target.
type Watch struct {
	ID           string
	TargetID     string
	Policy       json.RawMessage
	Labels       map[string]string
	BaselineDone bool
	// Name and ManagedBy identify a row owned by a declarative file (see
	// ConfigTx). Name is empty for rows without one; ManagedBy "" means
	// ManagedByAPI.
	Name      string
	ManagedBy string
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
	RemovedAt   time.Time // zero: listed upstream at the last check
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
	Name            string // see Watch.Name
	ManagedBy       string
}

// Subscription routes matching events to a sink.
type Subscription struct {
	ID        string
	Filter    json.RawMessage
	SinkID    string
	Template  string
	Name      string // see Watch.Name
	ManagedBy string
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
