package store

import (
	"context"
	"fmt"
	"time"
)

// APIToken is the stored form of an API token (story #72, ADR 0030 T3). The
// store never holds the token itself: Hash is the 32-byte SHA-256 of ADR 0030
// T2, computed by internal/apitoken.
type APIToken struct {
	// ID is the token's ULID, the visible id inside the token (ADR 0030 T1).
	ID string
	// Name is the operator's label; unique among active (unrevoked) tokens.
	Name string
	// Scope is "read", "write" or "admin".
	Scope string
	// Hash is the 32-byte hash of the whole token.
	Hash      []byte
	CreatedAt time.Time
	// CreatedBy is the creating token's id, "bootstrap" or "local".
	CreatedBy string
	// ExpiresAt is nil when the token never expires.
	ExpiresAt *time.Time
	// LastUsedAt is nil until the first recorded use; it is coalesced to
	// about 5 minutes (ADR 0030 T6).
	LastUsedAt *time.Time
	// RevokedAt is nil while the token is active.
	RevokedAt *time.Time
	// RevokedBy is the revoker ("" while active).
	RevokedBy string
}

// TokenReader holds the reads of API tokens. On the store they each run
// outside a transaction.
type TokenReader interface {
	// APITokenByID returns the row with this id (revoked or not), or
	// ErrNotFound.
	APITokenByID(ctx context.Context, id string) (APIToken, error)
	// ListAPITokens returns up to limit tokens ordered by id, after afterID
	// ("" starts at the beginning); includeRevoked adds revoked rows.
	// ErrInvalid when limit is not positive.
	ListAPITokens(ctx context.Context, afterID string, limit int, includeRevoked bool) ([]APIToken, error)
	// CountAPITokens counts every row, revoked and expired included
	// (bootstrap, ADR 0030 T4).
	CountAPITokens(ctx context.Context) (int64, error)
	// CountActiveAdminTokens counts admin rows that are neither revoked nor
	// expired at now (revoked_at IS NULL AND (expires_at IS NULL OR
	// expires_at > now)): the start-up warning of ADR 0030 T4.
	CountActiveAdminTokens(ctx context.Context, now time.Time) (int64, error)
}

// TokenStore is what API token management needs from an engine (ADR 0030
// T3). Every write is a single statement: there is no token transaction and
// no config lock; concurrent writes are settled by the primary key and the
// unique index on active names.
type TokenStore interface {
	TokenReader
	// CreateAPIToken inserts one row; LastUsedAt, RevokedAt and RevokedBy
	// are ignored (a new token is unused and active). ErrConflict when the
	// id, or the name of an active token, is taken. ErrInvalid for an empty
	// id, name or creator, an unknown scope, a hash that is not 32 bytes or
	// a zero CreatedAt.
	CreateAPIToken(ctx context.Context, t APIToken) error
	// RevokeAPIToken sets revoked_at and revoked_by if unset and reports
	// true; false when the row was already revoked (nothing changes);
	// ErrNotFound when it does not exist. ErrInvalid for a zero time or an
	// empty revoker.
	RevokeAPIToken(ctx context.Context, id string, at time.Time, by string) (bool, error)
	// TouchAPITokens sets last_used_at = max(last_used_at, t) for each id
	// (ADR 0030 T6): it never moves backwards, and unknown ids are ignored.
	TouchAPITokens(ctx context.Context, used map[string]time.Time) error
}

// API token scopes as stored.
const (
	TokenScopeRead  = "read"
	TokenScopeWrite = "write"
	TokenScopeAdmin = "admin"
)

// ValidateNewAPIToken checks the fields CreateAPIToken stores; engines call
// it before the insert. The error wraps ErrInvalid.
func ValidateNewAPIToken(t APIToken) error {
	switch {
	case t.ID == "" || t.Name == "" || t.CreatedBy == "":
		return invalidToken(t.ID, "empty field")
	case t.Scope != TokenScopeRead && t.Scope != TokenScopeWrite && t.Scope != TokenScopeAdmin:
		return invalidToken(t.ID, "unknown scope "+t.Scope)
	case len(t.Hash) != 32:
		return invalidToken(t.ID, "hash is not 32 bytes")
	case t.CreatedAt.IsZero():
		return invalidToken(t.ID, "zero created_at")
	}
	return nil
}

func invalidToken(id, why string) error {
	return fmt.Errorf("store: api token %s: %s: %w", id, why, ErrInvalid)
}
