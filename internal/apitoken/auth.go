package apitoken

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/angelia-dev/angelia/internal/api"
	"github.com/angelia-dev/angelia/internal/store"
)

// Reason is why an authentication failed, for the failure metric and the
// access log only (ADR 0030 T10, T11). It never reaches the caller: every
// failure is the same 401.
type Reason string

// The failure reasons, a closed set.
const (
	// ReasonMissing: no bearer token was sent. The server sets it; the
	// Authenticator never sees a missing token.
	ReasonMissing Reason = "missing"
	// ReasonMalformed: the token fails the strict parse or its checksum.
	ReasonMalformed Reason = "malformed"
	// ReasonUnknown: no row has the token's id, or the hash does not match,
	// whatever the row's state.
	ReasonUnknown Reason = "unknown"
	// ReasonRevoked and ReasonExpired are given only after the hash matched.
	ReasonRevoked Reason = "revoked"
	ReasonExpired Reason = "expired"
)

// AuthError is the error of a failed authentication. It wraps
// [api.ErrUnauthenticated] and never holds the token or its hash.
type AuthError struct {
	Reason Reason
	// ID is the token id for ReasonRevoked and ReasonExpired (the hash
	// matched, so the caller held the token); "" otherwise.
	ID string
}

func (e *AuthError) Error() string {
	if e.ID != "" {
		return fmt.Sprintf("apitoken: %s token %s", e.Reason, e.ID)
	}
	return "apitoken: " + string(e.Reason) + " token"
}

// Unwrap makes every AuthError an [api.ErrUnauthenticated].
func (e *AuthError) Unwrap() error { return api.ErrUnauthenticated }

// FailureReason returns the reason of an authentication error, and false
// when err is not one.
func FailureReason(err error) (Reason, string, bool) {
	var ae *AuthError
	if errors.As(err, &ae) {
		return ae.Reason, ae.ID, true
	}
	return "", "", false
}

// Store is what the Authenticator needs of the store; [store.TokenStore]
// has it.
type Store interface {
	APITokenByID(ctx context.Context, id string) (store.APIToken, error)
	TouchAPITokens(ctx context.Context, used map[string]time.Time) error
}

// TouchInterval is the shortest time between two writes of one token's
// last use (ADR 0030 T6).
const TouchInterval = 5 * time.Minute

// dummyHash is compared with the hash of a token whose id has no row, so an
// unknown id takes the same CPU path as a known one (ADR 0030 T10).
var dummyHash = hashArray(Prefix + "dummy")

// Authenticator is the [api.Authenticator] of API tokens. It reads the row by
// the token's id on every call (no cache, ADR 0030 T6), compares hashes in
// constant time, and records each successful use in memory; [Authenticator.Run]
// writes the uses to the store at most once per token per [TouchInterval].
type Authenticator struct {
	store Store
	log   *slog.Logger
	now   func() time.Time

	mu sync.Mutex
	// pending is the latest successful use of each token not yet written.
	pending map[string]time.Time
	// written is when each token's last use was last written.
	written map[string]time.Time
}

var _ api.Authenticator = (*Authenticator)(nil)

// Option configures an Authenticator.
type Option func(*Authenticator)

// WithClock sets the clock (tests).
func WithClock(now func() time.Time) Option { return func(a *Authenticator) { a.now = now } }

// NewAuthenticator returns an Authenticator over s. A nil log discards.
func NewAuthenticator(s Store, log *slog.Logger, opts ...Option) *Authenticator {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	a := &Authenticator{store: s, log: log, now: time.Now, pending: map[string]time.Time{}, written: map[string]time.Time{}}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Authenticate implements [api.Authenticator]. Every failure wraps
// [api.ErrUnauthenticated] as an [*AuthError]; any other error is a store
// failure.
func (a *Authenticator) Authenticate(ctx context.Context, token string) (api.Principal, error) {
	id, err := Parse(token)
	if err != nil {
		return api.Principal{}, &AuthError{Reason: ReasonMalformed}
	}
	h := Hash(token)
	row, err := a.store.APITokenByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		subtle.ConstantTimeCompare(h, dummyHash[:])
		return api.Principal{}, &AuthError{Reason: ReasonUnknown}
	}
	if err != nil {
		return api.Principal{}, fmt.Errorf("apitoken: read token %s: %w", id, err)
	}
	if subtle.ConstantTimeCompare(h, row.Hash) != 1 {
		return api.Principal{}, &AuthError{Reason: ReasonUnknown}
	}
	now := a.now()
	switch {
	case row.RevokedAt != nil:
		return api.Principal{}, &AuthError{Reason: ReasonRevoked, ID: id}
	case row.ExpiresAt != nil && !row.ExpiresAt.After(now):
		return api.Principal{}, &AuthError{Reason: ReasonExpired, ID: id}
	}
	scope, ok := api.ParseScope(row.Scope)
	if !ok {
		return api.Principal{}, fmt.Errorf("apitoken: token %s has an unknown scope %q", id, row.Scope)
	}
	a.recordUse(id, now)
	return api.Principal{ID: id, Name: row.Name, Scope: scope}, nil
}

// recordUse remembers a successful use, keyed by token id, so the map never
// holds more entries than the table has tokens.
func (a *Authenticator) recordUse(id string, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if at.After(a.pending[id]) {
		a.pending[id] = at
	}
}

// Flush writes the recorded uses of the tokens whose last use was written at
// least [TouchInterval] ago, or never; with all, it writes every recorded
// use (graceful shutdown). The store write runs outside the Authenticator's
// lock, so authentication never waits for it, and it takes no config lock.
// On an error the uses stay recorded for the next flush.
func (a *Authenticator) Flush(ctx context.Context, all bool) error {
	now := a.now()
	a.mu.Lock()
	batch := map[string]time.Time{}
	for id, at := range a.pending {
		if all || now.Sub(a.written[id]) >= TouchInterval {
			batch[id] = at
		}
	}
	// Forget write times that no longer hold anything back.
	for id, w := range a.written {
		if _, ok := a.pending[id]; !ok && now.Sub(w) >= TouchInterval {
			delete(a.written, id)
		}
	}
	a.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	if err := a.store.TouchAPITokens(ctx, batch); err != nil {
		return fmt.Errorf("apitoken: record last use: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, at := range batch {
		a.written[id] = now
		// A use recorded during the write stays pending.
		if !a.pending[id].After(at) {
			delete(a.pending, id)
		}
	}
	return nil
}

// Messages of Run's log records.
const (
	msgFlushFailed         = "api token last use not recorded"
	msgShutdownFlushFailed = "api token last use not recorded at shutdown"
)

// Run flushes recorded uses every tick until ctx ends, then once more with
// every recorded use, on a context of its own bounded by shutdownTimeout. A
// failed flush is logged and retried at the next tick. It returns nil when
// ctx ends, and an error at once for a tick that is not positive.
func (a *Authenticator) Run(ctx context.Context, tick, shutdownTimeout time.Duration) error {
	if tick <= 0 {
		return fmt.Errorf("apitoken: flush tick %s is not positive", tick)
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if err := a.Flush(ctx, false); err != nil && ctx.Err() == nil {
				a.log.WarnContext(ctx, msgFlushFailed, "err", err)
			}
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
			defer cancel()
			if err := a.Flush(fctx, true); err != nil {
				a.log.WarnContext(fctx, msgShutdownFlushFailed, "err", err)
			}
			return nil
		}
	}
}

// pendingIDs returns the ids with a recorded use not yet written (tests).
func (a *Authenticator) pendingIDs() map[string]time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return maps.Clone(a.pending)
}
