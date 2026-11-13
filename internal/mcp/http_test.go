package mcp

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	contract "github.com/angelia-dev/angelia/api/mcp"
	"github.com/angelia-dev/angelia/internal/api"
	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/pypi"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
	"github.com/angelia-dev/angelia/internal/ulid"
)

// tokInvalid is a synthetic token no authenticator knows.
const tokInvalid = "mcp-invalid-token-000000000000003"

// httpAuth is a revocable fake Authenticator that records every token it
// is asked about. revokeAfterNext, when set, revokes that token right after
// its next successful check. hook, when set, runs once after hookAfter more
// successful checks.
type httpAuth struct {
	mu              sync.Mutex
	tokens          map[string]api.Principal
	seen            []string
	revokeAfterNext string
	hookAfter       int
	hook            func()
}

func newHTTPAuth() *httpAuth {
	return &httpAuth{tokens: map[string]api.Principal{
		tokRead:  {ID: "read", Scope: api.ScopeRead},
		tokWrite: {ID: "write", Scope: api.ScopeWrite},
	}}
}

func (a *httpAuth) Authenticate(_ context.Context, token string) (api.Principal, error) {
	a.mu.Lock()
	a.seen = append(a.seen, token)
	for t, p := range a.tokens {
		if subtle.ConstantTimeCompare([]byte(t), []byte(token)) == 1 {
			if a.revokeAfterNext == t {
				delete(a.tokens, t)
				a.revokeAfterNext = ""
			}
			var run func()
			if a.hook != nil {
				if a.hookAfter--; a.hookAfter <= 0 {
					run, a.hook = a.hook, nil
				}
			}
			a.mu.Unlock()
			if run != nil {
				run()
			}
			return p, nil
		}
	}
	a.mu.Unlock()
	return api.Principal{}, fmt.Errorf("token: %w", api.ErrUnauthenticated)
}

func (a *httpAuth) setHook(after int, f func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hookAfter, a.hook = after, f
}

func (a *httpAuth) revoke(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.tokens, token)
}

func (a *httpAuth) setRevokeAfterNext(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.revokeAfterNext = token
}

func (a *httpAuth) takeSeen() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.seen
	a.seen = nil
	return s
}

// logBuffer is a log sink safe for concurrent writes.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p) //nolint:wrapcheck // bytes.Buffer does not fail
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// httpEnv is a real api.Server on SQLite, with /mcp served by
// NewHTTPHandler, mounted by the api role on an httptest server.
type httpEnv struct {
	url  string
	api  *api.Server
	st   *sqlite.Store
	auth *httpAuth
	log  *logBuffer
}

func newHTTPEnv(t *testing.T) *httpEnv {
	t.Helper()
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "api.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	reg := source.NewRegistry()
	if err := reg.Register(pypi.New(pypi.DefaultBaseURL, &http.Client{})); err != nil {
		t.Fatal(err)
	}
	e := &httpEnv{st: st, auth: newHTTPAuth(), log: &logBuffer{}}
	log := slog.New(slog.NewJSONHandler(e.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv, err := api.New(api.Options{
		Store: st, Auth: e.auth, Sources: reg, Logger: log, API: config.API{},
		Plugins: []api.PluginInfo{{Kind: "pypi", Interface: "source", Runtime: "builtin", Version: "0.1.0"}},
		MCP: func(rest http.RoundTripper) (http.Handler, error) {
			return NewHTTPHandler(HTTPOptions{REST: rest, Version: "test", Logger: log})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.api = srv
	mux := http.NewServeMux()
	srv.Role().(interface{ Mount(*http.ServeMux) }).Mount(mux)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	e.url = hs.URL
	return e
}

// authTransport sets the Authorization header and records each response's
// status and headers.
type authTransport struct {
	auth string
	mu   sync.Mutex
	resp []*http.Response
}

func (a *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	if a.auth != "" {
		r.Header.Set("Authorization", a.auth)
	}
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err == nil {
		a.mu.Lock()
		a.resp = append(a.resp, resp)
		a.mu.Unlock()
	}
	return resp, err //nolint:wrapcheck // a transport passes errors through
}

// connect runs initialize over Streamable HTTP with the SDK's client.
func (e *httpEnv) connect(t *testing.T, token string) (*sdk.ClientSession, *authTransport, error) {
	t.Helper()
	at := &authTransport{}
	if token != "" {
		at.auth = "Bearer " + token
	}
	tr := &sdk.StreamableClientTransport{
		Endpoint: e.url + api.MCPPath, HTTPClient: &http.Client{Transport: at},
		DisableStandaloneSSE: true, MaxRetries: -1,
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(t.Context(), tr, nil)
	if err == nil {
		t.Cleanup(func() { _ = cs.Close() })
	}
	return cs, at, err //nolint:wrapcheck // test helper
}

func (e *httpEnv) mustConnect(t *testing.T, token string) *sdk.ClientSession {
	t.Helper()
	cs, _, err := e.connect(t, token)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return cs
}

// rawResp is the status and headers of a raw /mcp response.
type rawResp struct {
	StatusCode int
	Header     http.Header
}

// post sends one raw JSON-RPC request to /mcp; hdr are header pairs.
func (e *httpEnv) post(t *testing.T, method, body string, hdr ...string) (rawResp, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, e.url+api.MCPPath, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "Host" {
			req.Host = hdr[i+1]
			continue
		}
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return rawResp{StatusCode: resp.StatusCode, Header: resp.Header}, b
}

func toolCall(name string, args any) string {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	return string(b)
}

// seed creates through REST (write token) the watch "requests" with a final
// and a pre-release version, the consumer "bot" from the start, and three
// events of the watch.
func (e *httpEnv) seed(t *testing.T) {
	t.Helper()
	tg := putWatch(t, e.url, e.st, "requests", `{"source":"pypi","coordinates":{"name":"requests"}}`)
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, v := range []store.Version{
		{TargetID: tg, Raw: "2.32.0", Normalized: "2.32.0", FirstSeenAt: t0},
		{TargetID: tg, Raw: "2.33.0rc1", Normalized: "2.33.0rc1", Prerelease: true, FirstSeenAt: t0},
	} {
		if _, err := e.st.AddVersion(t.Context(), v); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequestWithContext(t.Context(), "PUT", e.url+"/v1/consumers/bot", strings.NewReader(`{"start":"earliest"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tokWrite)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("consumer: %d", resp.StatusCode)
	}
	sw, err := e.st.WatchByName(t.Context(), "requests")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		at := t0.Add(time.Duration(i) * time.Hour)
		id := ulid.New(at)
		ev := store.Event{ID: id, WatchID: sw.ID, Type: event.TypePublished, Payload: testEnvelope(id, sw.ID, fmt.Sprintf("2.3%d.0", i), at), CreatedAt: at}
		if err := e.st.AppendEvent(t.Context(), ev, nil); err != nil {
			t.Fatal(err)
		}
	}
	e.auth.takeSeen()
}

// testEnvelope is a release event envelope like the spec's example.
func testEnvelope(id, watchID, version string, at time.Time) []byte {
	const ts = "2006-01-02T15:04:05.000Z"
	b, _ := json.Marshal(map[string]any{
		"specversion": "1.0", "type": event.TypePublished, "source": "angelia://angelia/watches/" + watchID, "id": id,
		"subject": "pypi:requests@" + version, "time": at.UTC().Format(ts),
		"datacontenttype": "application/json", "dataschema": "https://github.com/angelia-dev/spec/schemas/release/v1.json",
		"data": map[string]any{
			"source": "pypi", "package": "requests", "version": version, "normalized_version": version,
			"previous_version": nil, "scheme": "pep440", "bump": "minor", "backport": false, "prerelease": false,
			"yanked": false, "published_at": nil, "detected_at": at.UTC().Format(ts),
			"cooldown_satisfied_at": nil, "urls": map[string]any{"release": nil, "changelog": nil, "artifact": nil},
			"notes": nil, "watch": map[string]any{"id": watchID, "name": "requests", "labels": map[string]any{}},
			"purl": nil,
		},
	})
	return b
}

// TestHTTPEndToEnd: the SDK's client over Streamable HTTP against a real
// api.Server on SQLite: initialize, tools/list and every tool.
func TestHTTPEndToEnd(t *testing.T) {
	e := newHTTPEnv(t)
	e.seed(t)
	cs, at, err := e.connect(t, tokWrite)
	if err != nil {
		t.Fatal(err)
	}
	if ir := cs.InitializeResult(); ir == nil || ir.ServerInfo.Name != "angelia" || ir.Capabilities.Tools == nil {
		t.Fatalf("initialize result %+v", ir)
	}

	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(map[string]any{"tools": res.Tools})
	var g, w any
	_ = json.Unmarshal(got, &g)
	_ = json.Unmarshal(contract.Tools, &w)
	if !jsonEqual(g, w) {
		t.Fatalf("tools/list over HTTP differs from the contract:\n%s", got)
	}

	lw := success(t, "list_watches", callTool(t, cs, "list_watches", map[string]any{}))
	if items, _ := lw["items"].([]any); len(items) != 1 {
		t.Errorf("list_watches %v", lw)
	}
	lv := success(t, "latest_version", callTool(t, cs, "latest_version", map[string]any{"watch": "requests"}))
	if lv["version"] != "2.32.0" {
		t.Errorf("latest_version %v", lv)
	}
	lv = success(t, "latest_version", callTool(t, cs, "latest_version", map[string]any{"watch": "requests", "include_prerelease": true}))
	if lv["version"] != "2.33.0rc1" {
		t.Errorf("latest_version with pre-releases %v", lv)
	}
	es := success(t, "events_since", callTool(t, cs, "events_since", map[string]any{"limit": 2}))
	if items, _ := es["items"].([]any); len(items) != 2 || es["has_more"] != true {
		t.Errorf("events_since %v", es)
	}
	es = success(t, "events_since", callTool(t, cs, "events_since", map[string]any{"consumer": "bot"}))
	items, _ := es["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("events_since consumer %v", es)
	}
	ack := success(t, "ack_event", callTool(t, cs, "ack_event", map[string]any{"consumer": "bot", "seq": es["next_after"]}))
	if ack["acked_seq"] != es["next_after"] || ack["consumer"] != "bot" {
		t.Errorf("ack_event %v", ack)
	}
	es = success(t, "events_since", callTool(t, cs, "events_since", map[string]any{"consumer": "bot"}))
	if items, _ := es["items"].([]any); len(items) != 0 {
		t.Errorf("after the ack: %v", es)
	}
	aw := success(t, "add_watch", callTool(t, cs, "add_watch", map[string]any{"name": "flask", "source": "pypi", "coordinates": map[string]string{"name": "flask"}}))
	if aw["change"] != "created" {
		t.Errorf("add_watch %v", aw)
	}
	aw = success(t, "add_watch", callTool(t, cs, "add_watch", map[string]any{"name": "flask", "source": "pypi", "coordinates": map[string]string{"name": "flask"}}))
	if aw["change"] != "unchanged" {
		t.Errorf("add_watch again %v", aw)
	}

	// Stateless JSON (D10): every response is application/json, with no
	// session id.
	at.mu.Lock()
	defer at.mu.Unlock()
	if len(at.resp) < 10 {
		t.Fatalf("%d responses", len(at.resp))
	}
	for _, r := range at.resp {
		if r.Header.Get("Mcp-Session-Id") != "" {
			t.Errorf("a stateless response has Mcp-Session-Id %q", r.Header.Get("Mcp-Session-Id"))
		}
		if ct := r.Header.Get("Content-Type"); r.StatusCode == http.StatusOK && !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type %q, want JSON", ct)
		}
	}
}

// TestHTTPTokensByTool is the table tokens (none, invalid, read, write) ×
// tools. No or an invalid token never reaches MCP: 401 with the /v1/
// challenge. A read token lists every tool and calls the reads; the writes
// are tool errors with required_scope write.
func TestHTTPTokensByTool(t *testing.T) {
	e := newHTTPEnv(t)
	e.seed(t)
	args := map[string]any{
		"list_watches":   map[string]any{},
		"latest_version": map[string]any{"watch": "requests"},
		"events_since":   map[string]any{},
		"add_watch":      map[string]any{"name": "flask", "source": "pypi", "coordinates": map[string]string{"name": "flask"}},
		"ack_event":      map[string]any{"consumer": "bot", "seq": 1},
	}
	writes := map[string]bool{"add_watch": true, "ack_event": true}

	for _, tc := range []struct{ name, token, challenge string }{
		{"none", "", `Bearer realm="angelia"`},
		{"invalid", tokInvalid, `Bearer realm="angelia", error="invalid_token"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := e.connect(t, tc.token); err == nil {
				t.Fatal("initialize succeeded")
			}
			bodies := []string{`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`}
			for name, a := range args {
				bodies = append(bodies, toolCall(name, a))
			}
			for _, b := range bodies {
				var hdr []string
				if tc.token != "" {
					hdr = []string{"Authorization", "Bearer " + tc.token}
				}
				resp, body := e.post(t, "POST", b, hdr...)
				if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != tc.challenge ||
					resp.Header.Get("Content-Type") != "application/problem+json" || strings.Contains(string(body), "jsonrpc") {
					t.Errorf("%s: %d %v %s", b, resp.StatusCode, resp.Header, body)
				}
			}
			// Only the outer request was authenticated: no REST call ran.
			for _, s := range e.auth.takeSeen() {
				if s != tc.token {
					t.Errorf("authenticated %q", s)
				}
			}
		})
	}

	for _, tc := range []struct {
		name, token string
		write       bool
	}{{"read", tokRead, false}, {"write", tokWrite, true}} {
		t.Run(tc.name, func(t *testing.T) {
			cs := e.mustConnect(t, tc.token)
			lt, err := cs.ListTools(t.Context(), nil)
			if err != nil || len(lt.Tools) != len(args) {
				t.Fatalf("tools/list: %v %v", lt, err)
			}
			for _, name := range []string{"list_watches", "latest_version", "events_since", "add_watch", "ack_event"} {
				res := callTool(t, cs, name, args[name])
				if writes[name] && !tc.write {
					p := failure(t, res)
					if p["status"] != 403.0 || p["required_scope"] != "write" {
						t.Errorf("%s: %v", name, p)
					}
					continue
				}
				success(t, name, res)
			}
		})
	}
}

// TestHTTPTokenForwarding: every REST call of a tool carries the caller's
// token and is authenticated anew; a revoked token fails on its next call,
// even one already past the /mcp check.
func TestHTTPTokenForwarding(t *testing.T) {
	e := newHTTPEnv(t)
	e.seed(t)
	cs := e.mustConnect(t, tokRead)
	e.auth.takeSeen()
	success(t, "latest_version", callTool(t, cs, "latest_version", map[string]any{"watch": "requests"}))
	seen := e.auth.takeSeen()
	if len(seen) < 2 { // the /mcp request, then each REST call
		t.Fatalf("authenticated %d times", len(seen))
	}
	for _, s := range seen {
		if s != tokRead {
			t.Errorf("a REST call carried %q", s)
		}
	}
	// A second client with another token on the same server: its calls carry
	// its own token, nothing is shared.
	w := e.mustConnect(t, tokWrite)
	e.auth.takeSeen()
	success(t, "list_watches", callTool(t, w, "list_watches", map[string]any{}))
	for _, s := range e.auth.takeSeen() {
		if s != tokWrite {
			t.Errorf("the write client's call carried %q", s)
		}
	}

	// Revoked between the /mcp check and the REST call: the tool call fails
	// with the REST 401.
	e.auth.setRevokeAfterNext(tokRead)
	p := failure(t, callTool(t, cs, "list_watches", map[string]any{}))
	if p["status"] != 401.0 {
		t.Errorf("revoked mid-request: %v", p)
	}
	// And from then on the /mcp request itself is refused.
	if _, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: "list_watches", Arguments: map[string]any{}}); err == nil {
		t.Error("a revoked token still calls tools")
	}
	// The write token is untouched.
	success(t, "list_watches", callTool(t, w, "list_watches", map[string]any{}))
	e.auth.revoke(tokWrite)
	if _, err := w.CallTool(t.Context(), &sdk.CallToolParams{Name: "list_watches", Arguments: map[string]any{}}); err == nil {
		t.Error("a revoked write token still calls tools")
	}
}

// TestForwardAuthExactHeader: the REST calls carry exactly the request's
// Authorization header values, whatever the API client would have set.
func TestForwardAuthExactHeader(t *testing.T) {
	var mu sync.Mutex
	var got [][]string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		got = append(got, r.Header.Values("Authorization"))
		mu.Unlock()
		return &http.Response{
			StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"items":[],"next_cursor":null}`)), Request: r,
		}, nil
	})
	h, err := NewHTTPHandler(HTTPOptions{REST: rt, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, auth := range []string{"Bearer first-token", "bearer  second-token"} {
		req := httptest.NewRequest("POST", "/mcp", strings.NewReader(toolCall("list_watches", map[string]any{})))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", auth)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"isError"`) && !strings.Contains(rec.Body.String(), `"structuredContent"`) {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	}
	if len(got) != 2 || len(got[0]) != 1 || got[0][0] != "Bearer first-token" || len(got[1]) != 1 || got[1][0] != "bearer  second-token" {
		t.Fatalf("REST calls carried %q", got)
	}
	if _, err := NewHTTPHandler(HTTPOptions{}); err == nil {
		t.Error("no REST transport accepted")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestHTTPSessionsAndMethods: GET and DELETE get 405; a POST gets no
// session id.
func TestHTTPSessionsAndMethods(t *testing.T) {
	e := newHTTPEnv(t)
	for _, m := range []string{"GET", "DELETE"} {
		resp, _ := e.post(t, m, "", "Authorization", "Bearer "+tokRead)
		if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "POST" {
			t.Errorf("%s: %d Allow %q", m, resp.StatusCode, resp.Header.Get("Allow"))
		}
	}
	resp, body := e.post(t, "POST", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "Authorization", "Bearer "+tokRead)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Mcp-Session-Id") != "" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("%d %v %s", resp.StatusCode, resp.Header, body)
	}
	if !strings.Contains(string(body), `"list_watches"`) {
		t.Errorf("tools/list without initialize: %s", body)
	}
	// A malformed request is the SDK's to refuse, after authentication.
	resp, body = e.post(t, "POST", `{"jsonrpc":"2.0","id":1,"method":"no/such/method"}`, "Authorization", "Bearer "+tokRead)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown method: %d %s", resp.StatusCode, body)
	}
}

// TestHTTPOriginAndRebinding: a cross-origin Origin gets 403, no Origin or
// the request's own passes, and the SDK's localhost protection refuses a
// loopback request for another host (DNS rebinding).
func TestHTTPOriginAndRebinding(t *testing.T) {
	e := newHTTPEnv(t)
	host := strings.TrimPrefix(e.url, "http://")
	list := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	auth := []string{"Authorization", "Bearer " + tokRead}
	for _, tc := range []struct {
		name string
		hdr  []string
		want int
	}{
		{"no Origin", nil, http.StatusOK},
		{"same origin", []string{"Origin", "http://" + host}, http.StatusOK},
		{"cross origin", []string{"Origin", "https://evil.example"}, http.StatusForbidden},
		{"other port", []string{"Origin", "http://127.0.0.1:1"}, http.StatusForbidden},
		{"opaque origin", []string{"Origin", "null"}, http.StatusForbidden},
		{"rebinding, no Origin", []string{"Host", "evil.example"}, http.StatusForbidden},
		{"rebinding, matching Origin", []string{"Host", "evil.example", "Origin", "http://evil.example"}, http.StatusForbidden},
		{"localhost name", []string{"Host", "localhost:" + strings.Split(host, ":")[1]}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := e.post(t, "POST", list, append(slicesClone(auth), tc.hdr...)...)
			if resp.StatusCode != tc.want {
				t.Fatalf("%d, want %d: %s", resp.StatusCode, tc.want, body)
			}
			if tc.want == http.StatusOK && !strings.Contains(string(body), `"tools"`) {
				t.Errorf("body %s", body)
			}
		})
	}
	// Without a token the answer is 401 whatever the Origin: auth comes first.
	if resp, _ := e.post(t, "POST", list, "Origin", "https://evil.example"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous cross-origin: %d", resp.StatusCode)
	}
}

func slicesClone(s []string) []string { return append([]string(nil), s...) }

// TestHTTPLimitsAndDrain: bodies above 1 MiB get 413, and a draining server
// answers 503 with Retry-After.
func TestHTTPLimitsAndDrain(t *testing.T) {
	e := newHTTPEnv(t)
	big := toolCall("list_watches", map[string]any{"cursor": strings.Repeat("a", api.MaxBodyBytes)})
	resp, body := e.post(t, "POST", big, "Authorization", "Bearer "+tokRead)
	if resp.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(string(body), "payload-too-large") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	// Without a Content-Length (chunked), end to end.
	req, err := http.NewRequestWithContext(t.Context(), "POST", e.url+api.MCPPath, io.MultiReader(strings.NewReader(big)))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+tokRead)
	cr, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	cb, _ := io.ReadAll(cr.Body)
	_ = cr.Body.Close()
	if cr.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(string(cb), "payload-too-large") {
		t.Fatalf("chunked: %d %s", cr.StatusCode, cb)
	}
	// A JSON-RPC batch is refused (MCP 2025-06-18 has none).
	batch := "[" + toolCall("list_watches", map[string]any{}) + "," + toolCall("list_watches", map[string]any{}) + "]"
	resp, body = e.post(t, "POST", batch, "Authorization", "Bearer "+tokRead)
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Content-Type") != "application/problem+json" || !strings.Contains(string(body), "batches") {
		t.Fatalf("batch: %d %s", resp.StatusCode, body)
	}

	e.api.Drain()
	resp, body = e.post(t, "POST", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "Authorization", "Bearer "+tokRead)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != fmt.Sprint(api.RetryAfterSeconds) {
		t.Fatalf("draining: %d %v %s", resp.StatusCode, resp.Header, body)
	}
}

// TestHTTPLogging: one access-log record per /mcp request, with route /mcp,
// the principal and the status; the in-memory REST calls log as REST
// requests; the token and the tool arguments appear nowhere.
func TestHTTPLogging(t *testing.T) {
	e := newHTTPEnv(t)
	cs := e.mustConnect(t, tokWrite)
	const secretArg = "zz-argument-value-7f3a" //nolint:gosec // a tool argument, not a credential
	success(t, "add_watch", callTool(t, cs, "add_watch", map[string]any{
		"name": "flask", "source": "pypi", "coordinates": map[string]string{"name": secretArg},
		"labels": map[string]string{"team": secretArg},
	}))
	success(t, "list_watches", callTool(t, cs, "list_watches", map[string]any{"label": []string{"team=" + secretArg}}))
	success(t, "events_since", callTool(t, cs, "events_since", map[string]any{"watch": []string{"flask"}, "source": []string{"pypi"}}))
	resp, _ := e.post(t, "POST", toolCall("list_watches", map[string]any{"cursor": secretArg}), "Authorization", "Bearer "+tokInvalid)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("%d", resp.StatusCode)
	}

	out := e.log.String()
	for _, s := range []string{tokWrite, tokInvalid, secretArg, "Bearer", "tools/call", "jsonrpc"} {
		if strings.Contains(out, s) {
			t.Errorf("the log holds %q:\n%s", s, out)
		}
	}
	var mcpRecs, restRecs, other int
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		if rec["msg"] != "api request" {
			other++
			continue
		}
		switch route, _ := rec["route"].(string); {
		case route == api.MCPPath:
			mcpRecs++
			if rec["path"] != api.MCPPath || rec["method"] != "POST" {
				t.Errorf("record %v", rec)
			}
			if rec["status"] == float64(http.StatusOK) && rec["principal"] != "write" {
				t.Errorf("record without the principal: %v", rec)
			}
		case strings.HasPrefix(route, "/v1/"):
			restRecs++
			if rec["principal"] != "write" {
				t.Errorf("REST record %v", rec)
			}
		default:
			t.Errorf("record on route %q: %v", route, rec)
		}
	}
	// initialize and three tool calls with the write token, plus the refused
	// call.
	if mcpRecs != 5 || restRecs < 4 || other != 0 {
		t.Errorf("%d /mcp records, %d REST records, %d other:\n%s", mcpRecs, restRecs, other, out)
	}
	if !strings.Contains(out, `"route":"/mcp","status":401`) {
		t.Errorf("the refused call is not logged:\n%s", out)
	}
}

// TestHTTPDrainMidCall: a drain that starts between the two REST calls of
// one add_watch call (GET, then PUT) lets that call complete; a new /mcp
// request then gets 503.
func TestHTTPDrainMidCall(t *testing.T) {
	e := newHTTPEnv(t)
	cs := e.mustConnect(t, tokWrite)
	// Checks: the /mcp request, the GET, then the PUT. Drain after the GET's.
	e.auth.setHook(2, e.api.Drain)
	got := success(t, "add_watch", callTool(t, cs, "add_watch", map[string]any{"name": "flask", "source": "pypi", "coordinates": map[string]string{"name": "flask"}}))
	if got["change"] != "created" {
		t.Fatalf("add_watch during the drain: %v", got)
	}
	if _, err := e.st.WatchByName(t.Context(), "flask"); err != nil {
		t.Fatalf("the PUT did not land: %v", err)
	}
	resp, _ := e.post(t, "POST", toolCall("list_watches", map[string]any{}), "Authorization", "Bearer "+tokWrite)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("new request while draining: %d", resp.StatusCode)
	}
	if !strings.Contains(e.log.String(), `"method":"PUT","path":"/v1/watches/flask","route":"/v1/watches/{name}","status":201`) {
		t.Errorf("the PUT is not logged as served:\n%s", e.log.String())
	}
}

// TestBuildOncePerRequest: the SDK asks for the server twice per stateless
// POST; it is built once.
func TestBuildOncePerRequest(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"items":[],"next_cursor":null}`)), Request: r,
		}, nil
	})
	var builds int
	var mu sync.Mutex
	h, err := newHTTPHandler(HTTPOptions{REST: rt}, func() { mu.Lock(); builds++; mu.Unlock() })
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		req := httptest.NewRequest("POST", "/mcp", strings.NewReader(toolCall("list_watches", map[string]any{})))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		mu.Lock()
		if builds != i {
			t.Errorf("after %d requests, %d builds", i, builds)
		}
		mu.Unlock()
	}
}

func TestMaxBodyBytesMatchesAPI(t *testing.T) {
	if maxBodyBytes != api.MaxBodyBytes {
		t.Fatalf("maxBodyBytes %d, api.MaxBodyBytes %d", maxBodyBytes, api.MaxBodyBytes)
	}
}

// TestQuietLogger: below warn is dropped, also through WithAttrs and
// WithGroup, which keep the attributes and groups.
func TestQuietLogger(t *testing.T) {
	var buf logBuffer
	l := quiet(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	for _, lg := range []*slog.Logger{l, l.With("k", "v"), l.WithGroup("g").With("k", "v")} {
		lg.Info("dropped")
		lg.Warn("kept")
	}
	out := buf.String()
	if strings.Contains(out, "dropped") || strings.Count(out, `"msg":"kept"`) != 3 {
		t.Fatalf("%s", out)
	}
	if !strings.Contains(out, `"k":"v"`) || !strings.Contains(out, `"g":{"k":"v"}`) {
		t.Errorf("attributes or group lost: %s", out)
	}
	quiet(nil).Error("discarded") // must not panic
}
