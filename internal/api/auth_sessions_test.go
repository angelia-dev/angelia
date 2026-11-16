package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/api"
	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/webauth"
)

// Tests on real admin UI sessions: the server with internal/webauth's
// Sessions over a SQLite store, the sessions created by its sign-in handler
// with a fake identity provider.

const sessOrigin = "https://angelia.example.com"

// fakeIDP signs in whoever it is told to.
type fakeIDP struct{ who webauth.Identity }

func (*fakeIDP) Name() string { return webauth.GitHubName }

func (*fakeIDP) AuthCodeURL(state, _ string) string {
	return "https://idp.example/authorize?state=" + url.QueryEscape(state)
}

func (p *fakeIDP) Exchange(context.Context, string, string) (webauth.Identity, error) {
	return p.who, nil
}

type sessEnv struct {
	*tokEnv
	idp      *fakeIDP
	sessions *webauth.Sessions
	handler  *webauth.Handler
}

func sessUI() config.UI {
	return config.UI{
		Enabled: true, PublicURL: sessOrigin,
		Access: []config.UIAccess{
			{GitHubID: 1, Role: config.UIRoleViewer},
			{GitHubID: 2, Role: config.UIRoleEditor},
			{GitHubUser: "root-user", Role: config.UIRoleAdmin},
		},
		Session: config.UISession{IdleTimeout: time.Hour, MaxLifetime: 24 * time.Hour},
	}
}

func newSessEnv(t *testing.T, mod ...func(*api.Options)) *sessEnv {
	t.Helper()
	e := &sessEnv{idp: &fakeIDP{}}
	withUI := func(o *api.Options) {
		e.sessions = webauth.NewSessions(o.Store, sessUI(), nil, webauth.WithClock(o.Now))
		h, err := webauth.NewHandler(webauth.HandlerOptions{
			Providers: []webauth.IdentityProvider{e.idp}, Sessions: e.sessions, UI: sessUI(), Now: o.Now,
		})
		if err != nil {
			t.Fatal(err)
		}
		e.handler = h
		o.Sessions, o.SessionCookie, o.PublicOrigin, o.UIAuth = e.sessions, webauth.SessionCookie, sessOrigin, h
	}
	e.tokEnv = newTokEnv(t, append([]func(*api.Options){withUI}, mod...)...)
	return e
}

// signIn runs the browser sign-in through the handler that the api role
// mounts at api.UIAuthPath, and returns the
// session cookie's value.
func (e *sessEnv) signIn(who webauth.Identity) string {
	e.t.Helper()
	e.idp.who = who
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/auth/login/github", nil))
	loc, _ := url.Parse(rec.Header().Get("Location"))
	var oauth *http.Cookie
	for _, c := range rec.Result().Cookies() {
		oauth = c
	}
	if rec.Code != http.StatusFound || oauth == nil {
		e.t.Fatalf("login: %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/ui/auth/callback/github?code=c&state="+url.QueryEscape(loc.Query().Get("state")), nil)
	req.Header.Add("Cookie", oauth.Name+"="+oauth.Value)
	rec = httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == webauth.SessionCookie && c.Value != "" {
			return c.Value
		}
	}
	e.t.Fatalf("callback: %d %s", rec.Code, rec.Body)
	return ""
}

// doCookie sends a request with the session cookie, and the CSRF headers
// when csrf is set.
func (e *sessEnv) doCookie(method, path, cookie string, body any, csrf bool) res {
	e.t.Helper()
	hdr := []string{"Cookie", webauth.SessionCookie + "=" + cookie}
	if csrf {
		hdr = append(hdr, "Origin", sessOrigin, api.CSRFHeader, "1")
	}
	return e.do(method, path, "", body, hdr...)
}

var (
	viewerID = webauth.Identity{Provider: webauth.GitHubName, Subject: "1", Login: "viewer-user"}
	editorID = webauth.Identity{Provider: webauth.GitHubName, Subject: "2", Login: "editor-user"}
	adminID  = webauth.Identity{Provider: webauth.GitHubName, Subject: "3", Login: "root-user"}
)

// TestSessionEveryRoute runs every operation of the specification on real
// sessions: a role below the operation's scope gets 403, its own and above
// neither 401 nor 403; a change without the CSRF headers is 403 csrf; a
// malformed or unknown cookie is 401.
func TestSessionEveryRoute(t *testing.T) {
	e := newSessEnv(t)
	api.StopStreams(e.srv)
	cookies := map[api.Scope]string{
		api.ScopeRead: e.signIn(viewerID), api.ScopeWrite: e.signIn(editorID), api.ScopeAdmin: e.signIn(adminID),
	}
	_, unknownCookie := webauth.NewCookie(e.clock.now())
	for _, op := range api.Operations(e.srv) {
		t.Run(op.ID, func(t *testing.T) {
			path := concretePath(op)
			var body any
			if op.HasBody {
				body = map[string]any{}
			}
			safe := op.Method == http.MethodGet || op.Method == http.MethodHead
			for name, c := range map[string]string{"malformed": "nope", "unknown": unknownCookie} {
				r := e.doCookie(op.Method, path, c, body, true)
				if r.status != http.StatusUnauthorized {
					t.Fatalf("%s cookie: %d %s", name, r.status, r.body)
				}
			}
			if !safe {
				r := e.doCookie(op.Method, path, cookies[api.ScopeAdmin], body, false)
				if r.status != http.StatusForbidden || !strings.HasSuffix(r.json(t)["type"].(string), "csrf") {
					t.Fatalf("no CSRF headers: %d %s", r.status, r.body)
				}
			}
			for sc := api.ScopeRead; sc < op.Scope; sc++ {
				r := e.doCookie(op.Method, path, cookies[sc], body, true)
				if r.status != http.StatusForbidden || r.json(t)["required_scope"] != op.Scope.String() {
					t.Fatalf("scope %s: %d %s", sc, r.status, r.body)
				}
			}
			for sc := op.Scope; sc <= api.ScopeAdmin; sc++ {
				r := e.doCookie(op.Method, path, cookies[sc], body, true)
				if r.status == http.StatusUnauthorized || r.status == http.StatusForbidden {
					t.Fatalf("scope %s: %d %s", sc, r.status, r.body)
				}
			}
		})
	}
}

// A session ends at its idle timeout, its maximum lifetime and its
// revocation by an admin; GET /v1/me describes it; sign-out ends it.
func TestSessionLifecycle(t *testing.T) {
	e := newSessEnv(t)
	cookie := e.signIn(adminID)
	me := e.must(http.StatusOK, e.doCookie("GET", "/v1/me", cookie, nil, false)).json(t)
	user, _ := me["user"].(map[string]any)
	if me["principal_id"] != "github:3" || me["kind"] != "session" || me["role"] != "admin" || me["scope"] != "admin" || user["login"] != "root-user" {
		t.Fatalf("%v", me)
	}
	// The admin token sees the session and revokes it.
	page := e.must(http.StatusOK, e.do("GET", "/v1/sessions", e.tokens[api.ScopeAdmin], nil)).json(t)
	items, _ := page["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("%v", page)
	}
	id := items[0].(map[string]any)["id"].(string)
	e.must(http.StatusNoContent, e.do("DELETE", "/v1/sessions/"+id, e.tokens[api.ScopeAdmin], nil))
	e.must(http.StatusUnauthorized, e.doCookie("GET", "/v1/me", cookie, nil, false))

	// Idle: an hour without a request.
	cookie = e.signIn(editorID)
	e.clock.add(59 * time.Minute)
	e.must(http.StatusOK, e.doCookie("GET", "/v1/me", cookie, nil, false))
	e.clock.add(59 * time.Minute)
	e.must(http.StatusOK, e.doCookie("GET", "/v1/me", cookie, nil, false))
	e.clock.add(time.Hour)
	e.must(http.StatusUnauthorized, e.doCookie("GET", "/v1/me", cookie, nil, false))

	// Maximum lifetime, however active.
	cookie = e.signIn(viewerID)
	for range 30 {
		e.clock.add(55 * time.Minute)
		if r := e.doCookie("GET", "/v1/me", cookie, nil, false); r.status != http.StatusOK {
			break
		}
	}
	e.must(http.StatusUnauthorized, e.doCookie("GET", "/v1/me", cookie, nil, false))

	// Sign-out by the session itself.
	cookie = e.signIn(adminID)
	req := httptest.NewRequest(http.MethodPost, "/ui/auth/logout", nil)
	req.Header.Set("Origin", sessOrigin)
	req.Header.Set(api.CSRFHeader, "1")
	req.Header.Add("Cookie", webauth.SessionCookie+"="+cookie)
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	e.must(http.StatusUnauthorized, e.doCookie("GET", "/v1/me", cookie, nil, false))

	// The access log names the reason and never the cookie.
	logs := e.log.String()
	if strings.Contains(logs, cookie) || !strings.Contains(logs, `"auth_failure":"revoked"`) || !strings.Contains(logs, `"auth_failure":"expired"`) {
		t.Fatal(logs)
	}
}

// A session admin's token is created by its principal id.
func TestSessionCreatesToken(t *testing.T) {
	e := newSessEnv(t)
	cookie := e.signIn(adminID)
	r := e.must(http.StatusCreated, e.doCookie("POST", "/v1/tokens", cookie, map[string]any{"name": "ci", "scope": "read"}, true)).json(t)
	if r["created_by"] != "github:3" {
		t.Fatalf("%v", r)
	}
}
