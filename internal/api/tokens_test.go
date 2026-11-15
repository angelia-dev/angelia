package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/apitoken/format"
	"github.com/angelia-dev/angelia/internal/store"
)

// adminULID is the id of an admin principal shaped like a real token's,
// so that created_by and revoked_by validate against TokenActor.
const (
	adminULID = "01JCZ7P0N4D8E1B5XJ2Q6S3TGH"
	tokULID   = "test-admin-ulid-token-0000000004"
)

// tokenEnv is an env with an admin principal whose id is a ULID.
func tokenEnv(t *testing.T, mod ...func(*Options)) *env {
	t.Helper()
	e := newEnv(t, mod...)
	e.auth.mu.Lock()
	e.auth.tokens[tokULID] = Principal{ID: adminULID, Name: "ops", Scope: ScopeAdmin}
	e.auth.mu.Unlock()
	return e
}

// logRecords decodes the JSON log lines.
func (e *env) logRecords() []map[string]any {
	e.t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(e.log.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			e.t.Fatalf("log line %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

func errorPointers(t *testing.T, r result) []string {
	t.Helper()
	var ps []string
	for _, x := range r.json(t)["errors"].([]any) {
		ps = append(ps, x.(map[string]any)["pointer"].(string))
	}
	slices.Sort(ps)
	return slices.Compact(ps)
}

// AC: the name pattern, the scope, a future expires_at and an active-name
// clash are each a 422 with a pointer to the member; every error is listed.
func TestCreateTokenValidation(t *testing.T) {
	e := tokenEnv(t)
	past := e.now.Add(-time.Minute).Format(timestampLayout)
	for _, tc := range []struct {
		body any
		want []string
	}{
		{map[string]any{"name": "Grafana", "scope": "read"}, []string{"/name"}},
		{map[string]any{"name": "-x", "scope": "read"}, []string{"/name"}},
		{map[string]any{"name": strings.Repeat("a", 64), "scope": "read"}, []string{"/name"}},
		{map[string]any{"name": "grafana", "scope": "root"}, []string{"/scope"}},
		{map[string]any{"name": "grafana", "scope": "read", "expires_at": past}, []string{"/expires_at"}},
		{map[string]any{"name": "grafana", "scope": "read", "expires_at": e.now.Format(timestampLayout)}, []string{"/expires_at"}},
		{map[string]any{"name": "grafana", "scope": "read", "expires_at": "tomorrow"}, []string{"/expires_at"}},
		{map[string]any{"name": "Grafana", "scope": "read", "expires_at": past}, []string{"/expires_at", "/name"}},
		{map[string]any{"name": "grafana", "scope": "read", "token": "x"}, []string{"/token"}},
	} {
		r := e.do("POST", "/v1/tokens", tokULID, tc.body)
		if r.Status != http.StatusUnprocessableEntity || r.problemType(t) != SlugValidationFailed {
			t.Fatalf("%v: %d %s", tc.body, r.Status, r.Body)
		}
		if got := errorPointers(t, r); !slices.Equal(got, tc.want) {
			t.Errorf("%v: pointers %v, want %v (%s)", tc.body, got, tc.want, r.Body)
		}
	}
	if n, _ := e.st.CountAPITokens(context.Background()); n != 0 {
		t.Fatalf("%d tokens stored by refused requests", n)
	}

	// An active name clashes; a revoked one does not.
	r := e.must(http.StatusCreated, e.do("POST", "/v1/tokens", tokULID, map[string]any{"name": "grafana", "scope": "read"}))
	first := r.json(t)["id"].(string)
	r = e.do("POST", "/v1/tokens", tokULID, map[string]any{"name": "grafana", "scope": "admin"})
	if r.Status != http.StatusUnprocessableEntity || !slices.Equal(errorPointers(t, r), []string{"/name"}) {
		t.Fatalf("name clash: %d %s", r.Status, r.Body)
	}
	e.must(http.StatusNoContent, e.do("DELETE", "/v1/tokens/"+first, tokULID, nil))
	e.must(http.StatusCreated, e.do("POST", "/v1/tokens", tokULID, map[string]any{"name": "grafana", "scope": "read"}))
}

// AC: the secret appears only in the 201 body, with a Location header; the
// store holds its hash; created_by is the caller's token id; GET and the
// list never show the token or its hash.
func TestCreateTokenSecretOnce(t *testing.T) {
	e := tokenEnv(t)
	exp := e.now.Add(90 * 24 * time.Hour)
	r := e.must(http.StatusCreated, e.do("POST", "/v1/tokens", tokULID, map[string]any{
		"name": "ci", "scope": "write", "expires_at": exp.Format(timestampLayout),
	}))
	body := r.json(t)
	id, tok := body["id"].(string), body["token"].(string)
	if pid, err := format.Parse(tok); err != nil || pid != id {
		t.Fatalf("token %q does not parse to id %s: %v", tok, id, err)
	}
	if got := r.Header.Get("Location"); got != "/v1/tokens/"+id {
		t.Errorf("Location = %q", got)
	}
	if r.Header.Get("Angelia-Change") != "" {
		t.Errorf("Angelia-Change on a create: %v", r.Header)
	}
	if got := r.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q on the response that carries the token", got)
	}
	if body["created_by"] != adminULID || body["scope"] != "write" || body["name"] != "ci" ||
		body["expires_at"] != exp.Format(timestampLayout) || body["revoked_at"] != nil || body["last_used_at"] != nil {
		t.Errorf("created body: %s", r.Body)
	}
	row, err := e.st.APITokenByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if h := format.Hash(tok); string(row.Hash) != string(h[:]) || row.CreatedBy != adminULID || row.ExpiresAt == nil || !row.ExpiresAt.Equal(exp) {
		t.Fatalf("stored row %+v", row)
	}
	secret := tok[len(format.Prefix)+27:]
	hash := hex.EncodeToString(row.Hash)
	for _, path := range []string{"/v1/tokens/" + id, "/v1/tokens", "/v1/tokens?include_revoked=true"} {
		r := e.must(http.StatusOK, e.do("GET", path, tokULID, nil))
		if strings.Contains(string(r.Body), secret) || strings.Contains(string(r.Body), hash) || strings.Contains(string(r.Body), `"token"`) {
			t.Errorf("%s shows the token: %s", path, r.Body)
		}
	}
	if logs := e.log.String(); strings.Contains(logs, secret) || strings.Contains(logs, hash) {
		t.Errorf("the log holds the token or its hash: %s", logs)
	}
}

// AC: revoke is idempotent (204 twice), records the caller as revoker, and
// an unknown id is 404 for GET and DELETE.
func TestRevokeToken(t *testing.T) {
	e := tokenEnv(t)
	r := e.must(http.StatusCreated, e.do("POST", "/v1/tokens", tokULID, map[string]any{"name": "old", "scope": "read"}))
	id := r.json(t)["id"].(string)
	e.now = e.now.Add(time.Hour)
	r = e.must(http.StatusNoContent, e.do("DELETE", "/v1/tokens/"+id, tokULID, nil))
	if len(r.Body) != 0 || r.Header.Get("Angelia-Change") != "" {
		t.Errorf("revoke response: %v %s", r.Header, r.Body)
	}
	e.now = e.now.Add(time.Hour)
	e.must(http.StatusNoContent, e.do("DELETE", "/v1/tokens/"+id, tokULID, nil))
	got := e.must(http.StatusOK, e.do("GET", "/v1/tokens/"+id, tokULID, nil)).json(t)
	if got["revoked_by"] != adminULID || got["revoked_at"] != e.now.Add(-time.Hour).Format(timestampLayout) {
		t.Errorf("after two revokes: %v", got)
	}
	const unknown = "01JBX2A7C8D9E0F1G2H3J4K5M6"
	for _, m := range []string{"GET", "DELETE"} {
		r := e.do(m, "/v1/tokens/"+unknown, tokULID, nil)
		if r.Status != http.StatusNotFound || r.problemType(t) != SlugNotFound {
			t.Errorf("%s unknown: %d %s", m, r.Status, r.Body)
		}
	}
	// An id that is not a ULID names nothing, as for every resource id.
	if r := e.do("GET", "/v1/tokens/not-a-ulid", tokULID, nil); r.Status != http.StatusNotFound {
		t.Errorf("GET not-a-ulid: %d %s", r.Status, r.Body)
	}
}

// AC: audit records for create and revoke (not for a repeated revoke), with
// the actor and the target, and no token.
func TestTokenAudit(t *testing.T) {
	e := tokenEnv(t)
	r := e.must(http.StatusCreated, e.do("POST", "/v1/tokens", tokULID, map[string]any{"name": "grafana", "scope": "read"}))
	body := r.json(t)
	id, tok := body["id"].(string), body["token"].(string)
	e.must(http.StatusNoContent, e.do("DELETE", "/v1/tokens/"+id, tokULID, nil))
	e.must(http.StatusNoContent, e.do("DELETE", "/v1/tokens/"+id, tokULID, nil))
	var audits []map[string]any
	for _, rec := range e.logRecords() {
		if rec["audit"] == true {
			audits = append(audits, rec)
		}
	}
	if len(audits) != 2 {
		t.Fatalf("%d audit records, want 2: %v", len(audits), audits)
	}
	for i, action := range []string{"token.create", "token.revoke"} {
		a := audits[i]
		for k, want := range map[string]any{
			"action": action, "principal": adminULID, "principal_name": "ops",
			"token_id": id, "token_name": "grafana", "token_scope": "read",
		} {
			if a[k] != want {
				t.Errorf("%s: %s = %v, want %v", action, k, a[k], want)
			}
		}
	}
	if strings.Contains(e.log.String(), tok[len(format.Prefix)+27:]) {
		t.Error("the token reached the log")
	}
}

func TestListTokensPaging(t *testing.T) {
	e := tokenEnv(t)
	var ids []string
	for _, n := range []string{"a", "b", "c"} {
		r := e.must(http.StatusCreated, e.do("POST", "/v1/tokens", tokULID, map[string]any{"name": n, "scope": "read"}))
		ids = append(ids, r.json(t)["id"].(string))
	}
	revoked := ids[1]
	e.must(http.StatusNoContent, e.do("DELETE", "/v1/tokens/"+revoked, tokULID, nil))
	type page struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	read := func(path string) page {
		var p page
		if err := json.Unmarshal(e.must(http.StatusOK, e.do("GET", path, tokULID, nil)).Body, &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	idsOf := func(p page) []string {
		var out []string
		for _, it := range p.Items {
			out = append(out, it.ID)
		}
		return out
	}
	slices.Sort(ids) // one instant: the ULIDs' order is their random part's
	active := slices.DeleteFunc(slices.Clone(ids), func(s string) bool { return s == revoked })
	if got := idsOf(read("/v1/tokens")); !slices.Equal(got, active) {
		t.Errorf("active: %v, want %v", got, active)
	}
	p := read("/v1/tokens?include_revoked=true&limit=2")
	if got := idsOf(p); !slices.Equal(got, ids[:2]) || p.NextCursor == nil {
		t.Fatalf("first page: %v %v", got, p.NextCursor)
	}
	p = read("/v1/tokens?include_revoked=true&limit=2&cursor=" + *p.NextCursor)
	if got := idsOf(p); !slices.Equal(got, ids[2:]) || p.NextCursor != nil {
		t.Fatalf("second page: %v %v", got, p.NextCursor)
	}
	other := encodeCursor(pageCursor{K: cursorConsumers, N: "x"})
	if r := e.do("GET", "/v1/tokens?cursor="+*other, tokULID, nil); r.Status != http.StatusBadRequest {
		t.Fatalf("a consumer cursor: %d", r.Status)
	}
}

// failingTokens fails the token writes and reads.
type failingTokens struct{ store.APIStore }

var errTokenStore = errors.New("token table down")

func (failingTokens) CreateAPIToken(context.Context, store.APIToken) error { return errTokenStore }
func (failingTokens) ListAPITokens(context.Context, string, int, bool) ([]store.APIToken, error) {
	return nil, errTokenStore
}

func (failingTokens) RevokeAPIToken(context.Context, string, time.Time, string) (bool, error) {
	return false, errTokenStore
}

func TestTokenStoreFailures(t *testing.T) {
	e := tokenEnv(t)
	r := e.must(http.StatusCreated, e.do("POST", "/v1/tokens", tokULID, map[string]any{"name": "a", "scope": "read"}))
	id := r.json(t)["id"].(string)
	srv, err := New(func() Options { o := e.srv.opts; o.Store = failingTokens{e.st}; return o }())
	if err != nil {
		t.Fatal(err)
	}
	e.srv = srv
	for _, c := range []struct{ m, p string }{{"POST", "/v1/tokens"}, {"GET", "/v1/tokens"}, {"DELETE", "/v1/tokens/" + id}} {
		var body any
		if c.m == "POST" {
			body = map[string]any{"name": "b", "scope": "read"}
		}
		r := e.do(c.m, c.p, tokULID, body)
		if r.Status != http.StatusInternalServerError || strings.Contains(string(r.Body), "token table down") {
			t.Errorf("%s %s: %d %s", c.m, c.p, r.Status, r.Body)
		}
	}
}

// failureAuth refuses every token with a reason, as internal/apitoken does.
type failureAuth struct{ reason, id string }

type failureErr struct{ reason, id string }

func (e failureErr) Error() string                   { return "refused" }
func (e failureErr) Unwrap() error                   { return ErrUnauthenticated }
func (e failureErr) FailureReason() (string, string) { return e.reason, e.id }

func (a failureAuth) Authenticate(context.Context, string) (Principal, error) {
	return Principal{}, failureErr(a)
}

// authCounter is both StreamMetrics and AuthMetrics.
type authCounter struct {
	streamCounter
	mu      sync.Mutex
	reasons map[string]int
}

func (c *authCounter) AuthFailed(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reasons[reason]++
}

// AC: angelia_api_auth_failures_total counts each refused request by reason,
// and the access log carries auth_failure, the token id after a hash match
// only, and token_name for an accepted token.
func TestAuthFailureReasons(t *testing.T) {
	for _, tc := range []struct {
		auth   Authenticator
		token  string
		header []string
		reason string
		id     string
	}{
		{nil, "", nil, AuthReasonMissing, ""},
		{nil, "", []string{"Authorization", "Basic dXNlcjpwYXNz"}, AuthReasonMalformed, ""},
		{nil, "nope", nil, AuthReasonUnknown, ""}, // the fake's error does not say why
		{failureAuth{AuthReasonMalformed, ""}, "x", nil, AuthReasonMalformed, ""},
		{failureAuth{AuthReasonRevoked, "01JCZ8W3T6Q9N2H4K7M5R0V8XA"}, "x", nil, AuthReasonRevoked, "01JCZ8W3T6Q9N2H4K7M5R0V8XA"},
		{failureAuth{AuthReasonExpired, "01JCZ8W3T6Q9N2H4K7M5R0V8XA"}, "x", nil, AuthReasonExpired, "01JCZ8W3T6Q9N2H4K7M5R0V8XA"},
		// An id with a reason that is not after a hash match is not logged.
		{failureAuth{AuthReasonUnknown, "01JCZ8W3T6Q9N2H4K7M5R0V8XA"}, "x", nil, AuthReasonUnknown, ""},
		{noScopeAuth{}, "x", nil, AuthReasonUnknown, ""},
	} {
		c := &authCounter{reasons: map[string]int{}}
		e := newEnv(t, func(o *Options) {
			o.Metrics = c
			if tc.auth != nil {
				o.Auth = tc.auth
			}
		})
		r := e.do("GET", "/v1/watches", tc.token, nil, tc.header...)
		if r.Status != http.StatusUnauthorized {
			t.Fatalf("%v: %d", tc, r.Status)
		}
		if c.reasons[tc.reason] != 1 || len(c.reasons) != 1 {
			t.Errorf("%v: counted %v", tc, c.reasons)
		}
		recs := e.logRecords()
		rec := recs[len(recs)-1]
		if rec["auth_failure"] != tc.reason || (rec["token_id"] != nil) != (tc.id != "") || (tc.id != "" && rec["token_id"] != tc.id) {
			t.Errorf("%v: access log %v", tc, rec)
		}
	}

	c := &authCounter{reasons: map[string]int{}}
	e := tokenEnv(t, func(o *Options) { o.Metrics = c })
	e.must(http.StatusOK, e.do("GET", "/v1/watches", tokULID, nil))
	recs := e.logRecords()
	if rec := recs[len(recs)-1]; rec["token_name"] != "ops" || rec["principal"] != adminULID || rec["auth_failure"] != nil {
		t.Errorf("accepted: %v", rec)
	}
	if len(c.reasons) != 0 {
		t.Errorf("an accepted request counted %v", c.reasons)
	}
}

// blockingAuth waits for the request to end and returns its error, as a
// store read does when the client goes away.
type blockingAuth struct{ started chan struct{} }

func (a blockingAuth) Authenticate(ctx context.Context, _ string) (Principal, error) {
	close(a.started)
	<-ctx.Done()
	return Principal{}, ctx.Err()
}

// AC: a client that goes away during authentication is no server failure:
// no 500, no ERROR record, and the access log says 499.
func TestAuthenticateClientGone(t *testing.T) {
	a := blockingAuth{started: make(chan struct{})}
	e := newEnv(t, func(o *Options) { o.Auth = a })
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", "/v1/watches", nil)
	req.Header.Set("Authorization", "Bearer x")
	rec := &headerRecorder{h: http.Header{}}
	go func() { <-a.started; cancel() }()
	e.srv.ServeHTTP(rec, req)
	if rec.code != 0 || len(rec.body) != 0 {
		t.Fatalf("wrote %d %s", rec.code, rec.body)
	}
	logs := e.log.String()
	if strings.Contains(logs, `"level":"ERROR"`) {
		t.Errorf("an ERROR record: %s", logs)
	}
	recs := e.logRecords()
	if last := recs[len(recs)-1]; last["status"] != float64(StatusClientClosed) || last["client_error"] != context.Canceled.Error() {
		t.Errorf("access log %v", last)
	}
}

// headerRecorder records what a handler writes.
type headerRecorder struct {
	h    http.Header
	code int
	body []byte
}

func (r *headerRecorder) Header() http.Header { return r.h }
func (r *headerRecorder) WriteHeader(c int)   { r.code = c }
func (r *headerRecorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	r.body = append(r.body, b...)
	return len(b), nil
}

// AC: the api role runs AuthLoop for its lifetime; the loop's context ends
// after the drain, and the role waits for it (the final flush).
func TestRoleRunsAuthLoop(t *testing.T) {
	started := make(chan struct{})
	var drainedAtStop bool
	var srv *Server
	e := newEnv(t, func(o *Options) {
		o.AuthLoop = func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			drainedAtStop = srv.draining.Load()
			return nil
		}
	})
	srv = e.srv
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.srv.Role().Run(ctx) }()
	<-started
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !drainedAtStop {
		t.Error("the loop stopped before the drain")
	}

	for _, tc := range []struct {
		ret  error
		want string
	}{{errors.New("bad tick"), "bad tick"}, {nil, "stopped"}} {
		e := newEnv(t, func(o *Options) { o.AuthLoop = func(context.Context) error { return tc.ret } })
		err := e.srv.Role().Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), tc.want) || !e.srv.draining.Load() {
			t.Errorf("loop returning %v: role %v", tc.ret, err)
		}
	}
	e = newEnv(t, func(o *Options) {
		o.AuthLoop = func(ctx context.Context) error { <-ctx.Done(); return errors.New("flush failed") }
	})
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if err := e.srv.Role().Run(ctx); err == nil || !strings.Contains(err.Error(), "flush failed") {
		t.Errorf("a failing final flush: %v", err)
	}
}

// gateAuth blocks each authentication until released, then accepts it.
type gateAuth struct {
	entered chan struct{}
	release chan struct{}
}

func (a gateAuth) Authenticate(context.Context, string) (Principal, error) {
	a.entered <- struct{}{}
	<-a.release
	return Principal{ID: "tok-read", Scope: ScopeRead}, nil
}

// roleWithGate starts a request that blocks in authentication and the api
// role with an AuthLoop that reports how many requests were in flight when
// its context ended.
func roleWithGate(t *testing.T, wait time.Duration) (e *env, a gateAuth, cancel func(), roleDone chan error, inflightAtStop chan int, reqDone chan struct{}) {
	t.Helper()
	a = gateAuth{entered: make(chan struct{}), release: make(chan struct{})}
	inflightAtStop = make(chan int, 1)
	var srv *Server
	e = newEnv(t, func(o *Options) {
		o.Auth = a
		o.DrainWait = wait
		o.AuthLoop = func(ctx context.Context) error {
			<-ctx.Done()
			inflightAtStop <- srv.inflight.count()
			return nil
		}
	})
	srv = e.srv
	reqDone = make(chan struct{})
	go func() {
		defer close(reqDone)
		e.do("GET", "/v1/watches", tokRead, nil)
	}()
	<-a.entered
	ctx, c := context.WithCancel(context.Background())
	roleDone = make(chan error, 1)
	go func() { roleDone <- srv.Role().Run(ctx) }()
	return e, a, c, roleDone, inflightAtStop, reqDone
}

// AC (final flush): once the role drains, it waits for the requests in
// flight before it ends AuthLoop, so the final flush comes after their
// authentications.
func TestRoleWaitsForInflightBeforeFlush(t *testing.T) {
	e, a, cancel, roleDone, atStop, reqDone := roleWithGate(t, time.Hour)
	cancel()
	<-e.srv.stop // drained, and the role is waiting for the request
	select {
	case n := <-atStop:
		t.Fatalf("the loop stopped with %d request(s) in flight", n)
	default:
	}
	close(a.release)
	<-reqDone
	if n := <-atStop; n != 0 {
		t.Fatalf("%d request(s) in flight at the final flush", n)
	}
	if err := <-roleDone; err != nil {
		t.Fatal(err)
	}
	// A new request after the drain is refused before authentication.
	if r := e.do("GET", "/v1/watches", tokRead, nil); r.Status != http.StatusServiceUnavailable {
		t.Fatalf("after the drain: %d", r.Status)
	}
}

// The wait is bounded by DrainWait: a stuck request delays the final flush
// at most that long, and the gap is logged.
func TestRoleDrainWaitBound(t *testing.T) {
	e, a, cancel, roleDone, atStop, reqDone := roleWithGate(t, time.Millisecond)
	cancel()
	if err := <-roleDone; err != nil {
		t.Fatal(err)
	}
	if n := <-atStop; n != 1 {
		t.Fatalf("in flight at the flush: %d, want the stuck request", n)
	}
	if !strings.Contains(e.log.String(), "still in flight at the final token flush") {
		t.Errorf("no warning: %s", e.log.String())
	}
	close(a.release)
	<-reqDone
}

func TestReqCounter(t *testing.T) {
	var c reqCounter
	if !c.wait(context.Background()) {
		t.Fatal("an idle counter waits")
	}
	c.enter()
	c.enter()
	c.leave()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if c.wait(ctx) || c.count() != 1 {
		t.Fatal("wait returned with a request in flight")
	}
	c.leave()
	if !c.wait(context.Background()) || c.count() != 0 {
		t.Fatal("wait after the last leave")
	}
	c.enter() // reusable after a wait
	c.leave()
}
