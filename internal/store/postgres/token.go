package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// API tokens ([store.TokenStore], story #72, ADR 0030 T3). Every write is one
// autocommit statement that takes no config lock (ADR 0030 T6), so a touch
// never waits behind a config write.

var _ store.TokenStore = (*Store)(nil)

// APITokenByID implements [store.TokenReader].
func (s *Store) APITokenByID(ctx context.Context, id string) (store.APIToken, error) {
	r, err := s.q.GetAPIToken(ctx, id)
	if err != nil {
		return store.APIToken{}, wrap(err, "api token", id)
	}
	return apiTokenFromRow(r), nil
}

// ListAPITokens implements [store.TokenReader].
func (s *Store) ListAPITokens(ctx context.Context, afterID string, limit int, includeRevoked bool) ([]store.APIToken, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list api tokens: limit %d: %w", limit, store.ErrInvalid)
	}
	var (
		rows []ApiToken
		err  error
	)
	if includeRevoked {
		rows, err = s.q.ListAPITokensPage(ctx, ListAPITokensPageParams{AfterID: afterID, MaxRows: maxRows(limit)})
	} else {
		rows, err = s.q.ListActiveAPITokensPage(ctx, ListActiveAPITokensPageParams{AfterID: afterID, MaxRows: maxRows(limit)})
	}
	if err != nil {
		return nil, wrap(err, "list api tokens", afterID)
	}
	out := make([]store.APIToken, len(rows))
	for i, r := range rows {
		out[i] = apiTokenFromRow(r)
	}
	return out, nil
}

// CountAPITokens implements [store.TokenReader].
func (s *Store) CountAPITokens(ctx context.Context) (int64, error) {
	n, err := s.q.CountAPITokens(ctx)
	return n, wrap(err, "count api tokens", "")
}

// CountActiveAdminTokens implements [store.TokenReader].
func (s *Store) CountActiveAdminTokens(ctx context.Context, now time.Time) (int64, error) {
	n, err := s.q.CountActiveAdminTokens(ctx, millis(now))
	return n, wrap(err, "count active admin tokens", "")
}

// CreateAPIToken implements [store.TokenStore].
func (s *Store) CreateAPIToken(ctx context.Context, t store.APIToken) error {
	if err := store.ValidateNewAPIToken(t); err != nil {
		return err
	}
	return wrap(s.q.CreateAPIToken(ctx, CreateAPITokenParams{
		ID: t.ID, Name: t.Name, Scope: t.Scope, Hash: t.Hash, CreatedAt: millis(t.CreatedAt),
		CreatedBy: t.CreatedBy, ExpiresAt: ptrNullMillis(t.ExpiresAt),
	}), "create api token", t.ID)
}

// RevokeAPIToken implements [store.TokenStore].
func (s *Store) RevokeAPIToken(ctx context.Context, id string, at time.Time, by string) (bool, error) {
	if at.IsZero() || by == "" {
		return false, fmt.Errorf("store: revoke api token %s: zero time or empty revoker: %w", id, store.ErrInvalid)
	}
	n, err := s.q.RevokeAPIToken(ctx, RevokeAPITokenParams{RevokedAt: millis(at), RevokedBy: by, ID: id})
	if err != nil {
		return false, wrap(err, "revoke api token", id)
	}
	if n == 1 {
		return true, nil
	}
	// Nothing changed: already revoked, or no such row.
	if _, err := s.q.GetAPIToken(ctx, id); err != nil {
		return false, wrap(err, "revoke api token", id)
	}
	return false, nil
}

// TouchAPITokens implements [store.TokenStore]: one UPDATE per id, in id
// order.
func (s *Store) TouchAPITokens(ctx context.Context, used map[string]time.Time) error {
	var errs []error
	for _, id := range slices.Sorted(maps.Keys(used)) {
		if err := s.q.TouchAPIToken(ctx, TouchAPITokenParams{UsedAt: millis(used[id]), ID: id}); err != nil {
			errs = append(errs, wrap(err, "touch api token", id))
		}
	}
	return errors.Join(errs...)
}

func apiTokenFromRow(r ApiToken) store.APIToken {
	return store.APIToken{
		ID: r.ID, Name: r.Name, Scope: r.Scope, Hash: r.Hash,
		CreatedAt: fromMillis(r.CreatedAt), CreatedBy: r.CreatedBy,
		ExpiresAt: ptrFromNullMillis(r.ExpiresAt), LastUsedAt: ptrFromNullMillis(r.LastUsedAt),
		RevokedAt: ptrFromNullMillis(r.RevokedAt), RevokedBy: r.RevokedBy.String,
	}
}

func ptrNullMillis(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: millis(*t), Valid: true}
}

func ptrFromNullMillis(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := fromMillis(n.Int64)
	return &t
}
