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
	// ListSubscriptions returns every subscription, ordered by ID, for the
	// match step of a check (internal/event).
	ListSubscriptions(ctx context.Context) ([]Subscription, error)
	// SubscriptionGeneration returns a counter the store increments on every
	// insert, update and delete of a subscription, by whichever path or
	// process made it. A reader that cached ListSubscriptions at generation g
	// has the current set exactly while the counter still reads g.
	SubscriptionGeneration(ctx context.Context) (int64, error)
	// AppendEvent writes the event and its deliveries. A delivery with an
	// empty EventID gets e.ID; a different EventID is ErrInvalid. An event is
	// immutable: there is no update, an ID already written is ErrConflict and
	// the stored row is left as it was, and the SQL schema rejects an UPDATE
	// of an event row. Only PruneHistory deletes events.
	AppendEvent(ctx context.Context, e Event, ds []Delivery) error
	// UpdateTarget stores FetchState, LastCheckedAt, NextCheckAt and
	// ErrorCount of t. Kind and Coordinates are immutable.
	UpdateTarget(ctx context.Context, t Target) error
	// ListPendingReleases returns the versions held by the watches of a
	// target (cooldown, ADR 0012), with the current Yanked and Removed state
	// of each from the known set, ordered by WatchID, HeldAt, Raw. Rows of a
	// watch that now points at another target are left out.
	ListPendingReleases(ctx context.Context, targetID string) ([]PendingRelease, error)
	// AddPendingRelease holds a known version for a watch. Yanked and
	// Removed are ignored. ErrConflict when the watch already holds Raw of
	// TargetID; ErrInvalidRef when the watch or the version (TargetID, Raw)
	// does not exist. Rows are keyed by (WatchID, TargetID, Raw), so rows
	// left on a watch's previous target never conflict.
	AddPendingRelease(ctx context.Context, p PendingRelease) error
	// DeletePendingRelease releases a version of targetID held by a watch
	// (emitted or dropped). ErrNotFound when the watch does not hold it.
	DeletePendingRelease(ctx context.Context, watchID, targetID, raw string) error
	// GetLatestEventBySubject returns the event with the greatest Seq that
	// the watch emitted with this CloudEvents subject (<kind>:<package>@<raw
	// version>) and one of types (full CloudEvents types). ErrNotFound when
	// there is none, or when types is empty.
	GetLatestEventBySubject(ctx context.Context, watchID, subject string, types ...string) (Event, error)
	// CancelHeldDeliveries deletes the deliveries of event eventID that are
	// still held at now (pending, Hold.Until after now) and returns how many.
	// A yank calls it with the event the watch emitted for the yanked version
	// (ADR 0014 section 4, ADR 0018): a subscription must not receive a
	// version the registry withdrew before the subscription was told of it.
	// Keying on the event, not on (watch, raw version), keeps a watch moved to
	// another target from cancelling holds of its previous package. Deliveries
	// whose hold ended, and the event, are left alone.
	CancelHeldDeliveries(ctx context.Context, eventID string, now time.Time) (int64, error)
	// DeletePendingReleasesOfWatch forgets every version a watch holds, on
	// any target, and returns how many rows it deleted. The watch-file apply
	// (#48, PR #132) must call it, in the same transaction, when it moves a
	// watch to another target: the held versions of the old target must not
	// come back if the watch later moves back.
	DeletePendingReleasesOfWatch(ctx context.Context, watchID string) (int64, error)
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
	// LeaseDueTargets atomically leases up to limit due targets for holder:
	// targets with NextCheckAt <= now that no unexpired lease named
	// TargetLeaseName(id) covers, oldest due first. Each returned target is
	// leased to holder until the given time; renew it with AcquireLease and
	// release it with ReleaseLease under TargetLeaseName. On Postgres this
	// becomes SELECT ... FOR UPDATE SKIP LOCKED.
	LeaseDueTargets(ctx context.Context, holder string, now, until time.Time, limit int) ([]Target, error)
	// CountDueTargets counts the targets LeaseDueTargets would return
	// without a limit: due and not under an unexpired lease.
	CountDueTargets(ctx context.Context, now time.Time) (int64, error)
	// HasDueTargets reports whether LeaseDueTargets would return anything,
	// with a read only: callers check it before leasing so that idle polls
	// take no write lock.
	HasDueTargets(ctx context.Context, now time.Time) (bool, error)
	// UpdateTargetLeased is UpdateTarget fenced by a lease: it writes only
	// while holder owns an unexpired lease named leaseName at now, and
	// reports false, writing nothing, otherwise. A missing target is
	// ErrNotFound.
	UpdateTargetLeased(ctx context.Context, t Target, leaseName, holder string, now time.Time) (bool, error)
	// DeleteExpiredTargetLeases deletes the target leases (TargetLeaseName)
	// that expired at or before now, left by crashed holders, and returns
	// how many it deleted.
	DeleteExpiredTargetLeases(ctx context.Context, now time.Time) (int64, error)
	// LeaseDueDeliveries atomically leases up to limit due deliveries for
	// holder (ADR 0017): pending ones whose NextAttemptAt has passed (zero
	// counts as due), and in-flight ones whose lease expired (a crashed or
	// stalled holder), that no unexpired lease named DeliveryLeaseName(id)
	// covers, oldest due first. Each is set in flight, its Attempts counts
	// this attempt, and it is leased to holder until the given time. A
	// LeasedDelivery whose subscription or sink no longer exists has a nil
	// Subscription or Sink. On Postgres this becomes SELECT ... FOR UPDATE
	// SKIP LOCKED.
	LeaseDueDeliveries(ctx context.Context, holder string, now, until time.Time, limit int) ([]LeasedDelivery, error)
	// HasDueDeliveries reports whether LeaseDueDeliveries would return
	// anything, with a read only, so that idle polls take no write lock.
	HasDueDeliveries(ctx context.Context, now time.Time) (bool, error)
	// FinishDelivery records the end of an attempt and releases its lease,
	// fenced by it: it writes only while the delivery is in flight at
	// attempt f.Attempt and holder still owns its lease row (even expired:
	// no one took it over), and reports false, writing nothing, otherwise. ErrInvalid for a status
	// other than pending, delivered or dead, or a pending one without
	// NextAttemptAt.
	FinishDelivery(ctx context.Context, f DeliveryFinish, holder string) (bool, error)
	Close() error
}

// DeliveryLeaseName is the name of the lease a dispatcher holds on a
// delivery while it sends it. The SQLite due-delivery query relies on this
// format.
func DeliveryLeaseName(deliveryID string) string { return "delivery:" + deliveryID }

// LeasedDelivery is a delivery leased to a dispatcher, with what sending it
// needs. Subscription and Sink are nil when the row no longer exists.
type LeasedDelivery struct {
	Delivery     Delivery
	Event        Event
	Subscription *Subscription
	Sink         *Sink
}

// DeliveryFinish is how a leased attempt ended.
type DeliveryFinish struct {
	ID string
	// Attempt is the leased Delivery.Attempts of the attempt that ended.
	Attempt int64
	// Status is DeliveryPending (retry at NextAttemptAt), DeliveryDelivered
	// or DeliveryDead.
	Status        string
	NextAttemptAt time.Time
	// LastError is the attempt's error; empty clears it.
	LastError string
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
	// UpdateWatch stores TargetID, Policy and Labels. It leaves BaselineDone
	// alone, so that an apply cannot undo a worker's baseline.
	UpdateWatch(ctx context.Context, w Watch) error
	// ResetWatchBaseline sets BaselineDone to false.
	ResetWatchBaseline(ctx context.Context, id string) error
	DeleteWatch(ctx context.Context, id string) error
	CreateSink(ctx context.Context, s Sink) error
	// UpdateSink stores Kind and ConfigEncrypted.
	UpdateSink(ctx context.Context, s Sink) error
	DeleteSink(ctx context.Context, id string) error
	CreateSubscription(ctx context.Context, s Subscription) error
	// UpdateSubscription stores Filter, Policy, SinkID and Template. An empty
	// Policy resets the stored policy to "{}".
	UpdateSubscription(ctx context.Context, s Subscription) error
	DeleteSubscription(ctx context.Context, id string) error
	// FailPendingDeliveries sets every pending delivery of a subscription to
	// dead with LastError reason and returns how many (watch-file.md D8: a
	// removed subscription's queue is kept visible, not deleted). In-flight,
	// delivered and dead rows are left alone.
	FailPendingDeliveries(ctx context.Context, subscriptionID, reason string) (int64, error)
	// ListHeldDeliveries returns the deliveries of a subscription that are
	// pending and still held at now (Hold.Until after now), ordered by ID.
	ListHeldDeliveries(ctx context.Context, subscriptionID string, now time.Time) ([]Delivery, error)
	// ReleaseHeldDelivery clears the end of the hold of a pending delivery
	// that is still held at now and makes it due at now. ErrNotFound when it
	// is not one (including a hold that already ended).
	ReleaseHeldDelivery(ctx context.Context, id string, now time.Time) error
	// RescheduleHeldDelivery moves the end of the hold (and NextAttemptAt) of
	// a pending delivery still held at now to until. ErrNotFound when it is
	// not one.
	RescheduleHeldDelivery(ctx context.Context, id string, until, now time.Time) error
	// FailHeldDelivery sets a pending delivery still held at now to dead with
	// LastError reason and clears the end of its hold. ErrNotFound when it is
	// not one.
	FailHeldDelivery(ctx context.Context, id, reason string, now time.Time) error
}

// TargetLeaseName is the name of the lease a scheduler holds on a target
// while it checks it. The SQLite due-target query relies on this format.
func TargetLeaseName(targetID string) string { return "target:" + targetID }

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

// PendingRelease is a known version that a watch's policy holds back until
// its cooldown ends (ADR 0012). It is deleted with its watch or version.
type PendingRelease struct {
	WatchID  string
	TargetID string
	Raw      string
	HeldAt   time.Time
	// Release is the policy engine's snapshot of the version as detected,
	// opaque to the store.
	Release json.RawMessage
	// Yanked and Removed are the version's state in the known set, filled
	// by ListPendingReleases.
	Yanked, Removed bool
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
	// RefEventID is set on a release.yanked event to the id of the event the
	// watch emitted for the yanked version (ADR 0018); "" otherwise. Like
	// WatchID it may name a deleted (pruned) event.
	RefEventID string
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
	ID       string
	Filter   json.RawMessage
	SinkID   string
	Template string
	// Policy is the narrowing policy (prerelease, cooldown) of the
	// subscription, opaque to the store; empty means "{}" (ADR 0014).
	Policy    json.RawMessage
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
	// Hold is set when the delivery was ever held by a subscription cooldown
	// (ADR 0014); nil otherwise. Hold.Until is zero once an edit released the
	// hold or failed the delivery. Currently held iff Status is pending and
	// Hold.Until is after now; then NextAttemptAt equals Hold.Until, so the
	// dispatcher needs no special case.
	Hold *Hold
}

// Hold records why and until when a delivery waits, and what is needed to
// decide again when its subscription is edited.
type Hold struct {
	// Until is when the hold ends; AppendEvent copies it to NextAttemptAt.
	Until time.Time
	// StartedAt is the instant the cooldowns count from (the watch policy's
	// cooldown start).
	StartedAt time.Time
	// WatchCooldown is the watch cooldown the version already waited.
	WatchCooldown time.Duration
	// Prerelease tells whether the version is a pre-release.
	Prerelease bool
	// WatchID and Raw identify the version, so a yank can cancel the hold.
	WatchID, Raw string
	// Input is the filter input (JSON, opaque to the store) the delivery was
	// matched with, so that an edited filter can be applied to it again.
	Input json.RawMessage
}
