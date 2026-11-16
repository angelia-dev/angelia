package webauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/angelia-dev/angelia/internal/api"
	"github.com/angelia-dev/angelia/internal/config"
)

// The routes of the handler, under api.UIAuthPath (ADR 0033 §4).
const (
	loginPath    = "/ui/auth/login/"
	callbackPath = "/ui/auth/callback/"
	logoutPath   = "/ui/auth/logout"
	// homePath is where a successful sign-in lands.
	homePath = "/ui/"
)

// oauthCookieTTL bounds a sign-in: the oauth cookie's Max-Age, also checked
// on the server from the time inside the cookie.
const oauthCookieTTL = 10 * time.Minute

// HandlerOptions configures a Handler.
type HandlerOptions struct {
	// Providers are the identity providers people may sign in with.
	Providers []IdentityProvider
	// Sessions creates and ends the sessions.
	Sessions *Sessions
	// UI is the ui section of the instance configuration: public_url
	// gives the CSRF origin and whether the cookies may drop Secure.
	UI config.UI
	// Metrics counts sign-ins; nil counts nothing.
	Metrics LoginMetrics
	// Logger; nil discards.
	Logger *slog.Logger
	// Now defaults to the real clock.
	Now func() time.Time
}

// Handler serves the browser routes of the sign-in: GET
// /ui/auth/login/{provider}, GET /ui/auth/callback/{provider} and POST
// /ui/auth/logout. They are outside the REST API and its specification.
type Handler struct {
	providers map[string]IdentityProvider
	sessions  *Sessions
	origin    string
	local     bool
	metrics   LoginMetrics
	log       *slog.Logger
	now       func() time.Time
	mux       *http.ServeMux
}

var providerNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// NewHandler returns the handler of the sign-in routes.
func NewHandler(opts HandlerOptions) (*Handler, error) {
	if opts.Sessions == nil || len(opts.Providers) == 0 {
		return nil, errors.New("webauth: Sessions and at least one provider are required")
	}
	h := &Handler{
		providers: map[string]IdentityProvider{}, sessions: opts.Sessions, origin: opts.UI.PublicOrigin(),
		local: opts.UI.LocalDev(), metrics: opts.Metrics, log: opts.Logger, now: opts.Now,
	}
	if h.origin == "" {
		return nil, errors.New("webauth: ui.public_url is not a valid URL")
	}
	if h.log == nil {
		h.log = slog.New(slog.DiscardHandler)
	}
	if h.now == nil {
		h.now = time.Now
	}
	for _, p := range opts.Providers {
		if !providerNameRE.MatchString(p.Name()) {
			return nil, fmt.Errorf("webauth: provider name %q is not lower-case letters, digits and hyphens", p.Name())
		}
		if _, dup := h.providers[p.Name()]; dup {
			return nil, fmt.Errorf("webauth: provider %q is listed twice", p.Name())
		}
		h.providers[p.Name()] = p
	}
	h.mux = http.NewServeMux()
	h.mux.HandleFunc("GET "+loginPath+"{provider}", h.login)
	h.mux.HandleFunc("GET "+callbackPath+"{provider}", h.callback)
	h.mux.HandleFunc("POST "+logoutPath, h.logout)
	return h, nil
}

// RedirectURL is the callback URL of a provider for a public URL, to
// register with the provider's application.
func RedirectURL(ui config.UI, provider string) string {
	return ui.PublicOrigin() + callbackPath + provider
}

// ServeHTTP implements http.Handler. Every response is marked no-store and
// sends no Referer onwards (the callback's URL holds the code).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hd := w.Header()
	hd.Set("Cache-Control", "no-store")
	hd.Set("Referrer-Policy", "no-referrer")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("X-Frame-Options", "DENY")
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) count(provider, result string) {
	if h.metrics != nil {
		h.metrics.UILogin(provider, result)
	}
}

// cookie returns a cookie of the handler: HttpOnly, SameSite=Lax, Path=/,
// and Secure with the __Host- name unless the public URL is a loopback
// http:// URL.
func (h *Handler) cookie(name, local, value string, maxAge int) *http.Cookie {
	// G124: Secure is dropped only for a loopback http:// public_url.
	c := &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: !h.local, SameSite: http.SameSiteLaxMode} //nolint:gosec // see above
	if h.local {
		c.Name = local
	}
	return c
}

func (h *Handler) oauthName() string {
	if h.local {
		return oauthCookieNameLocal
	}
	return oauthCookieName
}

func (h *Handler) sessionName() string {
	if h.local {
		return SessionCookieLocal
	}
	return SessionCookie
}

// random returns n random bytes in unpadded base64url.
func random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read does not fail
	return base64.RawURLEncoding.EncodeToString(b)
}

// challengeS256 is the PKCE S256 challenge of a verifier (RFC 7636).
func challengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// login starts a sign-in: a fresh state, PKCE verifier and nonce go into
// the oauth cookie, and the browser goes to the provider.
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	p, ok := h.providers[r.PathValue("provider")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	state, verifier, nonce := random(32), random(32), random(32)
	target, err := p.AuthCodeURL(r.Context(), state, challengeS256(verifier), nonce)
	if err != nil {
		h.count(p.Name(), ResultProviderError)
		h.log.WarnContext(r.Context(), "ui sign-in failed: the provider cannot be reached", "provider", p.Name(), "error", err)
		h.page(w, http.StatusBadGateway, pageProviderError, "")
		return
	}
	value := strings.Join([]string{p.Name(), strconv.FormatInt(h.now().Unix(), 10), state, verifier, nonce}, ".")
	http.SetCookie(w, h.cookie(oauthCookieName, oauthCookieNameLocal, value, int(oauthCookieTTL/time.Second)))
	http.Redirect(w, r, target, http.StatusFound)
}

// oauthState reads the oauth cookie of a callback for provider: its state,
// verifier and nonce, if the cookie is well-formed, for this provider, and
// not older than oauthCookieTTL.
func (h *Handler) oauthState(r *http.Request, provider string) (state, verifier, nonce string, ok bool) {
	cs := r.CookiesNamed(h.oauthName())
	if len(cs) != 1 {
		return "", "", "", false
	}
	parts := strings.Split(cs[0].Value, ".")
	if len(parts) != 5 || parts[0] != provider || len(parts[2]) != 43 || len(parts[3]) != 43 || len(parts[4]) != 43 {
		return "", "", "", false
	}
	at, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", "", "", false
	}
	age := h.now().Sub(time.Unix(at, 0))
	if age < -time.Minute || age > oauthCookieTTL {
		return "", "", "", false
	}
	return parts[2], parts[3], parts[4], true
}

// callback ends a sign-in: it checks state against the oauth cookie (in
// constant time), exchanges the code, matches ui.access and, for a user it
// lists, creates the session and sets its cookie.
func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	p, ok := h.providers[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	// The oauth cookie is used once, whatever happens next.
	http.SetCookie(w, h.cookie(oauthCookieName, oauthCookieNameLocal, "", -1))
	ctx := r.Context()
	q := r.URL.Query()
	state, verifier, nonce, ok := h.oauthState(r, name)
	if !ok || len(q["state"]) != 1 || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
		h.count(name, ResultInvalidState)
		h.log.InfoContext(ctx, "ui sign-in refused: missing, expired or mismatched state", "provider", name)
		h.page(w, http.StatusBadRequest, pageSignInFailed, "")
		return
	}
	if e := q.Get("error"); e != "" {
		if e == "access_denied" {
			h.count(name, ResultCancelled)
			h.page(w, http.StatusBadRequest, pageCancelled, "")
			return
		}
		h.count(name, ResultProviderError)
		h.log.WarnContext(ctx, "ui sign-in failed: the provider returned an error", "provider", name, "error", safeWord(e))
		h.page(w, http.StatusBadGateway, pageProviderError, "")
		return
	}
	code := q.Get("code")
	if code == "" || len(q["code"]) != 1 {
		h.count(name, ResultProviderError)
		h.page(w, http.StatusBadRequest, pageSignInFailed, "")
		return
	}
	id, err := p.Exchange(ctx, code, verifier, nonce)
	if err != nil {
		h.count(name, ResultProviderError)
		h.log.WarnContext(ctx, "ui sign-in failed: the provider did not identify the user", "provider", name, "error", err)
		h.page(w, http.StatusBadGateway, pageProviderError, "")
		return
	}
	cookie, row, m, err := h.sessions.signIn(ctx, id, r.UserAgent())
	switch {
	case err != nil:
		h.count(name, ResultError)
		h.log.ErrorContext(ctx, "ui sign-in failed", "provider", name, "principal", id.PrincipalID(), "error", err)
		h.page(w, http.StatusInternalServerError, pageServerError, "")
		return
	case cookie == "" && m.mismatch:
		h.count(name, ResultIdentityMismatch)
		h.log.WarnContext(ctx, "ui sign-in refused: the login or email is pinned to another account id",
			"provider", name, "login", id.Login, "subject", id.Subject, "pinned_subject", m.pinnedTo)
		h.page(w, http.StatusForbidden, pageNotAuthorised, id.Login)
		return
	case cookie == "":
		h.count(name, ResultNotAuthorised)
		h.log.InfoContext(ctx, "ui sign-in refused: not in ui.access", "provider", name, "login", id.Login, "subject", id.Subject)
		h.page(w, http.StatusForbidden, pageNotAuthorised, id.Login)
		return
	}
	if m.mismatch {
		h.log.WarnContext(ctx, "ui access entry ignored: the login or email is pinned to another account id",
			"provider", name, "login", id.Login, "subject", id.Subject, "pinned_subject", m.pinnedTo)
	}
	h.count(name, ResultSuccess)
	h.log.InfoContext(ctx, "ui sign-in", "provider", name, "principal", id.PrincipalID(), "login", id.Login,
		"role", m.role, "session_id", row.ID)
	// Signing in again ends the session the browser held before.
	if cs := r.CookiesNamed(h.sessionName()); len(cs) == 1 {
		old, changed, err := h.sessions.revokeCookie(ctx, cs[0].Value, id.PrincipalID())
		switch {
		case err != nil:
			h.log.ErrorContext(ctx, "ui sign-in: the previous session was not revoked", "principal", id.PrincipalID(), "error", err)
		case changed:
			h.log.InfoContext(ctx, "ui session replaced by a new sign-in", "principal", id.PrincipalID(),
				"session_id", old.ID, "new_session_id", row.ID)
		}
	}
	maxAge := int(row.ExpiresAt.Sub(h.now()) / time.Second)
	http.SetCookie(w, h.cookie(SessionCookie, SessionCookieLocal, cookie, max(maxAge, 1)))
	http.Redirect(w, r, homePath, http.StatusSeeOther)
}

// logout revokes the session of the request's cookie and clears it. It is
// a cookie-authenticated change, so it needs the CSRF headers (ADR 0033 §6).
// It answers 204 whether or not a session was active.
func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if !api.CSRFOK(r, h.origin) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintf(w, `{"type":%q,"title":"CSRF check failed","status":403,"detail":"sign-out needs an Origin equal to the instance's public URL and %s: 1"}`,
			api.ProblemTypeBase+api.SlugCSRF, api.CSRFHeader)
		return
	}
	ctx := r.Context()
	if cs := r.CookiesNamed(h.sessionName()); len(cs) == 1 {
		row, changed, err := h.sessions.signOut(ctx, cs[0].Value)
		if err != nil {
			h.log.ErrorContext(ctx, "ui sign-out failed", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if changed {
			h.log.InfoContext(ctx, "ui sign-out", "principal", row.Provider+":"+row.Subject, "login", row.Login, "session_id", row.ID)
		}
	}
	http.SetCookie(w, h.cookie(SessionCookie, SessionCookieLocal, "", -1))
	w.WriteHeader(http.StatusNoContent)
}

// The pages of the sign-in, server-rendered without script.
type pageKind int

const (
	pageSignInFailed pageKind = iota
	pageCancelled
	pageProviderError
	pageServerError
	pageNotAuthorised
)

var pageTexts = map[pageKind]struct{ title, text string }{
	pageSignInFailed:  {"Sign-in failed", "The sign-in expired or was started in another browser. Start again."},
	pageCancelled:     {"Sign-in cancelled", "You did not authorise the sign-in."},
	pageProviderError: {"Sign-in failed", "The identity provider could not confirm who you are. Try again later."},
	pageServerError:   {"Sign-in failed", "The server could not complete the sign-in. Try again later."},
	pageNotAuthorised: {"Not authorised", "This account has no access to this Angelia instance. Ask an administrator to add it to ui.access."},
}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>{{.Title}} · Angelia</title>
</head>
<body>
<main>
<h1>{{.Title}}</h1>
{{if .Login}}<p>Signed in as <strong>{{.Login}}</strong>.</p>{{end}}
<p>{{.Text}}</p>
<p><a href="/ui/">Back to Angelia</a></p>
</main>
</body>
</html>
`))

// page writes a page with a CSP that allows nothing to load or run.
func (h *Handler) page(w http.ResponseWriter, status int, kind pageKind, login string) {
	t := pageTexts[kind]
	hd := w.Header()
	hd.Set("Content-Type", "text/html; charset=utf-8")
	hd.Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.WriteHeader(status)
	_ = pageTmpl.Execute(w, struct{ Title, Text, Login string }{t.title, t.text, login})
}
