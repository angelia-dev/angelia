package webauth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/angelia-dev/angelia/internal/api"
	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/store"
)

// Store is what the sessions need of the store; [store.SessionStore] has it.
type Store interface {
	PinStore
	UISessionByID(ctx context.Context, id string) (store.UISession, error)
	CreateUISession(ctx context.Context, s store.UISession) error
	RevokeUISession(ctx context.Context, id string, at time.Time, by string) (bool, error)
	TouchUISessions(ctx context.Context, seen map[string]time.Time) error
}

// TouchInterval is the shortest time between two writes of one session's
// last_seen_at (ADR 0033 §5, as ADR 0030 T6 for tokens).
const TouchInterval = 5 * time.Minute

// FlushTick is how often `angelia serve` writes the recorded uses ([Run]).
const FlushTick = time.Minute

// IdleSlack is added to the idle timeout: a use on another replica reaches
// the store only after up to TouchInterval plus FlushTick, so a session used
// there is not ended early here. A session therefore ends between
// idle_timeout and idle_timeout + IdleSlack after its last use.
const IdleSlack = TouchInterval + FlushTick

// maxUserAgent bounds the stored User-Agent (the Session schema's 256).
const maxUserAgent = 256

// AuthError is the error of a refused session. It wraps
// [api.ErrUnauthenticated] and never holds the cookie or its hash.
type AuthError struct {
	// Reason is one of api.AuthReason*. A session whose identity no
	// longer matches ui.access is "revoked": the access list withdrew it.
	Reason string
	// ID is the session id for revoked and expired (the hash matched).
	ID string
}

func (e *AuthError) Error() string {
	if e.ID != "" {
		return fmt.Sprintf("webauth: %s session %s", e.Reason, e.ID)
	}
	return "webauth: " + e.Reason + " session"
}

// FailureReason implements [api.AuthFailure].
func (e *AuthError) FailureReason() (reason, id string) { return e.Reason, e.ID }

// Unwrap makes every AuthError an [api.ErrUnauthenticated].
func (e *AuthError) Unwrap() error { return api.ErrUnauthenticated }

var _ api.AuthFailure = (*AuthError)(nil)

// dummyHash is compared with the hash of a cookie whose id has no row, so an
// unknown id takes the same CPU path as a known one.
var dummyHash = HashCookie(CookiePrefix + "dummy")

// Sessions creates, authenticates and ends the admin UI's browser sessions.
// It is the [api.SessionAuthenticator] of `angelia serve`.
type Sessions struct {
	store  Store
	access *accessList
	idle   time.Duration
	max    time.Duration
	now    func() time.Time
	log    *slog.Logger

	mu sync.Mutex
	// pending is the latest successful use of each session not yet
	// written; written is when each session's use was last written.
	pending map[string]time.Time
	written map[string]time.Time
}

var _ api.SessionAuthenticator = (*Sessions)(nil)

// SessionsOption configures Sessions.
type SessionsOption func(*Sessions)

// WithClock sets the clock (tests).
func WithClock(now func() time.Time) SessionsOption { return func(s *Sessions) { s.now = now } }

// NewSessions returns the sessions of a UI configuration over st. A nil log
// discards.
func NewSessions(st Store, ui config.UI, log *slog.Logger, opts ...SessionsOption) *Sessions {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Sessions{
		store: st, idle: ui.Session.IdleTimeout, max: ui.Session.MaxLifetime, now: time.Now, log: log,
		pending: map[string]time.Time{}, written: map[string]time.Time{},
	}
	for _, o := range opts {
		o(s)
	}
	s.access = newAccessList(ui.Access, st, s.now, log)
	return s
}

// IdleTimeout implements [api.SessionAuthenticator]: the configured idle
// timeout plus [IdleSlack], how long a session actually lives unused.
func (s *Sessions) IdleTimeout() time.Duration { return s.idle + IdleSlack }

// lookup reads the session of a cookie value and checks its hash. It
// returns an *AuthError for a malformed value, an unknown id or a wrong
// secret; any other error is a store failure.
func (s *Sessions) lookup(ctx context.Context, cookie string) (store.UISession, error) {
	id, err := ParseCookie(cookie)
	if err != nil {
		return store.UISession{}, &AuthError{Reason: api.AuthReasonMalformed}
	}
	h := HashCookie(cookie)
	row, err := s.store.UISessionByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		subtle.ConstantTimeCompare(h[:], dummyHash[:])
		return store.UISession{}, &AuthError{Reason: api.AuthReasonUnknown}
	}
	if err != nil {
		return store.UISession{}, fmt.Errorf("webauth: read session %s: %w", id, err)
	}
	if subtle.ConstantTimeCompare(h[:], row.Hash) != 1 {
		return store.UISession{}, &AuthError{Reason: api.AuthReasonUnknown}
	}
	return row, nil
}

// AuthenticateSession implements [api.SessionAuthenticator]. It reads the
// row on every call (no cache), so a revocation applies at the next request
// on every replica, evaluates the role from ui.access now, and records the
// use (last_seen_at).
func (s *Sessions) AuthenticateSession(ctx context.Context, cookie string) (api.Principal, error) {
	return s.authenticate(ctx, cookie, true)
}

// CheckSession implements [api.SessionAuthenticator]: AuthenticateSession
// without recording a use, for the re-check of an open event stream, which
// must not keep an otherwise idle session alive.
func (s *Sessions) CheckSession(ctx context.Context, cookie string) (api.Principal, error) {
	return s.authenticate(ctx, cookie, false)
}

func (s *Sessions) authenticate(ctx context.Context, cookie string, use bool) (api.Principal, error) {
	row, err := s.lookup(ctx, cookie)
	if err != nil {
		return api.Principal{}, err
	}
	now := s.now()
	if row.RevokedAt != nil {
		return api.Principal{}, &AuthError{Reason: api.AuthReasonRevoked, ID: row.ID}
	}
	if s.ended(row, now) {
		return api.Principal{}, &AuthError{Reason: api.AuthReasonExpired, ID: row.ID}
	}
	m, err := s.access.role(ctx, row.Provider, row.Subject, row.Login)
	if err != nil {
		return api.Principal{}, err
	}
	scope := RoleScope(m.role)
	if scope == api.ScopeNone {
		// Removed from ui.access since the sign-in: the list withdrew it.
		return api.Principal{}, &AuthError{Reason: api.AuthReasonRevoked, ID: row.ID}
	}
	if use {
		s.recordUse(row.ID, now)
	}
	return api.Principal{
		ID: row.Provider + ":" + row.Subject, Name: row.Login, Scope: scope,
		Session: &api.SessionInfo{
			ID: row.ID, Provider: row.Provider, Subject: row.Subject, Login: row.Login,
			Name: row.Name, AvatarURL: row.AvatarURL, Role: m.role,
		},
	}, nil
}

// ended reports whether a session is past its absolute end (its stored
// expires_at, or its creation plus the maximum lifetime configured now,
// whichever is first) or idle for the idle timeout plus [IdleSlack]. The
// idle check uses the latest use this process recorded, which the store may
// not hold yet.
func (s *Sessions) ended(row store.UISession, now time.Time) bool {
	end := row.ExpiresAt
	if e := row.CreatedAt.Add(s.max); e.Before(end) {
		end = e
	}
	if !now.Before(end) {
		return true
	}
	seen := row.LastSeenAt
	s.mu.Lock()
	if p := s.pending[row.ID]; p.After(seen) {
		seen = p
	}
	s.mu.Unlock()
	return now.Sub(seen) >= s.idle+IdleSlack
}

// signIn creates a session for an identity that ui.access lets in. It
// returns the cookie value and the session's end, or the result of a
// refusal (ResultNotAuthorised, ResultIdentityMismatch) with the access
// match; an error is a store failure.
func (s *Sessions) signIn(ctx context.Context, id Identity, userAgent string) (cookie string, row store.UISession, m match, err error) {
	m, err = s.access.role(ctx, id.Provider, id.Subject, id.Login)
	if err != nil || RoleScope(m.role) == api.ScopeNone {
		return "", store.UISession{}, m, err
	}
	now := s.now()
	sid, cookie := NewCookie(now)
	h := HashCookie(cookie)
	row = store.UISession{
		ID: sid, Hash: h[:], Provider: id.Provider, Subject: id.Subject, Login: id.Login, Name: id.Name,
		AvatarURL: id.AvatarURL, UserAgent: cutUserAgent(userAgent), CreatedAt: now, LastSeenAt: now,
		ExpiresAt: now.Add(s.max),
	}
	if err := s.store.CreateUISession(ctx, row); err != nil {
		return "", store.UISession{}, m, fmt.Errorf("webauth: create session: %w", err)
	}
	return cookie, row, m, nil
}

// signOut revokes the session of a cookie value, by its own principal. It
// returns the session, or false when the value names no active session; an
// error is a store failure.
func (s *Sessions) signOut(ctx context.Context, cookie string) (store.UISession, bool, error) {
	return s.revokeCookie(ctx, cookie, "")
}

// revokeCookie revokes the session of a cookie value by the principal by,
// or by the session's own principal when by is "". Signing in again uses it
// to end the session the browser held before. It returns the session, or
// false when the value names no active session; an error is a store
// failure.
func (s *Sessions) revokeCookie(ctx context.Context, cookie, by string) (store.UISession, bool, error) {
	row, err := s.lookup(ctx, cookie)
	var ae *AuthError
	switch {
	case errors.As(err, &ae):
		return store.UISession{}, false, nil
	case err != nil:
		return store.UISession{}, false, err
	}
	if by == "" {
		by = row.Provider + ":" + row.Subject
	}
	changed, err := s.store.RevokeUISession(ctx, row.ID, s.now(), by)
	if err != nil {
		return store.UISession{}, false, fmt.Errorf("webauth: revoke session %s: %w", row.ID, err)
	}
	s.mu.Lock()
	delete(s.pending, row.ID)
	s.mu.Unlock()
	return row, changed, nil
}

// cutUserAgent keeps at most maxUserAgent characters of valid UTF-8 without
// control characters.
func cutUserAgent(ua string) string {
	out := make([]rune, 0, min(len(ua), maxUserAgent))
	for _, r := range ua {
		if len(out) == maxUserAgent {
			break
		}
		if r < 0x20 || r == 0x7f || r == '�' {
			continue
		}
		out = append(out, r)
	}
	return string(out)
}

// recordUse remembers a successful use, keyed by session id.
func (s *Sessions) recordUse(id string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if at.After(s.pending[id]) {
		s.pending[id] = at
	}
}

// Flush writes the recorded uses of the sessions whose last use was written
// at least [TouchInterval] ago, or never; with all, every recorded use
// (graceful shutdown). The store write runs outside the lock. On an error
// the uses stay recorded for the next flush.
func (s *Sessions) Flush(ctx context.Context, all bool) error {
	now := s.now()
	s.mu.Lock()
	batch := map[string]time.Time{}
	for id, at := range s.pending {
		if all || now.Sub(s.written[id]) >= TouchInterval {
			batch[id] = at
		}
	}
	for id, w := range s.written {
		if _, ok := s.pending[id]; !ok && now.Sub(w) >= TouchInterval {
			delete(s.written, id)
		}
	}
	s.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	if err := s.store.TouchUISessions(ctx, batch); err != nil {
		return fmt.Errorf("webauth: record session use: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, at := range batch {
		s.written[id] = now
		if !s.pending[id].After(at) {
			delete(s.pending, id)
		}
	}
	return nil
}

// Run flushes recorded uses every tick until ctx ends, then once more with
// every recorded use, on a context of its own bounded by shutdownTimeout. A
// failed flush is logged and retried at the next tick.
func (s *Sessions) Run(ctx context.Context, tick, shutdownTimeout time.Duration) error {
	if tick <= 0 {
		return fmt.Errorf("webauth: flush tick %s is not positive", tick)
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if err := s.Flush(ctx, false); err != nil && ctx.Err() == nil {
				s.log.WarnContext(ctx, "ui session last use not recorded", "err", err)
			}
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
			defer cancel()
			if err := s.Flush(fctx, true); err != nil {
				s.log.WarnContext(fctx, "ui session last use not recorded at shutdown", "err", err)
			}
			return nil
		}
	}
}

// TokenLister lists the API tokens ([store.TokenReader] has it).
type TokenLister interface {
	ListAPITokens(ctx context.Context, afterID string, limit int, includeRevoked bool) ([]store.APIToken, error)
}

// OrphanTokens returns the ids of the active API tokens that a signed-in
// user created (created_by "<provider>:<subject>") whose identity ui.access
// no longer lists: removing a person from ui.access ends their sessions but
// not the tokens they created.
func (s *Sessions) OrphanTokens(ctx context.Context, tokens TokenLister) ([]string, error) {
	const page = 500
	var out []string
	now := s.now()
	for after := ""; ; {
		ts, err := tokens.ListAPITokens(ctx, after, page, false)
		if err != nil {
			return nil, fmt.Errorf("webauth: list api tokens: %w", err)
		}
		for _, t := range ts {
			provider, subject, ok := strings.Cut(t.CreatedBy, ":")
			if !ok || provider != GitHubName || subject == "" || (t.ExpiresAt != nil && !now.Before(*t.ExpiresAt)) {
				continue
			}
			listed, err := s.access.listsSubject(ctx, provider, subject)
			if err != nil {
				return nil, err
			}
			if !listed {
				out = append(out, t.ID)
			}
		}
		if len(ts) < page {
			return out, nil
		}
		after = ts[len(ts)-1].ID
	}
}

// WarnOrphanTokens logs one warning that lists the ids of the
// [Sessions.OrphanTokens], if any (the api role, at start). It logs ids
// only, never a token or its hash. A failed read is logged too.
func (s *Sessions) WarnOrphanTokens(ctx context.Context, tokens TokenLister) {
	ids, err := s.OrphanTokens(ctx, tokens)
	switch {
	case err != nil:
		s.log.WarnContext(ctx, "api tokens created by ui users not checked against ui.access", "err", err)
	case len(ids) > 0:
		s.log.WarnContext(ctx, "active api tokens were created by ui users that ui.access no longer lists; revoke them if those people left (angelia token revoke)",
			"token_ids", ids)
	}
}
