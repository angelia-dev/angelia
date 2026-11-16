package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
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

// Principal is who a request is authenticated as: an API token, or a
// browser session of the admin UI (ADR 0033 §2).
type Principal struct {
	// ID names the principal in logs and the audit trail: the token's id,
	// or "<provider>:<subject>" for a session (such as github:139323). It
	// must not be the token or the cookie, or derived from them in a
	// reversible way.
	ID string
	// Name is the token's operator label (ADR 0030 T9) or the signed-in
	// user's login, for the access log and the audit trail; it may be empty.
	Name  string
	Scope Scope
	// Session is set for a browser session and nil for a token.
	Session *SessionInfo
}

// SessionInfo is what a browser session principal adds (GET /v1/me).
type SessionInfo struct {
	// ID is the session's id (a ULID), never the cookie.
	ID string
	// Provider, Subject, Login, Name and AvatarURL are the identity the
	// session signed in with; Name and AvatarURL may be empty.
	Provider, Subject, Login, Name, AvatarURL string
	// Role is the user's role in ui.access now: viewer, editor or admin.
	Role string
}

// SessionAuthenticator resolves the admin UI's session cookie to its
// principal (ADR 0033 §6). internal/webauth implements it. The server calls
// it on a request without an Authorization header that carries the cookie,
// and again at least every 60 seconds on an open event stream. The role, and
// so the scope, is evaluated from the current access list on every call.
type SessionAuthenticator interface {
	// AuthenticateSession returns the session's principal, or an error
	// wrapping ErrUnauthenticated (an AuthFailure says why). Any other
	// error is a server failure (500).
	AuthenticateSession(ctx context.Context, cookie string) (Principal, error)
	// IdleTimeout is how long a session lives without a request: the
	// active filter of GET /v1/sessions.
	IdleTimeout() time.Duration
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

// sessionCookie reads the session cookie named name. ok is false when the
// request has none; several cookies of that name give "" and true (a
// malformed credential).
func sessionCookie(r *http.Request, name string) (value string, ok bool) {
	cs := r.CookiesNamed(name)
	switch {
	case len(cs) == 0:
		return "", false
	case len(cs) > 1:
		return "", true
	}
	return cs[0].Value, true
}

// CSRFHeader is the header that a request authenticated by the session
// cookie must send, with the value "1", to change anything (ADR 0033 §6). A
// cross-site form cannot set it, and a cross-site script cannot send it
// without a CORS preflight that the server never grants.
const CSRFHeader = "X-Angelia-CSRF"

// safeMethod reports whether a method changes nothing, so needs no CSRF
// check. Every other method, unknown ones included, needs it.
func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// CSRFOK applies the CSRF rule of ADR 0033 §6 to a request authenticated by
// the session cookie: a safe method passes; any other needs an Origin header
// equal to origin (the public URL's) and the header X-Angelia-CSRF: 1, each
// sent exactly once. internal/webauth applies it to the sign-out too.
func CSRFOK(r *http.Request, origin string) bool {
	if safeMethod(r.Method) {
		return true
	}
	o, x := r.Header.Values("Origin"), r.Header.Values(CSRFHeader)
	return origin != "" && len(o) == 1 && o[0] == origin && len(x) == 1 && x[0] == "1"
}

// csrfProblem is the 403 of a cookie request that fails CSRFOK.
func csrfProblem() *ProblemDetail {
	return problem(SlugCSRF, "a request authenticated by the session cookie that changes something needs an Origin equal to the instance's public URL and %s: 1", CSRFHeader)
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
