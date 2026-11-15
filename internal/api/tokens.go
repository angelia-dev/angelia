package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/angelia-dev/angelia/internal/apitoken/format"
	"github.com/angelia-dev/angelia/internal/store"
)

// API tokens: /v1/tokens (story #72, ADR 0030). Every operation needs the
// admin scope (the specification says so). The token itself appears in one
// place only, the 201 body of createToken: never in another response, a
// log, an audit record or a problem detail. The store keeps its hash.

// tokenJSON is the Token schema: metadata only.
type tokenJSON struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Scope      string     `json:"scope"`
	CreatedAt  timestamp  `json:"created_at"`
	CreatedBy  string     `json:"created_by"`
	ExpiresAt  *timestamp `json:"expires_at"`
	LastUsedAt *timestamp `json:"last_used_at"`
	RevokedAt  *timestamp `json:"revoked_at"`
	RevokedBy  *string    `json:"revoked_by"`
}

// tokenCreatedJSON is the TokenCreated schema: the Token plus the token.
type tokenCreatedJSON struct {
	tokenJSON
	Token string `json:"token"`
}

func tsPtr(t *time.Time) *timestamp {
	if t == nil {
		return nil
	}
	return ts(*t)
}

func tokenJSONOf(t store.APIToken) tokenJSON {
	out := tokenJSON{
		ID: t.ID, Name: t.Name, Scope: t.Scope, CreatedAt: timestamp(t.CreatedAt), CreatedBy: t.CreatedBy,
		ExpiresAt: tsPtr(t.ExpiresAt), LastUsedAt: tsPtr(t.LastUsedAt), RevokedAt: tsPtr(t.RevokedAt),
	}
	if t.RevokedAt != nil {
		by := t.RevokedBy
		out.RevokedBy = &by
	}
	return out
}

// ListTokens implements StrictServerInterface.
func (s *Server) ListTokens(ctx context.Context, req ListTokensRequestObject) (ListTokensResponseObject, error) {
	cur, p := decodeCursor(req.Params.Cursor, cursorTokens)
	if p != nil {
		return nil, p
	}
	limit := pageLimit(req.Params.Limit)
	all := req.Params.IncludeRevoked != nil && *req.Params.IncludeRevoked
	rows, err := s.opts.Store.ListAPITokens(ctx, cur.I, limit+1, all)
	if err != nil {
		return nil, err
	}
	page := pageJSON[tokenJSON]{Items: make([]tokenJSON, 0, min(len(rows), limit))}
	if len(rows) > limit {
		rows = rows[:limit]
		page.NextCursor = encodeCursor(pageCursor{K: cursorTokens, I: rows[limit-1].ID})
	}
	for _, t := range rows {
		page.Items = append(page.Items, tokenJSONOf(t))
	}
	return jsonOK(page), nil
}

// GetToken implements StrictServerInterface.
func (s *Server) GetToken(ctx context.Context, req GetTokenRequestObject) (GetTokenResponseObject, error) {
	t, err := s.opts.Store.APITokenByID(ctx, req.Id)
	if err != nil {
		return nil, storeProblem(err, "token", req.Id)
	}
	return jsonOK(tokenJSONOf(t)), nil
}

// tokenBody is a parsed createToken body.
type tokenBody struct {
	Name      string  `json:"name"`
	Scope     string  `json:"scope"`
	ExpiresAt *string `json:"expires_at"`
	expires   *time.Time
}

// parseTokenBody applies the rule the schema cannot express: expires_at,
// when given, is in the future. The name pattern and the scope enum are the
// schema's.
func parseTokenBody(raw []byte, now time.Time) (tokenBody, []fieldError) {
	var b tokenBody
	_ = json.Unmarshal(raw, &b) // validated against the schema before, or reported with its errors
	if b.ExpiresAt == nil {
		return b, nil
	}
	t, err := time.Parse(time.RFC3339Nano, *b.ExpiresAt)
	if err != nil {
		return b, nil // the schema reports the format
	}
	t = t.UTC().Truncate(time.Millisecond)
	if !t.After(now) {
		return b, []fieldError{{Pointer: "/expires_at", Detail: "must be in the future"}}
	}
	b.expires = &t
	return b, nil
}

// errNameTaken is the 422 of a name that an active token already has.
func errNameTaken() *ProblemDetail {
	return validationProblem("token", []fieldError{{Pointer: "/name", Detail: "an active token already has this name"}})
}

// CreateToken implements StrictServerInterface. The new token's creator is
// the caller's token id.
func (s *Server) CreateToken(ctx context.Context, _ CreateTokenRequestObject) (CreateTokenResponseObject, error) {
	now := s.now()
	b, errs := parseTokenBody(stateFrom(ctx).body, now)
	if len(errs) > 0 {
		return nil, validationProblem("token", errs)
	}
	id, token := format.Generate(now)
	hash := format.Hash(token)
	row := store.APIToken{
		ID: id, Name: b.Name, Scope: b.Scope, Hash: hash[:], CreatedAt: now,
		CreatedBy: principalFrom(ctx).ID, ExpiresAt: b.expires,
	}
	err := s.opts.Store.CreateAPIToken(ctx, row)
	switch {
	case errors.Is(err, store.ErrConflict):
		// The id is a fresh ULID with 80 random bits: the clash is the
		// name of an active token.
		return nil, errNameTaken()
	case err != nil:
		return nil, err
	}
	attrs := []slog.Attr{slog.String("token_id", id), slog.String("token_name", row.Name), slog.String("token_scope", row.Scope)}
	if row.ExpiresAt != nil {
		attrs = append(attrs, slog.Time("expires_at", *row.ExpiresAt))
	}
	s.audit(ctx, "token.create", attrs...)
	h := http.Header{}
	h.Set("Location", location("tokens", id))
	// The body carries the token: no cache may keep it.
	h.Set("Cache-Control", "no-store")
	return response{status: http.StatusCreated, header: h, body: tokenCreatedJSON{tokenJSON: tokenJSONOf(row), Token: token}}, nil
}

// RevokeToken implements StrictServerInterface. Revoking a revoked token is
// 204 too and changes nothing; the revoker is the caller's token id.
func (s *Server) RevokeToken(ctx context.Context, req RevokeTokenRequestObject) (RevokeTokenResponseObject, error) {
	t, err := s.opts.Store.APITokenByID(ctx, req.Id)
	if err != nil {
		return nil, storeProblem(err, "token", req.Id)
	}
	changed, err := s.opts.Store.RevokeAPIToken(ctx, req.Id, s.now(), principalFrom(ctx).ID)
	if err != nil {
		return nil, storeProblem(err, "token", req.Id)
	}
	if changed {
		s.audit(ctx, "token.revoke", slog.String("token_id", t.ID), slog.String("token_name", t.Name), slog.String("token_scope", t.Scope))
	}
	return response{status: http.StatusNoContent}, nil
}

// Visit methods of the token operations.

func (r response) VisitListTokensResponse(w http.ResponseWriter) error  { return r.write(w) }
func (r response) VisitGetTokenResponse(w http.ResponseWriter) error    { return r.write(w) }
func (r response) VisitCreateTokenResponse(w http.ResponseWriter) error { return r.write(w) }
func (r response) VisitRevokeTokenResponse(w http.ResponseWriter) error { return r.write(w) }
