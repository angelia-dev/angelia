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
	ID string
	// Name is the token's operator label (ADR 0030 T9), for the access log
	// and the audit trail; it may be empty.
	Name  string
	Scope Scope
}

// ErrUnauthenticated is returned (wrapped) by an Authenticator for a token it
// does not accept: unknown, revoked or malformed.
var ErrUnauthenticated = errors.New("api: unauthenticated")

// Authenticator resolves a bearer token to its principal. internal/apitoken
// implements it over the stored API tokens (ADR 0030); an implementation
// compares secrets in constant time (crypto/subtle). The
// server calls it on every request and again at least every 60 seconds on an
// open event stream, so a revoked token stops working within that time.
type Authenticator interface {
	// Authenticate returns the token's principal, or an error wrapping
	// ErrUnauthenticated. Any other error is a server failure (500).
	Authenticate(ctx context.Context, token string) (Principal, error)
}

// DenyAll accepts no token, so every API request gets 401.
type DenyAll struct{}

// Authenticate implements Authenticator.
func (DenyAll) Authenticate(context.Context, string) (Principal, error) {
	return Principal{}, ErrUnauthenticated
}

// The reasons of a refused authentication (ADR 0030 T10, T11), a closed
// set: the label values of angelia_api_auth_failures_total and the
// auth_failure field of the access log. They never reach the caller, whose
// 401 is the same for every reason.
const (
	// AuthReasonMissing: no Authorization header.
	AuthReasonMissing = "missing"
	// AuthReasonMalformed: a header that is not one "Bearer <token>", or a
	// token that fails the format's strict parse or checksum.
	AuthReasonMalformed = "malformed"
	// AuthReasonUnknown: no token has this id, or the hash does not match;
	// also any refusal whose error does not say why.
	AuthReasonUnknown = "unknown"
	// AuthReasonRevoked and AuthReasonExpired come only after the hash
	// matched, so they tell nothing to a caller without the token.
	AuthReasonRevoked = "revoked"
	AuthReasonExpired = "expired"
)

// AuthReasons lists the reasons, in a fixed order.
func AuthReasons() []string {
	return []string{AuthReasonMissing, AuthReasonMalformed, AuthReasonUnknown, AuthReasonRevoked, AuthReasonExpired}
}

// AuthFailure is an authentication error that says why it failed. An
// Authenticator's error may implement it (internal/apitoken does); the
// server reads it with errors.As for the metric and the access log.
type AuthFailure interface {
	error
	// FailureReason returns one of the AuthReason values and, for
	// AuthReasonRevoked and AuthReasonExpired only, the token id.
	FailureReason() (reason, tokenID string)
}

// AuthMetrics counts refused authentications by reason
// (angelia_api_auth_failures_total). metrics.Metrics implements it.
type AuthMetrics interface {
	AuthFailed(reason string)
}

// authFailed records a refused authentication for the access log and the
// metric.
func (s *Server) authFailed(st *reqState, reason, id string) {
	st.authFailure = reason
	if reason == AuthReasonRevoked || reason == AuthReasonExpired {
		st.failedID = id
	}
	if s.authObs != nil {
		s.authObs.AuthFailed(reason)
	}
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
