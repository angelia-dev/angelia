package webauth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/api"
	"github.com/angelia-dev/angelia/internal/config"
)

// fakeProvider is an IdentityProvider whose Exchange returns a set identity
// for the code "good".
type fakeProvider struct {
	name      string
	id        Identity
	err       error
	challenge string
	verifier  string
}

func (p *fakeProvider) Name() string { return p.name }

func (p *fakeProvider) AuthCodeURL(state, challenge string) string {
	p.challenge = challenge
	return "https://idp.example/authorize?state=" + url.QueryEscape(state)
}

func (p *fakeProvider) Exchange(_ context.Context, code, verifier string) (Identity, error) {
	p.verifier = verifier
	if p.err != nil {
		return Identity{}, p.err
	}
	if code != "good" {
		return Identity{}, &ProviderError{msg: "bad code"}
	}
	return p.id, nil
}

// fakeMetrics counts UILogin calls.
type fakeMetrics struct {
	mu sync.Mutex
	n  map[string]int
}

func (m *fakeMetrics) UILogin(provider, result string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.n == nil {
		m.n = map[string]int{}
	}
	m.n[provider+"/"+result]++
}

func (m *fakeMetrics) get(k string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.n[k]
}

type handlerEnv struct {
	t       *testing.T
	h       *Handler
	p       *fakeProvider
	s       *Sessions
	st      *countingStore
	c       *clock
	m       *fakeMetrics
	logs    *syncBuf
	session string
	oauth   string
}

func newHandlerEnv(t *testing.T, ui config.UI) *handlerEnv {
	t.Helper()
	s, st, c, logs := newTestSessions(t, ui)
	p := &fakeProvider{name: GitHubName, id: alice}
	m := &fakeMetrics{}
	h, err := NewHandler(HandlerOptions{
		Providers: []IdentityProvider{p}, Sessions: s, UI: ui, Metrics: m,
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), Now: c.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	e := &handlerEnv{t: t, h: h, p: p, s: s, st: st, c: c, m: m, logs: logs, session: SessionCookie, oauth: oauthCookieName}
	if ui.LocalDev() {
		e.session, e.oauth = SessionCookieLocal, oauthCookieNameLocal
	}
	return e
}

// result is a response of the handler, its body read.
type result struct {
	StatusCode int
	Status     string
	Header     http.Header
	Body       string
	cookies    []*http.Cookie
}

func (e *handlerEnv) do(r *http.Request) result {
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return result{
		StatusCode: w.Code, Status: http.StatusText(w.Code), Header: w.Header(), Body: w.Body.String(),
		cookies: (&http.Response{Header: w.Header()}).Cookies(),
	}
}

// addCookie adds a request cookie.
func addCookie(r *http.Request, name, value string) {
	r.Header.Add("Cookie", name+"="+value)
}

func cookieOf(resp result, name string) *http.Cookie {
	for _, c := range resp.cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// login starts a sign-in and returns the oauth cookie and the state.
func (e *handlerEnv) login(t *testing.T) (*http.Cookie, string) {
	t.Helper()
	resp := e.do(httptest.NewRequest(http.MethodGet, "/ui/auth/login/github", nil))
	if resp.StatusCode != http.StatusFound {
		t.Fatal(resp.Status)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	state := loc.Query().Get("state")
	c := cookieOf(resp, e.oauth)
	if c == nil || len(state) != 43 || !strings.Contains(c.Value, state) || c.MaxAge != 600 || !c.HttpOnly ||
		c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Secure == (e.oauth == oauthCookieNameLocal) {
		t.Fatalf("%+v state %q", c, state)
	}
	if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatal(resp.Header)
	}
	return c, state
}

func (e *handlerEnv) callback(query string, oauth *http.Cookie) result {
	r := httptest.NewRequest(http.MethodGet, "/ui/auth/callback/github?"+query, nil)
	r.Header.Set("User-Agent", "test-browser")
	if oauth != nil {
		addCookie(r, oauth.Name, oauth.Value)
	}
	return e.do(r)
}

func TestHandlerSignIn(t *testing.T) {
	e := newHandlerEnv(t, testUI())
	oc, state := e.login(t)
	resp := e.callback("code=good&state="+state, oc)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/ui/" {
		t.Fatal(resp.Status, resp.Body)
	}
	if c := cookieOf(resp, e.oauth); c == nil || c.MaxAge != -1 {
		t.Fatal("the oauth cookie is not cleared", c)
	}
	sc := cookieOf(resp, SessionCookie)
	if sc == nil || !sc.Secure || !sc.HttpOnly || sc.SameSite != http.SameSiteLaxMode || sc.Path != "/" || sc.Domain != "" ||
		sc.MaxAge != int((24*time.Hour)/time.Second) || len(sc.Value) != CookieLen {
		t.Fatalf("%+v", sc)
	}
	// PKCE: the verifier sent to Exchange is the one behind the challenge.
	if challengeS256(e.p.verifier) != e.p.challenge || len(e.p.verifier) != 43 {
		t.Fatal("PKCE verifier and challenge differ")
	}
	p, err := e.s.AuthenticateSession(t.Context(), sc.Value)
	if err != nil || p.ID != "github:1001" || p.Session.Role != config.UIRoleAdmin {
		t.Fatalf("%+v %v", p, err)
	}
	row, _ := e.st.UISessionByID(t.Context(), p.Session.ID)
	if row.UserAgent != "test-browser" {
		t.Fatal(row.UserAgent)
	}
	if e.m.get("github/success") != 1 {
		t.Fatal(e.m.n)
	}
	logs := e.logs.String()
	for _, secret := range []string{sc.Value, state, oc.Value, e.p.verifier, "code=good"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("the log holds %q", secret)
		}
	}
	if !strings.Contains(logs, "ui sign-in") || !strings.Contains(logs, "github:1001") {
		t.Fatal(logs)
	}

	// Sign-out needs the CSRF headers, revokes and clears the cookie.
	logout := func(origin, csrf string) result {
		r := httptest.NewRequest(http.MethodPost, "/ui/auth/logout", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if csrf != "" {
			r.Header.Set(api.CSRFHeader, csrf)
		}
		addCookie(r, SessionCookie, sc.Value)
		return e.do(r)
	}
	for _, bad := range [][2]string{{"", "1"}, {"https://evil.example", "1"}, {"https://angelia.example.com", ""}} {
		resp := logout(bad[0], bad[1])
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(resp.Body, `"`+api.ProblemTypeBase+api.SlugCSRF+`"`) {
			t.Fatal(bad, resp.Status)
		}
	}
	if _, err := e.s.AuthenticateSession(t.Context(), sc.Value); err != nil {
		t.Fatal("refused sign-out ended the session", err)
	}
	resp = logout("https://angelia.example.com", "1")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatal(resp.Status)
	}
	if c := cookieOf(resp, SessionCookie); c == nil || c.MaxAge != -1 {
		t.Fatal("the session cookie is not cleared", c)
	}
	_, err = e.s.AuthenticateSession(t.Context(), sc.Value)
	wantReason(t, err, api.AuthReasonRevoked)
	if !strings.Contains(e.logs.String(), "ui sign-out") {
		t.Fatal(e.logs.String())
	}
	// Again, and without any cookie: still 204.
	if resp := logout("https://angelia.example.com", "1"); resp.StatusCode != http.StatusNoContent {
		t.Fatal(resp.Status)
	}
	r := httptest.NewRequest(http.MethodPost, "/ui/auth/logout", nil)
	r.Header.Set("Origin", "https://angelia.example.com")
	r.Header.Set(api.CSRFHeader, "1")
	if resp := e.do(r); resp.StatusCode != http.StatusNoContent {
		t.Fatal(resp.Status)
	}
	// A store failure answers 500.
	e.st.failRead = errors.New("boom")
	if resp := logout("https://angelia.example.com", "1"); resp.StatusCode != http.StatusInternalServerError {
		t.Fatal(resp.Status)
	}
}

// Signing in again from a browser that holds a session ends that session,
// revoked by the principal that signed in; a stale or junk cookie is
// ignored.
func TestHandlerSignInAgain(t *testing.T) {
	e := newHandlerEnv(t, testUI())
	signInWith := func(old string) *http.Cookie {
		t.Helper()
		oc, state := e.login(t)
		r := httptest.NewRequest(http.MethodGet, "/ui/auth/callback/github?code=good&state="+state, nil)
		addCookie(r, oc.Name, oc.Value)
		if old != "" {
			addCookie(r, SessionCookie, old)
		}
		resp := e.do(r)
		sc := cookieOf(resp, SessionCookie)
		if resp.StatusCode != http.StatusSeeOther || sc == nil {
			t.Fatal(resp.Status, resp.Body)
		}
		return sc
	}
	first := signInWith("")
	p1, err := e.s.AuthenticateSession(t.Context(), first.Value)
	if err != nil {
		t.Fatal(err)
	}
	second := signInWith(first.Value)
	if second.Value == first.Value {
		t.Fatal("the cookie was reused")
	}
	if _, err := e.s.AuthenticateSession(t.Context(), second.Value); err != nil {
		t.Fatal(err)
	}
	_, err = e.s.AuthenticateSession(t.Context(), first.Value)
	wantReason(t, err, api.AuthReasonRevoked)
	row, err := e.st.UISessionByID(t.Context(), p1.Session.ID)
	if err != nil || row.RevokedAt == nil || row.RevokedBy != "github:1001" {
		t.Fatalf("%+v %v", row, err)
	}
	if !strings.Contains(e.logs.String(), "ui session replaced by a new sign-in") || strings.Contains(e.logs.String(), first.Value) {
		t.Fatal(e.logs.String())
	}
	// The revoked cookie, or junk, does not stop a new sign-in.
	signInWith(first.Value)
	signInWith("junk")
}

func TestHandlerState(t *testing.T) {
	e := newHandlerEnv(t, testUI())
	oc, state := e.login(t)
	other := "x" + state[1:]
	if other == state {
		other = "y" + state[1:]
	}
	cases := map[string]struct {
		query  string
		cookie *http.Cookie
	}{
		"no cookie":      {"code=good&state=" + state, nil},
		"wrong state":    {"code=good&state=" + other, oc},
		"no state":       {"code=good", oc},
		"two states":     {"code=good&state=" + state + "&state=" + state, oc},
		"bad cookie":     {"code=good&state=" + state, &http.Cookie{Name: e.oauth, Value: "github.1.2.3", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}},
		"other provider": {"code=good&state=" + state, &http.Cookie{Name: e.oauth, Value: strings.Replace(oc.Value, "github.", "gitlab.", 1), Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}},
		"bad time":       {"code=good&state=" + state, &http.Cookie{Name: e.oauth, Value: replacePart(oc.Value, 1, "x"), Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}},
	}
	for name, tc := range cases {
		resp := e.callback(tc.query, tc.cookie)
		b := resp.Body
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(b, "Sign-in failed") || cookieOf(resp, SessionCookie) != nil {
			t.Errorf("%s: %s", name, resp.Status)
		}
		if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") || strings.Contains(b, "<script") {
			t.Errorf("%s: %v", name, resp.Header)
		}
	}
	// Two oauth cookies.
	r := httptest.NewRequest(http.MethodGet, "/ui/auth/callback/github?code=good&state="+state, nil)
	addCookie(r, e.oauth, oc.Value)
	addCookie(r, e.oauth, oc.Value)
	if resp := e.do(r); resp.StatusCode != http.StatusBadRequest {
		t.Fatal(resp.Status)
	}
	// Expired: more than ten minutes after the login.
	e.c.Add(11 * time.Minute)
	if resp := e.callback("code=good&state="+state, oc); resp.StatusCode != http.StatusBadRequest {
		t.Fatal(resp.Status)
	}
	if n := e.m.get("github/invalid_state"); n != len(cases)+2 {
		t.Fatal(n)
	}
	if strings.Contains(e.logs.String(), state) {
		t.Fatal("the log holds the state")
	}
}

func replacePart(v string, i int, s string) string {
	parts := strings.Split(v, ".")
	parts[i] = s
	return strings.Join(parts, ".")
}

func TestHandlerProviderOutcomes(t *testing.T) {
	e := newHandlerEnv(t, testUI())
	run := func(query string) result {
		t.Helper()
		oc, state := e.login(t)
		return e.callback(query+"&state="+state, oc)
	}
	if resp := run("error=access_denied"); resp.StatusCode != http.StatusBadRequest || !strings.Contains(resp.Body, "cancelled") {
		t.Fatal(resp.Status)
	}
	if resp := run("error=server_error<b>"); resp.StatusCode != http.StatusBadGateway {
		t.Fatal(resp.Status)
	}
	if resp := run("code="); resp.StatusCode != http.StatusBadRequest {
		t.Fatal(resp.Status)
	}
	if resp := run("code=bad"); resp.StatusCode != http.StatusBadGateway {
		t.Fatal(resp.Status)
	}
	e.p.err = context.DeadlineExceeded
	if resp := run("code=good"); resp.StatusCode != http.StatusBadGateway {
		t.Fatal(resp.Status)
	}
	e.p.err = nil
	if e.m.get("github/cancelled") != 1 || e.m.get("github/provider_error") != 4 {
		t.Fatal(e.m.n)
	}
	if strings.Contains(e.logs.String(), "<b>") {
		t.Fatal("the provider's error is logged raw")
	}

	// Not in ui.access: a 403 page that names the login, escaped.
	e.p.id = Identity{Provider: GitHubName, Subject: "77", Login: "<i>eve</i>"}
	resp := run("code=good")
	b := resp.Body
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(b, "Not authorised") || strings.Contains(b, "<i>") ||
		!strings.Contains(b, "&lt;i&gt;eve&lt;/i&gt;") || cookieOf(resp, SessionCookie) != nil {
		t.Fatal(resp.Status, b)
	}
	// Pinned to another id.
	e.p.id = alice
	if resp := run("code=good"); resp.StatusCode != http.StatusSeeOther {
		t.Fatal(resp.Status)
	}
	e.p.id = mal
	if resp := run("code=good"); resp.StatusCode != http.StatusForbidden {
		t.Fatal(resp.Status)
	}
	if e.m.get("github/not_authorised") != 1 || e.m.get("github/identity_mismatch") != 1 {
		t.Fatal(e.m.n)
	}
	if !strings.Contains(e.logs.String(), "pinned_subject=1001") {
		t.Fatal(e.logs.String())
	}
	// A store failure.
	e.p.id = bob
	e.st.failMake = errors.New("boom")
	if resp := run("code=good"); resp.StatusCode != http.StatusInternalServerError {
		t.Fatal(resp.Status)
	}
	if e.m.get("github/error") != 1 {
		t.Fatal(e.m.n)
	}
}

func TestHandlerMismatchWithOtherGrant(t *testing.T) {
	ui := testUI()
	ui.Access = append(ui.Access, config.UIAccess{GitHubID: 6666, Role: config.UIRoleViewer})
	e := newHandlerEnv(t, ui)
	if _, err := e.s.access.pin(t.Context(), GitHubName, "alice", "1001"); err != nil {
		t.Fatal(err)
	}
	e.p.id = mal
	oc, state := e.login(t)
	if resp := e.callback("code=good&state="+state, oc); resp.StatusCode != http.StatusSeeOther {
		t.Fatal(resp.Status)
	}
	if !strings.Contains(e.logs.String(), "ui access entry ignored") {
		t.Fatal(e.logs.String())
	}
}

func TestHandlerRoutes(t *testing.T) {
	e := newHandlerEnv(t, testUI())
	for _, r := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/ui/auth/login/gitlab", nil),
		httptest.NewRequest(http.MethodGet, "/ui/auth/callback/gitlab", nil),
	} {
		if resp := e.do(r); resp.StatusCode != http.StatusNotFound {
			t.Fatal(r.URL, resp.Status)
		}
	}
	if resp := e.do(httptest.NewRequest(http.MethodPost, "/ui/auth/login/github", nil)); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatal(resp.Status)
	}
	if resp := e.do(httptest.NewRequest(http.MethodGet, "/ui/auth/logout", nil)); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatal(resp.Status)
	}
	if RedirectURL(testUI(), "github") != "https://angelia.example.com/ui/auth/callback/github" {
		t.Fatal(RedirectURL(testUI(), "github"))
	}
}

func TestHandlerLocalDev(t *testing.T) {
	ui := testUI()
	ui.PublicURL = "http://localhost:8080"
	e := newHandlerEnv(t, ui)
	oc, state := e.login(t)
	resp := e.callback("code=good&state="+state, oc)
	sc := cookieOf(resp, SessionCookieLocal)
	if resp.StatusCode != http.StatusSeeOther || sc == nil || sc.Secure || !sc.HttpOnly {
		t.Fatal(resp.Status, sc)
	}
	r := httptest.NewRequest(http.MethodPost, "/ui/auth/logout", nil)
	r.Header.Set("Origin", "http://localhost:8080")
	r.Header.Set(api.CSRFHeader, "1")
	addCookie(r, SessionCookieLocal, sc.Value)
	if resp := e.do(r); resp.StatusCode != http.StatusNoContent || cookieOf(resp, SessionCookieLocal) == nil {
		t.Fatal(resp.Status)
	}
	_, err := e.s.AuthenticateSession(t.Context(), sc.Value)
	wantReason(t, err, api.AuthReasonRevoked)
}

func TestNewHandlerErrors(t *testing.T) {
	s, _, _, _ := newTestSessions(t, testUI())
	p := &fakeProvider{name: GitHubName}
	for name, o := range map[string]HandlerOptions{
		"no sessions":  {Providers: []IdentityProvider{p}, UI: testUI()},
		"no provider":  {Sessions: s, UI: testUI()},
		"no origin":    {Providers: []IdentityProvider{p}, Sessions: s},
		"bad provider": {Providers: []IdentityProvider{&fakeProvider{name: "Git Hub"}}, Sessions: s, UI: testUI()},
	} {
		if _, err := NewHandler(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Defaults: no metrics, logger or clock.
	h, err := NewHandler(HandlerOptions{Providers: []IdentityProvider{p}, Sessions: s, UI: testUI()})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ui/auth/callback/github?code=x&state=y", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatal(w.Code)
	}
}
