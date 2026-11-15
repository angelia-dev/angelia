package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/api"
	"github.com/angelia-dev/angelia/internal/apitoken"
	"github.com/angelia-dev/angelia/internal/apitoken/format"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/githubrelease"
	"github.com/angelia-dev/angelia/internal/source/pypi"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
)

// Tests on real API tokens: the server with the authenticator of
// internal/apitoken over a SQLite store. Until the bootstrap (72-E) lands,
// the first tokens go straight into the store, as `token create --local`
// will put them there.

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// clock is a settable clock shared by the server and the authenticator.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type tokEnv struct {
	t      *testing.T
	st     *sqlite.Store
	srv    *api.Server
	auth   *apitoken.Authenticator
	log    *logBuf
	clock  *clock
	tokens map[api.Scope]string // one stored token per scope
	ids    map[api.Scope]string
}

func newTokEnv(t *testing.T, mod ...func(*api.Options)) *tokEnv {
	t.Helper()
	st, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "api.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	reg := source.NewRegistry()
	for _, s := range []source.Source{pypi.New(pypi.DefaultBaseURL, &http.Client{}), githubrelease.New(githubrelease.DefaultBaseURL, &http.Client{})} {
		if err := reg.Register(s); err != nil {
			t.Fatal(err)
		}
	}
	e := &tokEnv{
		t: t, st: st, log: &logBuf{}, clock: &clock{t: time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)},
		tokens: map[api.Scope]string{}, ids: map[api.Scope]string{},
	}
	e.auth = apitoken.NewAuthenticator(st, nil, apitoken.WithClock(e.clock.now))
	for _, sc := range []api.Scope{api.ScopeRead, api.ScopeWrite, api.ScopeAdmin} {
		e.ids[sc], e.tokens[sc] = e.store(sc.String(), sc.String(), nil)
	}
	opts := api.Options{
		Store: st, Auth: e.auth, Sources: reg,
		Logger: slog.New(slog.NewJSONHandler(e.log, nil)), Now: e.clock.now,
	}
	for _, m := range mod {
		m(&opts)
	}
	e.srv, err = api.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// store writes a token straight into the store and returns its id and token.
func (e *tokEnv) store(name, scope string, expires *time.Time) (string, string) {
	e.t.Helper()
	id, tok := apitoken.Generate(e.clock.now())
	if err := e.st.CreateAPIToken(context.Background(), store.APIToken{
		ID: id, Name: name, Scope: scope, Hash: apitoken.Hash(tok), CreatedAt: e.clock.now(), CreatedBy: "local", ExpiresAt: expires,
	}); err != nil {
		e.t.Fatal(err)
	}
	return id, tok
}

type res struct {
	status int
	header http.Header
	body   []byte
}

func (r res) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("%v: %s", err, r.body)
	}
	return m
}

func (e *tokEnv) do(method, path, token string, body any, hdr ...string) res {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	return res{rec.Code, rec.Header(), rec.Body.Bytes()}
}

func (e *tokEnv) must(want int, r res) res {
	e.t.Helper()
	if r.status != want {
		e.t.Fatalf("status %d, want %d: %s", r.status, want, r.body)
	}
	return r
}

// accessLog returns the access log records.
func (e *tokEnv) accessLog() []map[string]any {
	e.t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(e.log.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["msg"] == "api request" {
			out = append(out, m)
		}
	}
	return out
}

// concretePath fills the path parameters of an operation with valid values
// that name no existing resource.
func concretePath(op api.OpInfo) string {
	r := strings.NewReplacer("{name}", "does-not-exist", "{id}", "01JBX2A7C8D9E0F1G2H3J4K5M6",
		"{interface}", "source", "{kind}", "nope")
	return r.Replace(op.Path)
}

// TestAuthEveryRoute runs every operation of the specification on real API
// tokens: with no token, a malformed one, a well-formed unknown one, an
// admin token's id with a wrong secret, a revoked and an expired admin
// token, Basic auth (all 401); with each scope
// below its own (403 before anything else, so before 404); and with its own
// scope and above (neither 401 nor 403).
func TestAuthEveryRoute(t *testing.T) {
	ops := api.Operations(newTokEnv(t).srv)
	if len(ops) < 35 {
		t.Fatalf("only %d operations", len(ops))
	}
	for _, op := range ops {
		t.Run(op.ID, func(t *testing.T) {
			e := newTokEnv(t)
			api.StopStreams(e.srv) // nothing may run: the stream would block
			path := concretePath(op)
			var body any
			if op.HasBody {
				body = map[string]any{}
			}
			r := e.do(op.Method, path, "", body)
			if r.status != http.StatusUnauthorized || r.header.Get("WWW-Authenticate") != `Bearer realm="angelia"` {
				t.Fatalf("no token: %d %v", r.status, r.header)
			}
			_, unknown := apitoken.Generate(e.clock.now())
			revokedID, revoked := e.store("revoked-admin", "admin", nil)
			if _, err := e.st.RevokeAPIToken(context.Background(), revokedID, e.clock.now(), "local"); err != nil {
				t.Fatal(err)
			}
			past := e.clock.now().Add(-time.Second)
			_, expired := e.store("expired-admin", "admin", &past)
			for name, tok := range map[string]string{
				"malformed": "not-a-known-token", "unknown": unknown, "revoked": revoked, "expired": expired,
				// the admin token's id with another secret: the hash does not match
				"wrong secret": format.Format(e.ids[api.ScopeAdmin], [32]byte{1}),
			} {
				r := e.do(op.Method, path, tok, body)
				if r.status != http.StatusUnauthorized || !strings.Contains(r.header.Get("WWW-Authenticate"), `error="invalid_token"`) {
					t.Fatalf("%s token: %d %v", name, r.status, r.header)
				}
			}
			if r := e.do(op.Method, path, "", body, "Authorization", "Basic dXNlcjpwYXNz"); r.status != http.StatusUnauthorized {
				t.Fatalf("basic auth: %d", r.status)
			}
			for sc := api.ScopeRead; sc < op.Scope; sc++ {
				r := e.do(op.Method, path, e.tokens[sc], body)
				if r.status != http.StatusForbidden || r.json(t)["required_scope"] != op.Scope.String() {
					t.Fatalf("scope %s: %d %s", sc, r.status, r.body)
				}
			}
			for sc := op.Scope; sc <= api.ScopeAdmin; sc++ {
				r := e.do(op.Method, path, e.tokens[sc], body)
				if r.status == http.StatusUnauthorized || r.status == http.StatusForbidden {
					t.Fatalf("scope %s: %d %s", sc, r.status, r.body)
				}
				if !op.Statuses[r.status] && r.status != http.StatusBadRequest {
					t.Errorf("scope %s: status %d is not declared for %s: %s", sc, r.status, op.ID, r.body)
				}
			}
		})
	}
}

// sse reads an event stream line by line.
type sse struct {
	t     *testing.T
	lines chan string
}

func openSSE(t *testing.T, url, token string) *sse {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: %d", resp.StatusCode)
	}
	s := &sse{t: t, lines: make(chan string, 100)}
	go func() {
		defer close(s.lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			s.lines <- sc.Text()
		}
	}()
	return s
}

// next returns the next non-empty line, or closed at the end of the stream.
// The timeout only stops a broken test; nothing waits for it to pass.
func (s *sse) next() (string, bool) {
	s.t.Helper()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case l, ok := <-s.lines:
			if !ok {
				return "", true
			}
			if l != "" {
				return l, false
			}
		case <-timeout.C:
			s.t.Fatal("the stream neither sent a line nor closed")
		}
	}
}

// AC (end to end): an admin token creates a token through the API; the new
// token works, also for an event stream; the admin revokes it; the next
// request with it is 401 and the open stream closes at its next re-check
// (an injected 10 ms interval instead of 60 s; no sleeps).
func TestTokenLifecycle(t *testing.T) {
	e := newTokEnv(t, func(o *api.Options) {
		o.StreamPoll = 10 * time.Millisecond
		o.KeepAlive = time.Hour
		o.AuthRecheck = 10 * time.Millisecond
	})
	ts := httptest.NewServer(e.srv)
	t.Cleanup(ts.Close)
	admin, adminID := e.tokens[api.ScopeAdmin], e.ids[api.ScopeAdmin]

	r := e.must(http.StatusCreated, e.do("POST", "/v1/tokens", admin, map[string]any{"name": "agent", "scope": "read"}))
	created := r.json(t)
	id, tok := created["id"].(string), created["token"].(string)
	if created["created_by"] != adminID || r.header.Get("Location") != "/v1/tokens/"+id {
		t.Fatalf("created: %v %v", created, r.header)
	}
	e.must(http.StatusOK, e.do("GET", "/v1/watches", tok, nil))
	if r := e.do("PUT", "/v1/watches/x", tok, map[string]any{}); r.status != http.StatusForbidden {
		t.Fatalf("a read token writes: %d", r.status)
	}
	if r := e.do("GET", "/v1/tokens", tok, nil); r.status != http.StatusForbidden {
		t.Fatalf("a read token lists tokens: %d", r.status)
	}
	recs := e.accessLog()
	if last := recs[len(recs)-1]; last["principal"] != id || last["token_name"] != "agent" {
		t.Errorf("access log: %v", last)
	}

	stream := openSSE(t, ts.URL+"/v1/events/stream", tok)
	if l, closed := stream.next(); closed || l != "retry: 5000" {
		t.Fatalf("stream start: %q %v", l, closed)
	}

	e.must(http.StatusNoContent, e.do("DELETE", "/v1/tokens/"+id, admin, nil))
	r = e.do("GET", "/v1/watches", tok, nil)
	if r.status != http.StatusUnauthorized {
		t.Fatalf("revoked token: %d %s", r.status, r.body)
	}
	recs = e.accessLog()
	if last := recs[len(recs)-1]; last["auth_failure"] != "revoked" || last["token_id"] != id || last["principal"] != nil {
		t.Errorf("access log of the revoked token: %v", last)
	}
	for {
		l, closed := stream.next()
		if closed {
			break
		}
		if !strings.HasPrefix(l, ":") {
			t.Fatalf("stream after revocation: %q", l)
		}
	}

	got := e.must(http.StatusOK, e.do("GET", "/v1/tokens/"+id, admin, nil)).json(t)
	if got["revoked_by"] != adminID || got["revoked_at"] == nil {
		t.Errorf("revoked token: %v", got)
	}
	// The last use reaches the store with the flush.
	if err := e.auth.Flush(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if got := e.must(http.StatusOK, e.do("GET", "/v1/tokens/"+id, admin, nil)).json(t); got["last_used_at"] == nil {
		t.Errorf("last_used_at not recorded: %v", got)
	}
	if strings.Contains(e.log.String(), tok) {
		t.Error("the token reached the log")
	}
}

// A token past its expires_at is 401 like an unknown one; the log says
// expired, with its id.
func TestTokenExpiry(t *testing.T) {
	e := newTokEnv(t)
	exp := e.clock.now().Add(time.Hour)
	r := e.must(http.StatusCreated, e.do("POST", "/v1/tokens", e.tokens[api.ScopeAdmin], map[string]any{
		"name": "short", "scope": "write", "expires_at": exp.Format("2006-01-02T15:04:05.000Z"),
	}))
	id, tok := r.json(t)["id"].(string), r.json(t)["token"].(string)
	e.must(http.StatusOK, e.do("GET", "/v1/watches", tok, nil))
	e.clock.add(time.Hour)
	e.must(http.StatusUnauthorized, e.do("GET", "/v1/watches", tok, nil))
	recs := e.accessLog()
	if last := recs[len(recs)-1]; last["auth_failure"] != "expired" || last["token_id"] != id {
		t.Errorf("access log: %v", last)
	}
	got := e.must(http.StatusOK, e.do("GET", "/v1/tokens", e.tokens[api.ScopeAdmin], nil)).json(t)
	if n := len(got["items"].([]any)); n != 4 {
		t.Errorf("an expired token is listed: %d items, want 4", n)
	}
}

// A token may revoke itself; the request that revokes it still completes.
func TestTokenRevokesItself(t *testing.T) {
	e := newTokEnv(t)
	admin, id := e.tokens[api.ScopeAdmin], e.ids[api.ScopeAdmin]
	e.must(http.StatusNoContent, e.do("DELETE", "/v1/tokens/"+id, admin, nil))
	e.must(http.StatusUnauthorized, e.do("GET", "/v1/tokens/"+id, admin, nil))
	row, err := e.st.APITokenByID(context.Background(), id)
	if err != nil || row.RevokedBy != id {
		t.Fatalf("%+v %v", row, err)
	}
}
