// Package webauth signs people in to the admin web UI (story #262, ADR 0033
// §3 to §6): an [IdentityProvider] (GitHub, an OAuth App with PKCE) proves who
// they are, the instance's ui.access list gives them a role, and a
// server-side session, held by a cookie, carries that role's API scope on
// every /v1 request.
//
// [Sessions] is the [api.SessionAuthenticator]: it reads the session row by
// the cookie's id, compares hashes in constant time, ends the session at its
// idle timeout, its maximum lifetime, its revocation or once its identity no
// longer matches ui.access (re-evaluated on every request), and coalesces the
// last-seen writes like the API tokens' last use (ADR 0030 T6). [Handler]
// serves the browser routes under /ui/auth/: sign-in, the provider's
// callback and sign-out.
//
// Nothing here logs or returns a cookie, an OAuth code, state or verifier,
// a provider access token or the client secret.
package webauth

import (
	"context"
	"strconv"

	"github.com/angelia-dev/angelia/internal/api"
	"github.com/angelia-dev/angelia/internal/config"
)

// Identity is who a provider says signed in.
type Identity struct {
	// Provider is the provider's Name. Subject is its stable id of the
	// user (GitHub's numeric id); Login the user's login, which can change.
	Provider, Subject, Login string
	// Name and AvatarURL may be empty.
	Name, AvatarURL string
}

// PrincipalID is the principal id of an identity: "<provider>:<subject>"
// (ADR 0033 §5), so audit records survive a rename of the login.
func (i Identity) PrincipalID() string { return i.Provider + ":" + i.Subject }

// IdentityProvider is an OAuth 2.0 identity provider (ADR 0033 §4). A new
// provider (generic OIDC, #268) implements it and gets its own callback
// route; ui.access entries name their provider.
type IdentityProvider interface {
	// Name names the provider in routes, cookies, metrics and principal
	// ids: lower-case letters, digits and hyphens ("github").
	Name() string
	// AuthCodeURL is the provider's authorization URL for this sign-in,
	// with state and the PKCE S256 challenge.
	AuthCodeURL(state, codeChallenge string) string
	// Exchange trades the authorization code and the PKCE verifier for the
	// user's identity. Any credential the provider returns is used for this
	// call only: it is never stored, logged or returned.
	Exchange(ctx context.Context, code, codeVerifier string) (Identity, error)
}

// roleRank orders the roles of ui.access; 0 is no role.
func roleRank(role string) int {
	switch role {
	case config.UIRoleViewer:
		return 1
	case config.UIRoleEditor:
		return 2
	case config.UIRoleAdmin:
		return 3
	}
	return 0
}

// RoleScope is the API scope of a role of ui.access (ADR 0033 §2): viewer
// is read, editor write, admin admin; anything else is none.
func RoleScope(role string) api.Scope {
	switch role {
	case config.UIRoleViewer:
		return api.ScopeRead
	case config.UIRoleEditor:
		return api.ScopeWrite
	case config.UIRoleAdmin:
		return api.ScopeAdmin
	}
	return api.ScopeNone
}

// Cookie names (ADR 0033 §4, §5). The __Host- prefix makes the browser
// refuse a cookie that is not Secure, has a Domain or another Path, so a
// sibling subdomain cannot plant one. A loopback http:// public_url (local
// development) cannot use it.
const (
	SessionCookie        = "__Host-angelia_session"
	SessionCookieLocal   = "angelia_session"
	oauthCookieName      = "__Host-angelia_oauth"
	oauthCookieNameLocal = "angelia_oauth"
)

// SessionCookieName is the session cookie of a UI configuration.
func SessionCookieName(ui config.UI) string {
	if ui.LocalDev() {
		return SessionCookieLocal
	}
	return SessionCookie
}

// The results of a sign-in, a closed set: the result label of
// angelia_ui_logins_total.
const (
	// ResultSuccess: a session was created.
	ResultSuccess = "success"
	// ResultNotAuthorised: the provider identified the user, but no
	// ui.access entry gives them a role.
	ResultNotAuthorised = "not_authorised"
	// ResultIdentityMismatch: a github_user entry matched the login, but
	// the login is pinned to another id (a renamed or re-registered login).
	ResultIdentityMismatch = "identity_mismatch"
	// ResultInvalidState: the callback's state or the sign-in cookie is
	// missing, expired or does not match.
	ResultInvalidState = "invalid_state"
	// ResultCancelled: the user declined on the provider's page.
	ResultCancelled = "cancelled"
	// ResultProviderError: the provider returned an error, or the code
	// exchange or the user lookup failed.
	ResultProviderError = "provider_error"
	// ResultError: the server failed (the store).
	ResultError = "error"
)

// LoginResults lists the results, in a fixed order.
func LoginResults() []string {
	return []string{
		ResultSuccess, ResultNotAuthorised, ResultIdentityMismatch, ResultInvalidState,
		ResultCancelled, ResultProviderError, ResultError,
	}
}

// Providers lists the providers this build knows, the closed set of the
// provider label of angelia_ui_logins_total.
func Providers() []string { return []string{GitHubName} }

// LoginMetrics counts sign-ins (angelia_ui_logins_total). metrics.Metrics
// implements it.
type LoginMetrics interface {
	UILogin(provider, result string)
}

// githubSubject is the subject of a github_id entry.
func githubSubject(id int64) string { return strconv.FormatInt(id, 10) }
