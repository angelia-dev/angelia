package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// Scope is the scope of an API token. Scopes are ordered: a token may call
// every operation whose scope is at or below its own (spec docs/api.md
// decision 22).
type Scope int

// The scopes, in order. ScopeNone is no scope at all.
const (
	ScopeNone Scope = iota
	ScopeRead
	ScopeWrite
	ScopeAdmin
)

func (s Scope) String() string {
	switch s {
	case ScopeRead:
		return "read"
	case ScopeWrite:
		return "write"
	case ScopeAdmin:
		return "admin"
	}
	return "none"
}

// ParseScope reads "read", "write" or "admin".
func ParseScope(s string) (Scope, bool) {
	switch s {
	case "read":
		return ScopeRead, true
	case "write":
		return ScopeWrite, true
	case "admin":
		return ScopeAdmin, true
	}
	return ScopeNone, false
}

// Principal is who a token belongs to.
type Principal struct {
	// ID names the token in logs and the audit trail. It must not be the
	// token or derived from it in a reversible way.
	ID    string
	Scope Scope
}

// ErrUnauthenticated is returned (wrapped) by an Authenticator for a token it
// does not accept: unknown, revoked or malformed.
var ErrUnauthenticated = errors.New("api: unauthenticated")

// Authenticator resolves a bearer token to its principal. Token storage,
// hashing and revocation belong to the API tokens story (#72); an
// implementation compares secrets in constant time (crypto/subtle). The
// server calls it on every request and again at least every 60 seconds on an
// open event stream, so a revoked token stops working within that time.
type Authenticator interface {
	// Authenticate returns the token's principal, or an error wrapping
	// ErrUnauthenticated. Any other error is a server failure (500).
	Authenticate(ctx context.Context, token string) (Principal, error)
}

// DenyAll is the Authenticator `angelia serve` uses until API tokens (#72)
// exist: it accepts no token, so every API request gets 401.
type DenyAll struct{}

// Authenticate implements Authenticator.
func (DenyAll) Authenticate(context.Context, string) (Principal, error) {
	return Principal{}, ErrUnauthenticated
}

// bearerToken reads "Authorization: Bearer <token>". ok is false when the
// header is absent; a present but malformed header gives "" and true.
func bearerToken(r *http.Request) (token string, ok bool) {
	h := r.Header.Values("Authorization")
	if len(h) == 0 {
		return "", false
	}
	if len(h) > 1 {
		return "", true
	}
	scheme, tok, found := strings.Cut(h[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", true
	}
	tok = strings.TrimSpace(tok)
	if tok == "" || strings.ContainsAny(tok, " \t") {
		return "", true
	}
	return tok, true
}

// unauthorized is the 401 problem, with the RFC 6750 challenge.
func unauthorized(sent bool) *ProblemDetail {
	p := problem(SlugUnauthorized, "the bearer token is missing, unknown or revoked")
	if sent {
		return p.withHeader("WWW-Authenticate", `Bearer realm="angelia", error="invalid_token"`)
	}
	return p.withHeader("WWW-Authenticate", `Bearer realm="angelia"`)
}

type principalKey struct{}

// principalFrom returns the authenticated principal of a request context.
func principalFrom(ctx context.Context) Principal {
	p, _ := ctx.Value(principalKey{}).(Principal)
	return p
}
