package sqlite

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// Browser sessions of the admin UI and identity pins ([store.SessionStore],
// story #262, ADR 0033 §5). Every write is one autocommit statement.

var _ store.SessionStore = (*Store)(nil)

// UISessionByID implements [store.SessionStore].
func (s *Store) UISessionByID(ctx context.Context, id string) (store.UISession, error) {
	r, err := s.q.GetUISession(ctx, id)
	if err != nil {
		return store.UISession{}, wrap(err, "ui session", id)
	}
	return uiSessionFromRow(r), nil
}

// ListUISessions implements [store.SessionStore].
func (s *Store) ListUISessions(ctx context.Context, q store.UISessionQuery) ([]store.UISession, error) {
	if q.Limit <= 0 {
		return nil, fmt.Errorf("store: list ui sessions: limit %d: %w", q.Limit, store.ErrInvalid)
	}
	var (
		rows []UiSession
		err  error
	)
	if q.IncludeEnded {
		rows, err = s.q.ListUISessionsPage(ctx, ListUISessionsPageParams{AfterID: q.AfterID, MaxRows: int64(q.Limit)})
	} else {
		rows, err = s.q.ListActiveUISessionsPage(ctx, ListActiveUISessionsPageParams{
			AfterID: q.AfterID, Now: millis(q.Now), IdleSince: millis(q.IdleSince), MaxRows: int64(q.Limit),
		})
	}
	if err != nil {
		return nil, wrap(err, "list ui sessions", q.AfterID)
	}
	out := make([]store.UISession, len(rows))
	for i, r := range rows {
		out[i] = uiSessionFromRow(r)
	}
	return out, nil
}

// CreateUISession implements [store.SessionStore].
func (s *Store) CreateUISession(ctx context.Context, u store.UISession) error {
	if err := store.ValidateNewUISession(u); err != nil {
		return err
	}
	return wrap(s.q.CreateUISession(ctx, CreateUISessionParams{
		ID: u.ID, Hash: u.Hash, Provider: u.Provider, Subject: u.Subject, Login: u.Login, Name: u.Name,
		AvatarUrl: u.AvatarURL, UserAgent: u.UserAgent, CreatedAt: millis(u.CreatedAt),
		LastSeenAt: millis(u.LastSeenAt), ExpiresAt: millis(u.ExpiresAt),
	}), "create ui session", u.ID)
}

// RevokeUISession implements [store.SessionStore].
func (s *Store) RevokeUISession(ctx context.Context, id string, at time.Time, by string) (bool, error) {
	if at.IsZero() || by == "" {
		return false, fmt.Errorf("store: revoke ui session %s: zero time or empty revoker: %w", id, store.ErrInvalid)
	}
	n, err := s.q.RevokeUISession(ctx, RevokeUISessionParams{RevokedAt: millis(at), RevokedBy: by, ID: id})
	if err != nil {
		return false, wrap(err, "revoke ui session", id)
	}
	if n == 1 {
		return true, nil
	}
	// Nothing changed: already revoked, or no such row.
	if _, err := s.q.GetUISession(ctx, id); err != nil {
		return false, wrap(err, "revoke ui session", id)
	}
	return false, nil
}

// TouchUISessions implements [store.SessionStore]: one UPDATE per id, in id
// order.
func (s *Store) TouchUISessions(ctx context.Context, seen map[string]time.Time) error {
	var errs []error
	for _, id := range slices.Sorted(maps.Keys(seen)) {
		if err := s.q.TouchUISession(ctx, TouchUISessionParams{SeenAt: millis(seen[id]), ID: id}); err != nil {
			errs = append(errs, wrap(err, "touch ui session", id))
		}
	}
	return errors.Join(errs...)
}

// PinUIIdentity implements [store.SessionStore]: an insert that keeps an
// existing pin, then a read of the pin that holds.
func (s *Store) PinUIIdentity(ctx context.Context, provider, login, subject string, at time.Time) (string, error) {
	if err := store.ValidatePin(provider, login, subject, at); err != nil {
		return "", err
	}
	key := provider + "/" + login
	if err := s.q.InsertUIIdentityPin(ctx, InsertUIIdentityPinParams{Provider: provider, Login: login, Subject: subject, PinnedAt: millis(at)}); err != nil {
		return "", wrap(err, "pin ui identity", key)
	}
	pinned, err := s.q.GetUIIdentityPin(ctx, GetUIIdentityPinParams{Provider: provider, Login: login})
	if err != nil {
		return "", wrap(err, "read ui identity pin", key)
	}
	return pinned, nil
}

// UIIdentityPin implements [store.SessionStore].
func (s *Store) UIIdentityPin(ctx context.Context, provider, login string) (string, error) {
	pinned, err := s.q.GetUIIdentityPin(ctx, GetUIIdentityPinParams{Provider: provider, Login: login})
	if err != nil {
		return "", wrap(err, "read ui identity pin", provider+"/"+login)
	}
	return pinned, nil
}

func uiSessionFromRow(r UiSession) store.UISession {
	return store.UISession{
		ID: r.ID, Hash: r.Hash, Provider: r.Provider, Subject: r.Subject, Login: r.Login, Name: r.Name,
		AvatarURL: r.AvatarUrl, UserAgent: r.UserAgent, CreatedAt: fromMillis(r.CreatedAt),
		LastSeenAt: fromMillis(r.LastSeenAt), ExpiresAt: fromMillis(r.ExpiresAt),
		RevokedAt: ptrFromNullMillis(r.RevokedAt), RevokedBy: r.RevokedBy.String,
	}
}
