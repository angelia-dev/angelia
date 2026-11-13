package api

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// MCPPath is where the api role serves MCP over Streamable HTTP (ADR 0029,
// D9): outside /v1/, since MCP versions itself through protocol negotiation.
const MCPPath = "/mcp"

// MCPFactory builds the handler of MCPPath. rest is an in-memory transport
// into the Server's REST API: every request it carries goes through
// ServeHTTP, so authentication, scopes, validation and the access log are the
// REST API's. internal/mcp.NewHTTPHandler is the implementation; the api
// package does not import the MCP SDK.
type MCPFactory func(rest http.RoundTripper) (http.Handler, error)

// serveMCP serves an authenticated request to MCPPath. The scope is checked
// per tool call, by the REST calls the tool makes with the caller's token.
// Before the MCP handler sees the request, it must be a POST from no browser
// origin or the request's own, with a body of at most MaxBodyBytes.
func (s *Server) serveMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(r.Method, []string{http.MethodPost}).write(w, r)
		return
	}
	if !sameOrigin(r) {
		problem(SlugForbidden, "a cross-origin request to %s is refused", MCPPath).write(w, r)
		return
	}
	if r.ContentLength > MaxBodyBytes {
		problem(SlugPayloadTooLarge, "the request body is larger than 1 MiB").write(w, r)
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		problem(SlugBadRequest, "reading the request body failed").write(w, r)
		return
	}
	if len(b) > MaxBodyBytes {
		problem(SlugPayloadTooLarge, "the request body is larger than 1 MiB").write(w, r)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	s.mcp.ServeHTTP(w, r)
}

// methodNotAllowed is the 405 problem with its Allow header.
func methodNotAllowed(method string, allowed []string) *ProblemDetail {
	list := strings.Join(allowed, ", ")
	pr := problem(SlugBadRequest, "method %s is not allowed here (allowed: %s)", method, list)
	pr.Status, pr.Title = http.StatusMethodNotAllowed, "Method not allowed"
	return pr.withHeader("Allow", list)
}

// sameOrigin reports whether r has no Origin header (a non-browser client)
// or one whose host is the request's Host (spec docs/mcp.md D10). An opaque
// origin ("null"), a malformed one or several Origin headers are
// cross-origin.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Values("Origin")
	switch len(o) {
	case 0:
		return true
	case 1:
	default:
		return false
	}
	u, err := url.Parse(o[0])
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// restTransport is the in-memory transport of MCPFactory: it serves a /v1/
// request with ServeHTTP and buffers the response. Nothing goes over the
// network and nothing is cached: each request is authenticated anew.
type restTransport struct{ s *Server }

func (t restTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		defer func() { _ = req.Body.Close() }()
	}
	if !strings.HasPrefix(req.URL.Path, "/v1/") {
		return nil, fmt.Errorf("api: the in-process transport serves /v1/ only, not %s", req.URL.Path)
	}
	r := req.Clone(req.Context())
	if r.Body == nil {
		r.Body = http.NoBody
	}
	r.RequestURI = req.URL.RequestURI()
	r.RemoteAddr = "in-process"
	w := &memResponse{header: http.Header{}}
	t.s.ServeHTTP(w, r)
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	h := w.sent
	if h == nil {
		h = w.header.Clone()
	}
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        h,
		Body:          io.NopCloser(bytes.NewReader(w.body.Bytes())),
		ContentLength: int64(w.body.Len()),
		Request:       req,
	}, nil
}

// memResponse is the http.ResponseWriter of restTransport.
type memResponse struct {
	header http.Header
	sent   http.Header // the header as of WriteHeader
	status int
	body   bytes.Buffer
}

func (m *memResponse) Header() http.Header { return m.header }

func (m *memResponse) WriteHeader(code int) {
	if m.status == 0 {
		m.status, m.sent = code, m.header.Clone()
	}
}

func (m *memResponse) Write(b []byte) (int, error) {
	if m.status == 0 {
		m.WriteHeader(http.StatusOK)
	}
	return m.body.Write(b) //nolint:wrapcheck // bytes.Buffer does not fail
}
