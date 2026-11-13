package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/ulid"
)

// MaxBodyBytes is the largest request body accepted (spec docs/api.md
// decision 21); larger bodies get 413 payload-too-large.
const MaxBodyBytes = 1 << 20

// RetryAfterSeconds is the Retry-After of a 503 while the instance drains.
const RetryAfterSeconds = 5

// Stream defaults (spec docs/api.md decision 15).
const (
	DefaultStreamPoll = time.Second
	DefaultKeepAlive  = 15 * time.Second
	MaxAuthRecheck    = 60 * time.Second
	DefaultEventScan  = 10000
	// DefaultMaxStreams bounds the event streams one Server holds open.
	DefaultMaxStreams  = 1000
	streamWriteTimeout = 10 * time.Second
	// StatusClientClosed is the status the access log records for a request
	// whose client went away before anything was written (nginx's 499). It
	// is never sent.
	StatusClientClosed = 499
)

// Options configures a Server. Store, Auth and Sources are required.
type Options struct {
	Store store.APIStore
	// Auth resolves bearer tokens. `angelia serve` passes DenyAll until API
	// tokens (#72) exist.
	Auth Authenticator
	// Sources validates watch coordinates (no I/O), as for the watch file.
	Sources *source.Registry
	// Plugins is the catalogue of GET /v1/plugins: every usable source and
	// sink kind, built-in or installed.
	Plugins []PluginInfo
	// API is the instance configuration's api section (api.allowed_env).
	API config.API
	// Secrets seals the values of /v1/secrets; nil means no key is
	// configured, and a write gets 422 secrets-key-missing.
	Secrets *secrets.Keyring
	// AllowHTTPHosts is http.allow_http_hosts: the hosts a webhook may
	// reach over plain http:// (decision D12). Empty: https only.
	AllowHTTPHosts []string
	// Logger receives the access log and the audit records; nil discards.
	Logger *slog.Logger
	// Now and NewID default to the real clock and random ULIDs.
	Now   func() time.Time
	NewID func() string

	// StreamPoll is how often an open event stream looks for new events;
	// KeepAlive how long it stays silent before a keep-alive comment;
	// AuthRecheck how often it checks its token again (at most 60 s).
	StreamPoll  time.Duration
	KeepAlive   time.Duration
	AuthRecheck time.Duration
	// EventScan bounds the events one GET /v1/events request reads.
	EventScan int
	// MaxStreams bounds the open event streams: the next one gets 503
	// unavailable with Retry-After. 0 means DefaultMaxStreams.
	MaxStreams int
	// Metrics, if set, observes the open event streams
	// (angelia_api_event_streams). Pass a non-nil value or leave it nil.
	Metrics StreamMetrics
	// MCP, if set, builds the handler of POST /mcp (MCPPath), which the api
	// role then mounts next to /v1/. Nil: /mcp is not served.
	MCP MCPFactory
}

// StreamMetrics observes the open event streams. metrics.Metrics implements
// it.
type StreamMetrics interface {
	EventStreamOpened()
	EventStreamClosed()
}

// Server serves the REST API of spec openapi/angelia.v1.yaml. It is an
// http.Handler for the /v1/ paths, and for MCPPath when Options.MCP is set.
type Server struct {
	opts     Options
	log      *slog.Logger
	spec     *specDoc
	gen      http.Handler
	draining atomic.Bool
	stop     chan struct{}
	stopOnce atomic.Bool
	hub      *streamHub
	stopHub  context.CancelFunc
	streams  atomic.Int64 // open event streams
	mcp      http.Handler // POST /mcp; nil when not served
}

// New builds a Server.
func New(opts Options) (*Server, error) {
	if opts.Store == nil || opts.Auth == nil || opts.Sources == nil {
		return nil, errors.New("api: Store, Auth and Sources are required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.NewID == nil {
		opts.NewID = func() string { return ulid.New(opts.Now()) }
	}
	if opts.StreamPoll <= 0 {
		opts.StreamPoll = DefaultStreamPoll
	}
	if opts.KeepAlive <= 0 {
		opts.KeepAlive = DefaultKeepAlive
	}
	// A stream checks its token on the first poll after AuthRecheck has
	// passed, so the worst case is AuthRecheck + StreamPoll: keep it within
	// MaxAuthRecheck (spec docs/api.md decision 15).
	opts.StreamPoll = min(opts.StreamPoll, MaxAuthRecheck/2)
	if opts.AuthRecheck <= 0 || opts.AuthRecheck+opts.StreamPoll > MaxAuthRecheck {
		opts.AuthRecheck = MaxAuthRecheck - opts.StreamPoll
	}
	if opts.EventScan <= 0 {
		opts.EventScan = DefaultEventScan
	}
	if opts.MaxStreams <= 0 {
		opts.MaxStreams = DefaultMaxStreams
	}
	spec, err := loadSpec()
	if err != nil {
		return nil, err
	}
	s := &Server{opts: opts, log: opts.Logger, spec: spec, stop: make(chan struct{})}
	hubCtx, hubStop := context.WithCancel(context.Background())
	s.hub = newStreamHub(hubCtx, opts.Store, opts.StreamPoll, func(ctx context.Context, err error) {
		s.log.ErrorContext(ctx, "event stream poller", "error", err)
	})
	s.stopHub = hubStop
	if opts.MCP != nil {
		h, err := opts.MCP(restTransport{s: s})
		if err != nil {
			hubStop()
			return nil, fmt.Errorf("api: mcp: %w", err)
		}
		s.mcp = h
	}
	router := chi.NewRouter()
	router.NotFound(func(w http.ResponseWriter, r *http.Request) {
		problem(SlugNotFound, "no such resource").write(w, r)
	})
	strict := NewStrictHandlerWithOptions(s, nil, StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			problem(SlugBadRequest, "%s", firstLine(err)).write(w, r)
		},
		ResponseErrorHandlerFunc: s.responseError,
	})
	s.gen = HandlerWithOptions(strict, ChiServerOptions{
		BaseRouter: router,
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			problem(SlugBadRequest, "%s", firstLine(err)).write(w, r)
		},
	})
	return s, nil
}

func firstLine(err error) string {
	s, _, _ := strings.Cut(err.Error(), "\n")
	return s
}

// Drain makes every new request get 503 unavailable with Retry-After and
// closes the open event streams. It is called once shutdown starts.
func (s *Server) Drain() {
	s.draining.Store(true)
	if s.stopOnce.CompareAndSwap(false, true) {
		close(s.stop)
		s.stopHub()
	}
}

// reqState is what the front of the pipeline hands to the handlers.
type reqState struct {
	op   *operation
	body []byte // the raw request body, validated against the spec
	// schemaErrs are the body's schema errors, for the operations that
	// report them together with the resource's own rules (deferredOps).
	schemaErrs []fieldError
	token      string // for the event stream's periodic recheck; never logged
	mcp        bool   // the request is for MCPPath
	id         string // request id, in the access log and 500 details
}

type reqStateKey struct{}

func stateFrom(ctx context.Context) *reqState {
	st, _ := ctx.Value(reqStateKey{}).(*reqState)
	if st == nil {
		return &reqState{}
	}
	return st
}

// ServeHTTP runs the pipeline: drain check, route, authentication and scope
// (before anything that could reveal whether a resource exists), body limits,
// validation against the specification, then the generated router and the
// handlers. MCPPath takes the same drain check and authentication, then
// serveMCP; its scopes are checked per tool call by the REST calls.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &recorder{ResponseWriter: w}
	st := &reqState{id: s.opts.NewID()}
	var principal Principal
	defer func() { //nolint:contextcheck // r carries the request context
		if v := recover(); v != nil {
			if v == http.ErrAbortHandler { //nolint:errorlint // the sentinel is compared by identity
				panic(v)
			}
			s.log.ErrorContext(r.Context(), "api handler panicked", "request_id", st.id, "panic", fmt.Sprint(v))
			if !rec.wrote {
				problem(SlugInternalError, "internal error; see the server log for request %s", st.id).write(rec, r)
			}
		}
		s.accessLog(r, rec, st, principal, time.Since(start))
	}()

	st.mcp = s.mcp != nil && r.URL.Path == MCPPath // no operation has this path
	// A REST call that a tool of an accepted /mcp request makes in memory is
	// part of that in-flight request: it completes during the drain, as the
	// rest of an in-flight REST request does. Only restTransport sets the key.
	if s.draining.Load() && !fromMCP(r.Context()) {
		problem(SlugUnavailable, "the instance is shutting down; retry on another instance or later").
			withHeader("Retry-After", fmt.Sprint(RetryAfterSeconds)).write(rec, r)
		return
	}
	op, vars, allowed := s.spec.match(r.Method, r.URL.EscapedPath())
	st.op = op

	token, sent := bearerToken(r)
	st.token = token // also for the access log: never logged, only compared
	if token == "" {
		unauthorized(sent).write(rec, r)
		return
	}
	p, err := s.opts.Auth.Authenticate(r.Context(), token)
	switch {
	case errors.Is(err, ErrUnauthenticated):
		unauthorized(true).write(rec, r)
		return
	case err != nil:
		s.log.ErrorContext(r.Context(), "authenticate", "request_id", st.id, "error", err)
		problem(SlugInternalError, "internal error; see the server log for request %s", st.id).write(rec, r)
		return
	case p.Scope < ScopeRead:
		unauthorized(true).write(rec, r)
		return
	}
	principal = p

	if st.mcp {
		s.serveMCP(rec, r)
		return
	}
	if op == nil {
		if len(allowed) > 0 {
			slices.Sort(allowed)
			methodNotAllowed(r.Method, allowed).write(rec, r)
			return
		}
		problem(SlugNotFound, "no such resource").write(rec, r)
		return
	}
	if p.Scope < op.Scope {
		pr := problem(SlugForbidden, "this operation needs the %s scope; the token has %s", op.Scope, p.Scope)
		pr.RequiredScope = op.Scope.String()
		pr.write(rec, r)
		return
	}

	body, schemaErrs, perr := s.checkRequest(r, op, vars)
	st.schemaErrs = schemaErrs
	if perr != nil {
		perr.write(rec, r)
		return
	}
	if len(schemaErrs) > 0 {
		s.deferredProblem(op, vars["name"], body, schemaErrs).write(rec, r)
		return
	}
	st.body = body
	ctx := context.WithValue(r.Context(), principalKey{}, p)
	ctx = context.WithValue(ctx, reqStateKey{}, st)
	r = r.WithContext(ctx)
	if body != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	}
	s.gen.ServeHTTP(rec, r)
}

// checkRequest enforces the body limits and validates the parameters and the
// body against the operation's specification.
//
// The PUT operations of deferredOps get their schema errors back instead of
// a 422, so that their handlers list them with the errors of the resource's
// own rules: a 422 lists every error found (spec docs/api.md).
func (s *Server) checkRequest(r *http.Request, op *operation, vars map[string]string) ([]byte, []fieldError, *ProblemDetail) {
	var body []byte
	if op.Body != nil {
		if r.ContentLength > MaxBodyBytes {
			return nil, nil, problem(SlugPayloadTooLarge, "the request body is larger than 1 MiB")
		}
		b, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
		if err != nil {
			return nil, nil, problem(SlugBadRequest, "reading the request body failed")
		}
		if len(b) > MaxBodyBytes {
			return nil, nil, problem(SlugPayloadTooLarge, "the request body is larger than 1 MiB")
		}
		if len(b) > 0 || op.BodyRequired {
			mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mt != "application/json" {
				return nil, nil, problem(SlugUnsupportedMediaType, "the request body must be application/json")
			}
		}
		if len(b) == 0 && op.BodyRequired {
			return nil, nil, problem(SlugBadRequest, "the request body is required")
		}
		if len(b) > 0 {
			body = b
		}
	}
	if p := s.checkParams(r, op, vars); p != nil {
		return nil, nil, p
	}
	if body == nil {
		return nil, nil, nil
	}
	v, err := decodeJSON(body)
	if err != nil {
		return nil, nil, problem(SlugBadRequest, "the request body is not valid JSON: %s", jsonErrorDetail(err))
	}
	var errs []fieldError
	s.spec.validate(op.Body, v, "", &errs)
	switch {
	case len(errs) == 0:
		return body, nil, nil
	case !op.Statuses[http.StatusUnprocessableEntity]:
		return nil, nil, problem(SlugBadRequest, "request body %s: %s", pointerOrRoot(errs[0].Pointer), errs[0].Detail)
	case deferredOps[op.ID] && !slices.ContainsFunc(errs, func(e fieldError) bool { return e.Pointer == "" }):
		return body, errs, nil
	}
	return nil, nil, validationProblem("request body", errs)
}

// deferredOps validate their body against the schema and the resource's
// rules together.
var deferredOps = map[string]bool{"putWatch": true, "putSink": true, "putSubscription": true}

// deferredProblem is the 422 of a deferredOps body that breaks the schema:
// its schema errors and the errors of the resource's rules, together.
func (s *Server) deferredProblem(op *operation, name string, body []byte, schemaErrs []fieldError) *ProblemDetail {
	var what string
	var rules []fieldError
	switch op.ID {
	case "putWatch":
		what = "watch"
		_, rules = s.parseWatchBody(name, body)
	case "putSink":
		what = "sink"
		var invalid, env []fieldError
		_, invalid, env = s.parseSinkBody(name, body)
		rules = append(invalid, env...)
	case "putSubscription":
		what = "subscription"
		_, rules = s.parseSubscriptionBody(name, body)
	}
	return validationProblem(what, mergeErrors(schemaErrs, rules))
}

// mergeErrors lists the schema errors, then the errors of the resource's
// rules that are not at, above or under a pointer the schema reported.
func mergeErrors(schema, rules []fieldError) []fieldError {
	out := slices.Clone(schema)
	for _, r := range rules {
		if !slices.ContainsFunc(schema, func(e fieldError) bool {
			return r.Pointer == e.Pointer || strings.HasPrefix(r.Pointer, e.Pointer+"/") || strings.HasPrefix(e.Pointer, r.Pointer+"/")
		}) {
			out = append(out, r)
		}
	}
	return out
}

func pointerOrRoot(p string) string {
	if p == "" {
		return "(root)"
	}
	return p
}

// jsonErrorDetail describes a decoding error. encoding/json quotes at most
// one character of the input, never a value.
func jsonErrorDetail(err error) string { return err.Error() }

// responseError writes the error a handler returned.
func (s *Server) responseError(w http.ResponseWriter, r *http.Request, err error) {
	var p *ProblemDetail
	if errors.As(err, &p) {
		p.write(w, r)
		return
	}
	st := stateFrom(r.Context())
	if r.Context().Err() != nil {
		return // the client went away
	}
	s.log.ErrorContext(r.Context(), "api request failed", "request_id", st.id, "error", err)
	problem(SlugInternalError, "internal error; see the server log for request %s", st.id).write(w, r)
}

// recorder captures the status and size of a response for the access log.
// It keeps Flush and Unwrap working for the event stream.
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.status, r.wrote = http.StatusOK, true
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach Flush and SetWriteDeadline.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Flush implements http.Flusher.
func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// accessLog writes one structured record per request. It never logs a
// header, the query string or a body, so no token, secret or sink URL can
// reach it; a path segment that looks like a credential is replaced.
func (s *Server) accessLog(r *http.Request, rec *recorder, st *reqState, p Principal, d time.Duration) {
	route := ""
	switch {
	case st.op != nil:
		route = st.op.Path
	case st.mcp:
		route = MCPPath
	}
	status := rec.status
	if !rec.wrote {
		status = http.StatusOK
	}
	clientErr := r.Context().Err()
	if clientErr != nil && !rec.wrote {
		// The client went away before anything was written: 499, as nginx
		// logs it. A stream that ends this way keeps its 200.
		status = StatusClientClosed
	}
	attrs := []slog.Attr{
		slog.String("request_id", st.id),
		slog.String("method", r.Method),
		slog.String("path", safePath(r.URL.Path, st.token)),
		slog.String("route", route),
		slog.Int("status", status),
		slog.Int64("bytes", rec.bytes),
		slog.Float64("duration_ms", float64(d.Microseconds())/1000),
	}
	if p.ID != "" {
		attrs = append(attrs, slog.String("principal", p.ID), slog.String("scope", p.Scope.String()))
	}
	if clientErr != nil {
		attrs = append(attrs, slog.String("client_error", clientErr.Error()))
	}
	if fromMCP(r.Context()) {
		attrs = append(attrs, slog.String("via", "mcp"))
	}
	s.log.LogAttrs(r.Context(), slog.LevelInfo, "api request", attrs...)
}

// safePath returns the request path, with every segment that looks like a
// credential or equals the request's bearer token replaced, so a token pasted
// into a URL never reaches the log.
func safePath(p, token string) string {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		if config.LooksLikeToken(seg) || len(seg) > 128 ||
			(token != "" && subtle.ConstantTimeCompare([]byte(seg), []byte(token)) == 1) {
			segs[i] = "[redacted]"
		}
	}
	return strings.Join(segs, "/")
}
