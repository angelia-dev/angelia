package store

import (
	"context"
	"fmt"
	"time"
)

// UISession is the stored form of a browser session of the admin UI (story
// #262, ADR 0033 §5). The store never holds the session cookie: Hash is the
// 32-byte SHA-256 of the cookie value, computed by internal/webauth.
type UISession struct {
	// ID is the session's ULID, the visible id inside the cookie.
	ID string
	// Hash is the 32-byte hash of the whole cookie value.
	Hash []byte
	// Provider is the identity provider ("github"), Subject its stable id
	// of the user, Login the user's login at sign-in.
	Provider, Subject, Login string
	// Name, AvatarURL and UserAgent are "" when unknown.
	Name, AvatarURL, UserAgent string
	CreatedAt                  time.Time
	// LastSeenAt is when the session last authenticated a request; it is
	// coalesced to about 5 minutes. A new session's is its CreatedAt.
	LastSeenAt time.Time
	// ExpiresAt is the absolute end of the session.
	ExpiresAt time.Time
	// RevokedAt is nil while the session is not revoked.
	RevokedAt *time.Time
	// RevokedBy is the revoker's principal id ("" while not revoked).
	RevokedBy string
}

// UISessionQuery selects a page of sessions for ListUISessions.
type UISessionQuery struct {
	// AfterID starts the page after this id; "" starts at the beginning.
	AfterID string
	// Limit is the page size; it must be positive.
	Limit int
	// IncludeEnded lists every row. Otherwise only active rows are listed:
	// not revoked, ExpiresAt after Now and LastSeenAt after IdleSince.
	IncludeEnded bool
	Now          time.Time
	IdleSince    time.Time
}

// SessionStore is what the admin UI's sessions and identity pins need from
// an engine (ADR 0033 §3, §5). Every write is a single statement: there is
// no transaction and no config lock.
type SessionStore interface {
	// UISessionByID returns the row with this id (revoked or not), or
	// ErrNotFound.
	UISessionByID(ctx context.Context, id string) (UISession, error)
	// ListUISessions returns up to q.Limit sessions ordered by id.
	// ErrInvalid when the limit is not positive.
	ListUISessions(ctx context.Context, q UISessionQuery) ([]UISession, error)
	// CreateUISession inserts one row; RevokedAt and RevokedBy are ignored.
	// ErrConflict when the id is taken. ErrInvalid for an empty id,
	// provider, subject or login, a hash that is not 32 bytes, or a zero
	// CreatedAt, LastSeenAt or ExpiresAt.
	CreateUISession(ctx context.Context, s UISession) error
	// RevokeUISession sets revoked_at and revoked_by if unset and reports
	// true; false when the row was already revoked (nothing changes);
	// ErrNotFound when it does not exist. ErrInvalid for a zero time or an
	// empty revoker.
	RevokeUISession(ctx context.Context, id string, at time.Time, by string) (bool, error)
	// TouchUISessions sets last_seen_at = max(last_seen_at, t) for each id:
	// it never moves backwards, and unknown ids are ignored.
	TouchUISessions(ctx context.Context, seen map[string]time.Time) error
	// PinUIIdentity binds login (already in lower case) of a provider to
	// subject, unless the login is pinned already, and returns the subject
	// of the pin that holds: subject itself, or the one pinned before. A
	// pin is never changed. ErrInvalid for an empty field or a zero time.
	PinUIIdentity(ctx context.Context, provider, login, subject string, at time.Time) (string, error)
	// UIIdentityPin returns the subject that login (already in lower case)
	// of a provider is pinned to, or ErrNotFound when it is not pinned. It
	// never pins.
	UIIdentityPin(ctx context.Context, provider, login string) (string, error)
}

// ValidateNewUISession checks the fields CreateUISession stores; engines
// call it before the insert. The error wraps ErrInvalid.
func ValidateNewUISession(s UISession) error {
	switch {
	case s.ID == "" || s.Provider == "" || s.Subject == "" || s.Login == "":
		return invalidSession(s.ID, "empty field")
	case len(s.Hash) != 32:
		return invalidSession(s.ID, "hash is not 32 bytes")
	case s.CreatedAt.IsZero() || s.LastSeenAt.IsZero() || s.ExpiresAt.IsZero():
		return invalidSession(s.ID, "zero time")
	}
	return nil
}

// ValidatePin checks the arguments of PinUIIdentity.
func ValidatePin(provider, login, subject string, at time.Time) error {
	if provider == "" || login == "" || subject == "" || at.IsZero() {
		return fmt.Errorf("store: ui identity pin %s/%s: empty field or zero time: %w", provider, login, ErrInvalid)
	}
	return nil
}

func invalidSession(id, why string) error {
	return fmt.Errorf("store: ui session %s: %s: %w", id, why, ErrInvalid)
}
