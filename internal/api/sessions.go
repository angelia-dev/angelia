package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/angelia-dev/angelia/internal/store"
)

// The caller and the admin UI's browser sessions: /v1/me and /v1/sessions
// (story #262, ADR 0033 §1, spec 0.8.0). No response carries a cookie or a
// hash.

// sessionUserJSON is the SessionUser schema.
type sessionUserJSON struct {
	Provider  string  `json:"provider"`
	Subject   string  `json:"subject"`
	Login     string  `json:"login"`
	Name      *string `json:"name"`
	AvatarURL *string `json:"avatar_url"`
}

// meJSON is the Me schema.
type meJSON struct {
	PrincipalID string           `json:"principal_id"`
	Name        string           `json:"name"`
	Scope       string           `json:"scope"`
	Kind        string           `json:"kind"`
	Role        *string          `json:"role"`
	User        *sessionUserJSON `json:"user"`
}

// sessionJSON is the Session schema.
type sessionJSON struct {
	ID         string          `json:"id"`
	User       sessionUserJSON `json:"user"`
	UserAgent  *string         `json:"user_agent"`
	CreatedAt  timestamp       `json:"created_at"`
	LastSeenAt timestamp       `json:"last_seen_at"`
	ExpiresAt  timestamp       `json:"expires_at"`
	RevokedAt  *timestamp      `json:"revoked_at"`
	RevokedBy  *string         `json:"revoked_by"`
}

// orNull is nil for "", else a pointer to s.
func orNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// The kinds of principal of the Me schema.
const (
	kindToken   = "token"
	kindSession = "session"
)

// GetMe implements StrictServerInterface.
func (s *Server) GetMe(ctx context.Context, _ GetMeRequestObject) (GetMeResponseObject, error) {
	p := principalFrom(ctx)
	out := meJSON{PrincipalID: p.ID, Name: p.Name, Scope: p.Scope.String(), Kind: kindToken}
	if si := p.Session; si != nil {
		out.Kind = kindSession
		out.Role = orNull(si.Role)
		out.User = &sessionUserJSON{
			Provider: si.Provider, Subject: si.Subject, Login: si.Login,
			Name: orNull(si.Name), AvatarURL: orNull(si.AvatarURL),
		}
	}
	return jsonOK(out), nil
}

func sessionJSONOf(u store.UISession) sessionJSON {
	out := sessionJSON{
		ID: u.ID,
		User: sessionUserJSON{
			Provider: u.Provider, Subject: u.Subject, Login: u.Login,
			Name: orNull(u.Name), AvatarURL: orNull(u.AvatarURL),
		},
		UserAgent: orNull(u.UserAgent), CreatedAt: timestamp(u.CreatedAt), LastSeenAt: timestamp(u.LastSeenAt),
		ExpiresAt: timestamp(u.ExpiresAt), RevokedAt: tsPtr(u.RevokedAt),
	}
	if u.RevokedAt != nil {
		out.RevokedBy = orNull(u.RevokedBy)
	}
	return out
}

// ListSessions implements StrictServerInterface. Without include_ended, a
// session is listed while it is not revoked, not past expires_at and used
// within the idle timeout of the session authenticator (none without one).
func (s *Server) ListSessions(ctx context.Context, req ListSessionsRequestObject) (ListSessionsResponseObject, error) {
	cur, p := decodeCursor(req.Params.Cursor, cursorSessions)
	if p != nil {
		return nil, p
	}
	limit := pageLimit(req.Params.Limit)
	now := s.now()
	q := store.UISessionQuery{
		AfterID: cur.I, Limit: limit + 1, Now: now,
		IncludeEnded: req.Params.IncludeEnded != nil && *req.Params.IncludeEnded,
	}
	if s.opts.Sessions != nil {
		q.IdleSince = now.Add(-s.opts.Sessions.IdleTimeout())
	}
	rows, err := s.opts.Store.ListUISessions(ctx, q)
	if err != nil {
		return nil, err
	}
	page := pageJSON[sessionJSON]{Items: make([]sessionJSON, 0, min(len(rows), limit))}
	if len(rows) > limit {
		rows = rows[:limit]
		page.NextCursor = encodeCursor(pageCursor{K: cursorSessions, I: rows[limit-1].ID})
	}
	for _, u := range rows {
		page.Items = append(page.Items, sessionJSONOf(u))
	}
	return jsonOK(page), nil
}

// RevokeSession implements StrictServerInterface. Revoking a revoked
// session is 204 too and changes nothing; the revoker is the caller's
// principal id.
func (s *Server) RevokeSession(ctx context.Context, req RevokeSessionRequestObject) (RevokeSessionResponseObject, error) {
	u, err := s.opts.Store.UISessionByID(ctx, req.Id)
	if err != nil {
		return nil, storeProblem(err, "session", req.Id)
	}
	changed, err := s.opts.Store.RevokeUISession(ctx, req.Id, s.now(), principalFrom(ctx).ID)
	if err != nil {
		return nil, storeProblem(err, "session", req.Id)
	}
	if changed {
		s.audit(ctx, "session.revoke", slog.String("session_id", u.ID),
			slog.String("session_principal", u.Provider+":"+u.Subject), slog.String("session_login", u.Login))
	}
	return response{status: http.StatusNoContent}, nil
}

// Visit methods of the caller and session operations.

func (r response) VisitGetMeResponse(w http.ResponseWriter) error         { return r.write(w) }
func (r response) VisitListSessionsResponse(w http.ResponseWriter) error  { return r.write(w) }
func (r response) VisitRevokeSessionResponse(w http.ResponseWriter) error { return r.write(w) }
