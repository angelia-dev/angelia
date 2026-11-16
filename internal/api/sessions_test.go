package api

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// Synthetic session cookies of the fake session authenticator.
const (
	testSessionCookie = "angelia_session"
	testOrigin        = "https://angelia.example.com"
	tokMe             = "test-me-token-00000000000000005"
	sessMe            = "angs_test-session-admin"
	sessRead          = "angs_test-session-viewer"
)

// fakeSessions accepts a fixed set of cookies, compared in constant time.
type fakeSessions struct {
	mu       sync.Mutex
	sessions map[string]Principal
	calls    int
	checks   int
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{sessions: map[string]Principal{
		sessMe: {ID: "github:139323", Name: "mmornati", Scope: ScopeAdmin, Session: &SessionInfo{
			ID: "01JD0B6Q1W4E7R9T2Y5V8X0Z3A", Provider: "github", Subject: "139323", Login: "mmornati",
			Name: "Marco Mornati", AvatarURL: "https://avatars.githubusercontent.com/u/139323?v=4", Role: "admin",
		}},
		sessRead: {ID: "github:583231", Name: "octocat", Scope: ScopeRead, Session: &SessionInfo{
			ID: "01JD08F2K5M8P1R4T7W0Y3B6C9", Provider: "github", Subject: "583231", Login: "octocat", Role: "viewer",
		}},
	}}
}

func (f *fakeSessions) AuthenticateSession(_ context.Context, cookie string) (Principal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	var found Principal
	ok := false
	for c, p := range f.sessions {
		if subtle.ConstantTimeCompare([]byte(c), []byte(cookie)) == 1 {
			found, ok = p, true
		}
	}
	if !ok {
		return Principal{}, fmt.Errorf("session: %w", ErrUnauthenticated)
	}
	return found, nil
}

func (f *fakeSessions) CheckSession(ctx context.Context, cookie string) (Principal, error) {
	p, err := f.AuthenticateSession(ctx, cookie)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls--
	f.checks++
	return p, err
}

func (f *fakeSessions) checkCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks
}

func (f *fakeSessions) IdleTimeout() time.Duration { return 12 * time.Hour }

func (f *fakeSessions) revoke(cookie string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, cookie)
}

func (f *fakeSessions) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// withSessions turns the session cookie on, with the fake authenticator.
func withSessions(f *fakeSessions) func(*Options) {
	return func(o *Options) {
		o.Sessions, o.SessionCookie, o.PublicOrigin = f, testSessionCookie, testOrigin
	}
}

// seedSessions stores the sessions of the SessionPage example.
func seedSessions(c *contract) {
	c.t.Helper()
	ex := c.example("SessionPage").(map[string]any)
	at := func(v any) time.Time {
		tm, err := time.Parse(time.RFC3339Nano, v.(string))
		if err != nil {
			c.t.Fatal(err)
		}
		return tm
	}
	str := func(v any) string {
		s, _ := v.(string)
		return s
	}
	for i, it := range ex["items"].([]any) {
		m := it.(map[string]any)
		u := m["user"].(map[string]any)
		row := store.UISession{
			ID: m["id"].(string), Hash: []byte(strings.Repeat(fmt.Sprint(i), 32)), Provider: u["provider"].(string),
			Subject: u["subject"].(string), Login: u["login"].(string), Name: str(u["name"]), AvatarURL: str(u["avatar_url"]),
			UserAgent: str(m["user_agent"]), CreatedAt: at(m["created_at"]), LastSeenAt: at(m["last_seen_at"]),
			ExpiresAt: at(m["expires_at"]),
		}
		if err := c.e.st.CreateUISession(context.Background(), row); err != nil {
			c.t.Fatal(err)
		}
		if m["revoked_at"] != nil {
			if _, err := c.e.st.RevokeUISession(context.Background(), row.ID, at(m["revoked_at"]), m["revoked_by"].(string)); err != nil {
				c.t.Fatal(err)
			}
		}
	}
}

func cookieHdr(v string) []string { return []string{"Cookie", testSessionCookie + "=" + v} }

func csrfHdr(v string) []string {
	return append(cookieHdr(v), "Origin", testOrigin, CSRFHeader, "1")
}

// A session cookie authenticates a request without an Authorization header,
// with the session's scope; the scope checks are the tokens'.
func TestSessionCookieAuthenticates(t *testing.T) {
	f := newFakeSessions()
	e := newEnv(t, withSessions(f))
	r := e.must(http.StatusOK, e.do("GET", "/v1/me", "", nil, cookieHdr(sessRead)...))
	if m := r.json(t); m["kind"] != "session" || m["principal_id"] != "github:583231" || m["role"] != "viewer" || m["scope"] != "read" {
		t.Fatalf("me: %v", m)
	}
	r = e.do("GET", "/v1/tokens", "", nil, cookieHdr(sessRead)...)
	if r.Status != http.StatusForbidden || r.problemType(t) != SlugForbidden || !strings.Contains(string(r.Body), "the session has read") {
		t.Fatalf("viewer on an admin operation: %d %s", r.Status, r.Body)
	}
	e.must(http.StatusOK, e.do("GET", "/v1/tokens", "", nil, cookieHdr(sessMe)...))
	r = e.do("GET", "/v1/me", "", nil, cookieHdr("angs_unknown")...)
	if r.Status != http.StatusUnauthorized || !strings.Contains(r.Header.Get("WWW-Authenticate"), "invalid_token") {
		t.Fatalf("unknown cookie: %d %v", r.Status, r.Header)
	}
	// Two cookies of the name: malformed, and the authenticator is not asked.
	n := f.callCount()
	r = e.do("GET", "/v1/me", "", nil, "Cookie", testSessionCookie+"="+sessMe+"; "+testSessionCookie+"="+sessRead)
	if r.Status != http.StatusUnauthorized || f.callCount() != n {
		t.Fatalf("two cookies: %d", r.Status)
	}
	if !strings.Contains(e.log.String(), `"auth_failure":"malformed"`) {
		t.Fatalf("two cookies are not logged as malformed: %s", e.log.String())
	}
	// Another cookie name is no credential.
	r = e.do("GET", "/v1/me", "", nil, "Cookie", "other="+sessMe)
	if r.Status != http.StatusUnauthorized || r.Header.Get("WWW-Authenticate") != `Bearer realm="angelia"` {
		t.Fatalf("other cookie: %d %v", r.Status, r.Header)
	}
	if strings.Contains(e.log.String(), sessMe) {
		t.Fatal("a cookie value reached the log")
	}
}

// The Authorization header wins over the cookie, even a bad one.
func TestBearerWinsOverCookie(t *testing.T) {
	f := newFakeSessions()
	e := newEnv(t, withSessions(f))
	r := e.must(http.StatusOK, e.do("GET", "/v1/me", tokRead, nil, cookieHdr(sessMe)...))
	if m := r.json(t); m["kind"] != "token" || m["principal_id"] != "tok-read" || m["role"] != nil || m["user"] != nil {
		t.Fatalf("me: %v", m)
	}
	if r := e.do("GET", "/v1/me", "bad-token", nil, cookieHdr(sessMe)...); r.Status != http.StatusUnauthorized {
		t.Fatalf("a bad bearer with a good cookie: %d", r.Status)
	}
	if f.callCount() != 0 {
		t.Fatal("the cookie was read although a bearer token was sent")
	}
}

// Without a SessionAuthenticator, cookies are ignored.
func TestCookieIgnoredWithoutSessions(t *testing.T) {
	e := newEnv(t)
	if r := e.do("GET", "/v1/me", "", nil, cookieHdr(sessMe)...); r.Status != http.StatusUnauthorized {
		t.Fatalf("%d", r.Status)
	}
}

// Unsafe methods with the cookie need Origin and X-Angelia-CSRF: 1; GET does
// not; bearer requests never do. A refused request does not reach the
// session authenticator.
func TestSessionCSRF(t *testing.T) {
	f := newFakeSessions()
	e := newEnv(t, withSessions(f))
	path := "/v1/sessions/01JBX2A7C8D9E0F1G2H3J4K5M6"
	for name, hdr := range map[string][]string{
		"no headers":       cookieHdr(sessMe),
		"no csrf header":   append(cookieHdr(sessMe), "Origin", testOrigin),
		"no origin":        append(cookieHdr(sessMe), CSRFHeader, "1"),
		"other origin":     append(cookieHdr(sessMe), "Origin", "https://evil.example", CSRFHeader, "1"),
		"origin with path": append(cookieHdr(sessMe), "Origin", testOrigin+"/", CSRFHeader, "1"),
		"csrf not 1":       append(cookieHdr(sessMe), "Origin", testOrigin, CSRFHeader, "true"),
		"unknown cookie":   cookieHdr("angs_unknown"),
	} {
		n := f.callCount()
		r := e.do("DELETE", path, "", nil, hdr...)
		if r.Status != http.StatusForbidden || r.problemType(t) != SlugCSRF {
			t.Errorf("%s: %d %s", name, r.Status, r.Body)
		}
		if f.callCount() != n {
			t.Errorf("%s: the session was authenticated", name)
		}
	}
	// Two Origin headers are refused too.
	req, _ := http.NewRequest("DELETE", path, nil)
	req.Header.Set("Cookie", testSessionCookie+"="+sessMe)
	req.Header.Add("Origin", testOrigin)
	req.Header.Add("Origin", testOrigin)
	req.Header.Set(CSRFHeader, "1")
	if CSRFOK(req, testOrigin) || CSRFOK(req, "") {
		t.Error("two Origin headers pass")
	}
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE", "PROPFIND"} {
		req.Method = m
		if CSRFOK(req, testOrigin) {
			t.Errorf("%s passes without a valid header set", m)
		}
	}
	req.Method = "HEAD"
	if !CSRFOK(req, testOrigin) {
		t.Error("HEAD needs no CSRF check")
	}
	if r := e.do("DELETE", path, "", nil, csrfHdr(sessMe)...); r.Status != http.StatusNotFound {
		t.Fatalf("with both headers: %d %s", r.Status, r.Body)
	}
	if r := e.do("DELETE", path, "", nil, csrfHdr(sessRead)...); r.Status != http.StatusForbidden || r.problemType(t) != SlugForbidden {
		t.Fatalf("a viewer with both headers: %d %s", r.Status, r.Body)
	}
	if r := e.do("DELETE", path, tokAdmin, nil); r.Status != http.StatusNotFound {
		t.Fatalf("bearer without CSRF headers: %d %s", r.Status, r.Body)
	}
	e.must(http.StatusOK, e.do("GET", "/v1/sessions", "", nil, cookieHdr(sessMe)...))
	// A write with a body, through the whole pipeline.
	e.must(http.StatusCreated, e.do("PUT", "/v1/watches/requests", "", watchPut, csrfHdr(sessMe)...))
	if !strings.Contains(e.log.String(), `"principal":"github:139323"`) {
		t.Fatal("the access log does not name the session principal")
	}
}

// /mcp never accepts the cookie, nor does an MCP tool's in-memory REST call.
func TestMCPIgnoresCookie(t *testing.T) {
	f := newFakeSessions()
	stub := &stubMCP{}
	e := newEnv(t, withSessions(f), func(o *Options) { o.MCP = stub.factory })
	r := e.do("POST", MCPPath, "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, csrfHdr(sessMe)...)
	if r.Status != http.StatusUnauthorized || stub.calls() != 0 {
		t.Fatalf("/mcp with a cookie: %d %s", r.Status, r.Body)
	}
	inner := 0
	stub.do = func(w http.ResponseWriter, r *http.Request, rest http.RoundTripper) {
		req, _ := http.NewRequestWithContext(r.Context(), "GET", "http://in-process/v1/me", nil)
		req.Header.Set("Cookie", testSessionCookie+"="+sessMe)
		resp, err := rest.RoundTrip(req)
		if err == nil {
			inner = resp.StatusCode
			_ = resp.Body.Close()
		}
		w.WriteHeader(http.StatusOK)
	}
	e.must(http.StatusOK, e.do("POST", MCPPath, tokRead, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if inner != http.StatusUnauthorized {
		t.Fatalf("in-memory call with a cookie only: %d", inner)
	}
	if f.callCount() != 0 {
		t.Fatal("the cookie was read")
	}
}

// A path segment equal to the request's cookie is redacted in the log.
func TestSafePathRedactsCookie(t *testing.T) {
	if got := safePath("/v1/watches/"+sessMe, "", sessMe); got != "/v1/watches/[redacted]" {
		t.Fatalf("%s", got)
	}
	if got := safePath("/v1/watches/a", "", ""); got != "/v1/watches/a" {
		t.Fatalf("%s", got)
	}
}

// An event stream opened with the cookie re-checks the session, like a
// token: once the session stops authenticating, the stream closes. The
// re-checks do not record a use (CheckSession): only the opening request
// counts, so an open stream does not keep an idle session alive.
func TestEventStreamSessionRecheck(t *testing.T) {
	f := newFakeSessions()
	_, ts := streamEnv(t, withSessions(f))
	c := openStream(t, ts.URL+"/v1/events/stream", "", cookieHdr(sessRead)...)
	if c.resp.StatusCode != http.StatusOK {
		t.Fatalf("%d", c.resp.StatusCode)
	}
	c.expect("retry: 5000")
	n := f.callCount()
	f.revoke(sessRead)
	c.expectClosed()
	if f.checkCount() == 0 {
		t.Fatal("the stream did not check the session again")
	}
	if f.callCount() != n {
		t.Fatalf("the stream's re-check recorded a use: %d authentications, want %d", f.callCount(), n)
	}
}

// ListSessions refuses a cursor of another list.
func TestListSessionsCursor(t *testing.T) {
	e := newEnv(t)
	other := *encodeCursor(pageCursor{K: cursorTokens, I: "x"})
	if r := e.do("GET", "/v1/sessions?cursor="+other, tokAdmin, nil); r.Status != http.StatusBadRequest {
		t.Fatalf("%d", r.Status)
	}
}
