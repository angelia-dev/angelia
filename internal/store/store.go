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
)

// Store is the persistence contract. Every engine must pass the same tests.
type Store interface {
	CreateTarget(ctx context.Context, t Target) error
	GetTarget(ctx context.Context, id string) (Target, error)
	CreateWatch(ctx context.Context, w Watch) error
	GetWatch(ctx context.Context, id string) (Watch, error)
	// AddVersion records a version in the known set. It reports false, without
	// error, when (TargetID, Raw) is already known.
	AddVersion(ctx context.Context, v Version) (bool, error)
	ListVersions(ctx context.Context, targetID string) ([]Version, error)
	CreateSink(ctx context.Context, s Sink) error
	CreateSubscription(ctx context.Context, s Subscription) error
	// AppendEvent writes the event and its deliveries in one transaction
	// (transactional outbox, ADR 0004): either all rows are written or none.
	AppendEvent(ctx context.Context, e Event, ds []Delivery) error
	// ListEventsAfter returns up to limit events with an ID greater than
	// afterID, in ID order. An empty afterID starts from the beginning.
	ListEventsAfter(ctx context.Context, afterID string, limit int) ([]Event, error)
	ListDeliveries(ctx context.Context, eventID string) ([]Delivery, error)
	// AcquireLease takes or renews the named lease for holder until the given
	// time. It reports false when another holder owns an unexpired lease.
	AcquireLease(ctx context.Context, name, holder string, now, until time.Time) (bool, error)
	ReleaseLease(ctx context.Context, name, holder string) error
	Close() error
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

// Event is an immutable release event.
type Event struct {
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
type Delivery struct {
	ID             string
	EventID        string
	SubscriptionID string
	Status         string
	Attempts       int64
	NextAttemptAt  time.Time // zero: not scheduled
	LastError      string
}
