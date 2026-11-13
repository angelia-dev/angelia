package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// stubMCP stands in for internal/mcp's handler (the api package does not
// import the SDK): it records the requests that reach it and, when do is
// set, uses the in-memory REST transport.
type stubMCP struct {
	mu   sync.Mutex
	rest http.RoundTripper
	got  []*http.Request
	body []byte
	do   func(w http.ResponseWriter, r *http.Request, rest http.RoundTripper)
}

func (s *stubMCP) factory(rest http.RoundTripper) (http.Handler, error) {
	s.rest = rest
	return s, nil
}

func (s *stubMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.got = append(s.got, r)
	s.body = b
	s.mu.Unlock()
	if s.do != nil {
		s.do(w, r, s.rest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
}

func (s *stubMCP) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

func newMCPEnv(t *testing.T) (*env, *stubMCP) {
	t.Helper()
	stub := &stubMCP{}
	return newEnv(t, func(o *Options) { o.MCP = stub.factory }), stub
}

const rpcBody = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

// TestMCPAuth: /mcp authenticates exactly like /v1/, with the same 401 and
// challenge, before the MCP handler sees anything.
func TestMCPAuth(t *testing.T) {
	e, stub := newMCPEnv(t)
	for _, tc := range []struct {
		name, auth, challenge string
	}{
		{"missing", "", `Bearer realm="angelia"`},
		{"malformed", "Basic dXNlcjpwYXNz", `Bearer realm="angelia", error="invalid_token"`},
		{"unknown", "Bearer not-a-known-token-000000000000", `Bearer realm="angelia", error="invalid_token"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hdr []string
			if tc.auth != "" {
				hdr = []string{"Authorization", tc.auth}
			}
			r := e.do("POST", MCPPath, "", rpcBody, hdr...)
			v1 := e.do("GET", "/v1/watches", "", nil, hdr...)
			if r.Status != http.StatusUnauthorized || r.problemType(t) != SlugUnauthorized {
				t.Fatalf("%d %s", r.Status, r.Body)
			}
			if got := r.Header.Get("WWW-Authenticate"); got != tc.challenge || got != v1.Header.Get("WWW-Authenticate") {
				t.Errorf("challenge %q, /v1/ %q, want %q", got, v1.Header.Get("WWW-Authenticate"), tc.challenge)
			}
		})
	}
	// Every method needs a token before anything else is said.
	for _, m := range []string{"GET", "DELETE"} {
		if r := e.do(m, MCPPath, "", nil); r.Status != http.StatusUnauthorized {
			t.Errorf("anonymous %s: %d", m, r.Status)
		}
	}
	if stub.calls() != 0 {
		t.Fatalf("the MCP handler ran %d times without a valid token", stub.calls())
	}

	e2 := newEnv(t, func(o *Options) { o.Auth = noScopeAuth{}; o.MCP = stub.factory })
	if r := e2.do("POST", MCPPath, tokRead, rpcBody); r.Status != http.StatusUnauthorized {
		t.Fatalf("a token without a scope: %d", r.Status)
	}
	e3 := newEnv(t, func(o *Options) { o.Auth = failingAuth{}; o.MCP = stub.factory })
	if r := e3.do("POST", MCPPath, tokRead, rpcBody); r.Status != http.StatusInternalServerError {
		t.Fatalf("authenticator failure: %d", r.Status)
	}
	if stub.calls() != 0 {
		t.Fatal("the MCP handler ran without a principal")
	}

	// A read token reaches the handler: scopes are checked per tool call.
	if r := e.do("POST", MCPPath, tokRead, rpcBody); r.Status != http.StatusOK {
		t.Fatalf("read token: %d %s", r.Status, r.Body)
	}
	if stub.calls() != 1 || string(stub.body) != rpcBody {
		t.Fatalf("calls %d, body %q", stub.calls(), stub.body)
	}
}

// TestMCPMethods: only POST; GET and DELETE get 405 (stateless, D10).
func TestMCPMethods(t *testing.T) {
	e, stub := newMCPEnv(t)
	for _, m := range []string{"GET", "DELETE", "PUT", "OPTIONS"} {
		r := e.do(m, MCPPath, tokWrite, nil)
		if r.Status != http.StatusMethodNotAllowed || r.Header.Get("Allow") != "POST" {
			t.Errorf("%s: %d Allow %q", m, r.Status, r.Header.Get("Allow"))
		}
	}
	if stub.calls() != 0 {
		t.Fatal("the MCP handler saw a non-POST request")
	}
	// Only the exact path: /mcp/x is no MCP endpoint.
	if r := e.do("POST", MCPPath+"/x", tokWrite, rpcBody); r.Status != http.StatusNotFound {
		t.Errorf("/mcp/x: %d", r.Status)
	}
}

// TestMCPOrigin: a cross-origin Origin gets 403; no Origin, or the request's
// own host, passes.
func TestMCPOrigin(t *testing.T) {
	e, stub := newMCPEnv(t)
	// httptest.NewRequest sets Host to example.com.
	for _, tc := range []struct {
		origin string
		want   int
	}{
		{"", http.StatusOK},
		{"https://example.com", http.StatusOK},
		{"http://EXAMPLE.com", http.StatusOK},
		{"https://evil.example", http.StatusForbidden},
		{"https://example.com:8443", http.StatusForbidden},
		{"https://example.com.evil.example", http.StatusForbidden},
		{"null", http.StatusForbidden},
		{"file://example.com", http.StatusForbidden},
		{"::not a url", http.StatusForbidden},
		{"https://example.com/", http.StatusOK},
		{"https://user@example.com", http.StatusForbidden},
		{"https://example.com/path", http.StatusForbidden},
		{"https://example.com?x=1", http.StatusForbidden},
		{"https://example.com?", http.StatusForbidden},
		{"https://example.com#f", http.StatusForbidden},
		{"https://example.com:443", http.StatusForbidden}, // ports are not normalised
	} {
		var hdr []string
		if tc.origin != "" {
			hdr = []string{"Origin", tc.origin}
		}
		before := stub.calls()
		r := e.do("POST", MCPPath, tokRead, rpcBody, hdr...)
		if r.Status != tc.want {
			t.Errorf("Origin %q: %d, want %d", tc.origin, r.Status, tc.want)
			continue
		}
		if tc.want == http.StatusForbidden {
			if r.problemType(t) != SlugForbidden || stub.calls() != before {
				t.Errorf("Origin %q: reached the handler or no forbidden problem", tc.origin)
			}
		}
	}
	// Two Origin headers are refused.
	req := httptest.NewRequest("POST", MCPPath, strings.NewReader(rpcBody))
	req.Header.Set("Authorization", "Bearer "+tokRead)
	req.Header.Add("Origin", "https://example.com")
	req.Header.Add("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("two Origin headers: %d", rec.Code)
	}
}

// TestMCPLimits: bodies above MaxBodyBytes get 413, and draining gives 503
// with Retry-After.
func TestMCPLimits(t *testing.T) {
	e, stub := newMCPEnv(t)
	big := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"x":"` + strings.Repeat("a", MaxBodyBytes) + `"}}`
	r := e.do("POST", MCPPath, tokRead, big)
	if r.Status != http.StatusRequestEntityTooLarge || r.problemType(t) != SlugPayloadTooLarge {
		t.Fatalf("Content-Length: %d", r.Status)
	}
	// Without a Content-Length (chunked), the read limit catches it.
	req := httptest.NewRequest("POST", MCPPath, io.MultiReader(strings.NewReader(big)))
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+tokRead)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked: %d", rec.Code)
	}
	// A failing body read is a 400.
	req = httptest.NewRequest("POST", MCPPath, errReader{})
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+tokRead)
	rec = httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("body read error: %d", rec.Code)
	}
	if stub.calls() != 0 {
		t.Fatal("an oversized body reached the handler")
	}
	// Exactly MaxBodyBytes passes.
	exact := `{"x":"` + strings.Repeat("a", MaxBodyBytes-8) + `"}`
	if r := e.do("POST", MCPPath, tokRead, exact); r.Status != http.StatusOK || len(stub.body) != MaxBodyBytes {
		t.Fatalf("1 MiB body: %d, %d bytes", r.Status, len(stub.body))
	}

	// JSON-RPC batches are refused before the handler.
	before := stub.calls()
	for _, b := range []string{"[" + rpcBody + "]", " \r\n\t[" + rpcBody + "]"} {
		r := e.do("POST", MCPPath, tokRead, b)
		if r.Status != http.StatusBadRequest || r.problemType(t) != SlugBadRequest || !strings.Contains(string(r.Body), "batches") {
			t.Errorf("batch %q: %d %s", b, r.Status, r.Body)
		}
	}
	if stub.calls() != before {
		t.Error("a batch reached the handler")
	}
	if r := e.do("POST", MCPPath, tokRead, `{"params":["[x"]}`); r.Status != http.StatusOK {
		t.Errorf("an object with an array: %d", r.Status)
	}

	e.srv.Drain()
	r = e.do("POST", MCPPath, tokRead, rpcBody)
	if r.Status != http.StatusServiceUnavailable || r.Header.Get("Retry-After") == "" || r.problemType(t) != SlugUnavailable {
		t.Fatalf("draining: %d %v", r.Status, r.Header)
	}
	if !strings.Contains(e.log.String(), `"route":"/mcp","status":503`) {
		t.Errorf("the drained request is not logged on route /mcp:\n%s", e.log.String())
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// TestMCPNotServed: without Options.MCP, /mcp is an unknown path and the
// role does not mount it.
func TestMCPNotServed(t *testing.T) {
	e := newEnv(t)
	if r := e.do("POST", MCPPath, tokRead, rpcBody); r.Status != http.StatusNotFound {
		t.Fatalf("%d", r.Status)
	}
	mux := http.NewServeMux()
	e.srv.Role().(interface{ Mount(*http.ServeMux) }).Mount(mux)
	if _, pat := mux.Handler(httptest.NewRequest("POST", MCPPath, nil)); pat != "" {
		t.Fatalf("/mcp mounted as %q", pat)
	}

	e2, _ := newMCPEnv(t)
	mux = http.NewServeMux()
	e2.srv.Role().(interface{ Mount(*http.ServeMux) }).Mount(mux)
	for path, want := range map[string]string{MCPPath: MCPPath, "/v1/watches": "/v1/", "/mcp/x": ""} {
		if _, pat := mux.Handler(httptest.NewRequest("POST", path, nil)); pat != want {
			t.Errorf("%s: pattern %q, want %q", path, pat, want)
		}
	}

	_, err := New(Options{Store: e.st, Auth: e.auth, Sources: testSources(t), MCP: func(http.RoundTripper) (http.Handler, error) {
		return nil, errors.New("no contract")
	}})
	if err == nil || !strings.Contains(err.Error(), "no contract") {
		t.Fatalf("factory error: %v", err)
	}
}

// TestMCPRESTTransport: the in-memory transport runs the REST pipeline with
// the Authorization the caller gives it: auth, scope and access log, each
// time anew.
func TestMCPRESTTransport(t *testing.T) {
	e, stub := newMCPEnv(t)
	call := func(method, path, auth string, body string) (*http.Response, error) {
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(t.Context(), method, "http://angelia.in-process"+path, rd)
		if err != nil {
			t.Fatal(err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		return stub.rest.RoundTrip(req)
	}
	status := func(method, path, auth, body string) int {
		t.Helper()
		resp, err := call(method, path, auth, body)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	watch := `{"source":"pypi","coordinates":{"name":"requests"}}`
	if s := status("PUT", "/v1/watches/w1", "Bearer "+tokRead, watch); s != http.StatusForbidden {
		t.Errorf("read token PUT: %d", s)
	}
	if s := status("PUT", "/v1/watches/w1", "Bearer "+tokWrite, watch); s != http.StatusCreated {
		t.Errorf("write token PUT: %d", s)
	}
	resp, err := call("GET", "/v1/watches/w1", "Bearer "+tokRead, "")
	if err != nil {
		t.Fatal(err)
	}
	var w map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&w); err != nil || w["name"] != "w1" {
		t.Fatalf("GET: %v %v", err, w)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" || resp.Request == nil || resp.ContentLength <= 0 {
		t.Errorf("response %d %v", resp.StatusCode, resp.Header)
	}
	if s := status("GET", "/v1/watches", "", ""); s != http.StatusUnauthorized {
		t.Errorf("no token: %d", s)
	}
	e.auth.revoke(tokRead)
	if s := status("GET", "/v1/watches", "Bearer "+tokRead, ""); s != http.StatusUnauthorized {
		t.Errorf("revoked token: %d", s)
	}
	if resp, err := call("POST", MCPPath, "Bearer "+tokWrite, rpcBody); err == nil {
		_ = resp.Body.Close()
		t.Error("the transport served a path outside /v1/")
	}
	// Each in-memory call is logged as a REST request, marked via mcp.
	if n := strings.Count(e.log.String(), `"route":"/v1/watches/{name}"`); n != 3 {
		t.Errorf("%d access-log records for /v1/watches/{name}:\n%s", n, e.log.String())
	}
	if n := strings.Count(e.log.String(), `"via":"mcp"`); n != 5 {
		t.Errorf("%d records via mcp:\n%s", n, e.log.String())
	}

	// While draining, the in-memory calls of an accepted /mcp request still
	// complete; a request from the network gets 503.
	e.srv.Drain()
	if s := status("GET", "/v1/watches", "Bearer "+tokWrite, ""); s != http.StatusOK {
		t.Errorf("in-memory call while draining: %d", s)
	}
	if r := e.do("GET", "/v1/watches", tokWrite, nil); r.Status != http.StatusServiceUnavailable {
		t.Errorf("network call while draining: %d", r.Status)
	}
	if r := e.do("POST", MCPPath, tokWrite, rpcBody); r.Status != http.StatusServiceUnavailable {
		t.Errorf("new /mcp request while draining: %d", r.Status)
	}
}

// TestMCPAccessLog: one record per /mcp request, with route /mcp, the
// principal and the status, and neither the token nor the body.
func TestMCPAccessLog(t *testing.T) {
	e, stub := newMCPEnv(t)
	stub.do = func(w http.ResponseWriter, r *http.Request, rest http.RoundTripper) {
		req, _ := http.NewRequestWithContext(r.Context(), "GET", "http://angelia.in-process/v1/watches?label=team%3Dsecret-arg-value", nil)
		req.Header["Authorization"] = r.Header.Values("Authorization")
		resp, err := rest.RoundTrip(req)
		if err != nil {
			t.Error(err)
			return
		}
		_ = resp.Body.Close()
		w.WriteHeader(http.StatusAccepted)
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_watches","arguments":{"label":["team=secret-arg-value"]}}}`
	if r := e.do("POST", MCPPath, tokRead, body); r.Status != http.StatusAccepted {
		t.Fatalf("%d", r.Status)
	}
	var mcpRecs, restRecs int
	for _, line := range strings.Split(strings.TrimSpace(e.log.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		switch rec["route"] {
		case MCPPath:
			mcpRecs++
			if _, via := rec["via"]; via {
				t.Errorf("the /mcp record is marked via: %v", rec)
			}
			if rec["principal"] != "tok-read" || rec["status"] != float64(http.StatusAccepted) || rec["path"] != MCPPath || rec["method"] != "POST" {
				t.Errorf("record %v", rec)
			}
		case "/v1/watches":
			restRecs++
			if rec["via"] != "mcp" {
				t.Errorf("the in-memory REST record is not marked via mcp: %v", rec)
			}
		}
	}
	if mcpRecs != 1 || restRecs != 1 {
		t.Errorf("%d /mcp and %d REST records", mcpRecs, restRecs)
	}
	for _, s := range []string{tokRead, "secret-arg-value", "tools/call"} {
		if strings.Contains(e.log.String(), s) {
			t.Errorf("the log holds %q", s)
		}
	}
}

func TestMemResponse(t *testing.T) {
	var m memResponse
	m.header = http.Header{}
	m.Header().Set("A", "1")
	if _, err := m.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	m.Header().Set("A", "2") // after the header was sent: ignored
	m.WriteHeader(http.StatusTeapot)
	if m.status != http.StatusOK || m.sent.Get("A") != "1" || !bytes.Equal(m.body.Bytes(), []byte("x")) {
		t.Fatalf("%d %v %q", m.status, m.sent, m.body.Bytes())
	}
}
