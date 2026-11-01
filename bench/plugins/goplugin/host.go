package goplugin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
)

// Defaults, the same as the WASM host's (internal/plugin/wasm).
const (
	DefaultMaxRequestsPerCall = 100
	DefaultCallTimeout        = 2 * time.Minute
	DefaultValidateTimeout    = 2 * time.Second
	DefaultMaxResponseBytes   = 10 << 20
)

// Options configures a Source.
type Options struct {
	// Kind is the source kind the plugin serves. Required.
	Kind string
	// Command returns a fresh command that starts the plugin binary. It is
	// called again when the plugin process has died. The plugin does not
	// inherit the core's environment: only cmd.Env is passed. Required.
	Command func() *exec.Cmd
	// Checksum, if set, is the SHA-256 of the plugin binary; go-plugin
	// refuses to start a binary that does not match.
	Checksum []byte
	// AllowedHosts are the hosts the plugin may reach through the host's
	// HTTP service, exact match on every hop (ADR 0011).
	AllowedHosts []string
	// HTTPClient sends every hop; production passes the netguard client.
	// Required, with a non-nil Transport.
	HTTPClient *http.Client
	// Config is the plugin config map, sent once after start.
	Config map[string]string
	// Logger receives go-plugin's logs and the plugin's stderr. Default:
	// discard.
	Logger hclog.Logger

	MaxRequestsPerCall int
	CallTimeout        time.Duration
	ValidateTimeout    time.Duration
	MaxResponseBytes   int64
}

// ErrHostNotAllowed is wrapped by the error of a call in which the plugin
// asked the host for a host outside AllowedHosts.
var ErrHostNotAllowed = errors.New("goplugin: host not in allowed_hosts")

// ErrPolicy is wrapped when the plugin exhausted its request budget.
var ErrPolicy = errors.New("goplugin: plugin violated host policy")

// Source is a source.Source served by a go-plugin subprocess. One process
// serves every call of the Source, concurrently; it is restarted on the
// next call after it dies.
type Source struct {
	opts    Options
	allowed map[string]bool
	client  *http.Client
	calls   sync.Map // call id -> *callState

	mu     sync.Mutex
	pc     *goplugin.Client
	rem    *remote
	closed bool
}

var _ source.Source = (*Source)(nil)

// New starts the plugin process and configures it.
func New(ctx context.Context, opts Options) (*Source, error) {
	if opts.Kind == "" || opts.Command == nil {
		return nil, errors.New("goplugin: Options.Kind and Options.Command are required")
	}
	if opts.HTTPClient == nil || opts.HTTPClient.Transport == nil {
		return nil, errors.New("goplugin: Options.HTTPClient with a Transport is required")
	}
	setDefault(&opts.MaxRequestsPerCall, DefaultMaxRequestsPerCall)
	setDefault(&opts.CallTimeout, DefaultCallTimeout)
	setDefault(&opts.ValidateTimeout, DefaultValidateTimeout)
	setDefault(&opts.MaxResponseBytes, DefaultMaxResponseBytes)
	if opts.Logger == nil {
		opts.Logger = hclog.NewNullLogger()
	}
	s := &Source{opts: opts, allowed: map[string]bool{}}
	for _, h := range opts.AllowedHosts {
		s.allowed[normHost(h)] = true
	}
	hc := *opts.HTTPClient
	inner := hc.CheckRedirect
	hc.Transport = &allowlist{base: hc.Transport, allowed: s.allowed}
	hc.CheckRedirect = func(r *http.Request, via []*http.Request) error { return s.checkRedirect(r, via, inner) }
	hc.Jar = nil
	s.client = &hc

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.startLocked(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func setDefault[T comparable](v *T, d T) {
	var zero T
	if *v == zero {
		*v = d
	}
}

func (s *Source) startLocked(ctx context.Context) error {
	cfg := &goplugin.ClientConfig{
		HandshakeConfig:  Handshake,
		Plugins:          goplugin.PluginSet{PluginName: &sourcePlugin{}},
		Cmd:              s.opts.Command(),
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		AutoMTLS:         true,
		SkipHostEnv:      true, // the core's environment holds its secrets
		Logger:           s.opts.Logger,
	}
	if s.opts.Checksum != nil {
		cfg.SecureConfig = &goplugin.SecureConfig{Checksum: s.opts.Checksum, Hash: sha256.New()}
	}
	pc := goplugin.NewClient(cfg)
	rem, err := connect(ctx, pc, s)
	if err != nil {
		pc.Kill()
		return fmt.Errorf("goplugin %s: start: %w", s.opts.Kind, err)
	}
	s.pc, s.rem = pc, rem
	return nil
}

func connect(ctx context.Context, pc *goplugin.Client, s *Source) (*remote, error) {
	proto, err := pc.Client()
	if err != nil {
		return nil, err
	}
	raw, err := proto.Dispense(PluginName)
	if err != nil {
		return nil, err
	}
	rem, ok := raw.(*remote)
	if !ok {
		return nil, fmt.Errorf("unexpected plugin type %T", raw)
	}
	id := rem.broker.NextId()
	go rem.broker.AcceptAndServe(id, func(opts []grpc.ServerOption) *grpc.Server {
		srv := grpc.NewServer(opts...)
		srv.RegisterService(&hostDesc, s)
		return srv
	})
	if _, err := invoke[Empty](ctx, rem.cc, sourceService, "Configure", &ConfigureRequest{Config: s.opts.Config, BrokerID: id}); err != nil {
		return nil, fmt.Errorf("configure: %w", err)
	}
	return rem, nil
}

// conn returns the running plugin, restarting a dead one.
func (s *Source) conn(ctx context.Context) (*remote, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("goplugin %s: source closed", s.opts.Kind)
	}
	if s.pc.Exited() {
		s.pc.Kill()
		if err := s.startLocked(ctx); err != nil {
			return nil, err
		}
	}
	return s.rem, nil
}

// Close kills the plugin process.
func (s *Source) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.pc.Kill()
}

// Kind implements source.Source.
func (s *Source) Kind() string { return s.opts.Kind }

// Validate implements source.Source. The plugin process does the check.
func (s *Source) Validate(t source.Target) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.ValidateTimeout)
	defer cancel()
	rem, err := s.conn(ctx)
	if err != nil {
		return err
	}
	out, err := invoke[ValidateResponse](ctx, rem.cc, sourceService, "Validate", toTarget(t))
	if err != nil {
		return fmt.Errorf("goplugin %s: validate: %w", s.opts.Kind, err)
	}
	if out.Reason != "" {
		return fmt.Errorf("%w: %s", source.ErrInvalidTarget, out.Reason)
	}
	return nil
}

// List implements source.Source.
func (s *Source) List(ctx context.Context, t source.Target, st source.FetchState) (source.Result, error) {
	if err := ctx.Err(); err != nil {
		return source.Result{}, fmt.Errorf("goplugin %s: list: %w", s.opts.Kind, err)
	}
	ctx, cancel := context.WithTimeout(ctx, s.opts.CallTimeout)
	defer cancel()
	id := rand.Text()
	cs := &callState{ctx: ctx}
	s.calls.Store(id, cs)
	defer s.calls.Delete(id)

	rem, err := s.conn(ctx)
	if err != nil {
		return source.Result{}, err
	}
	out, err := invoke[ListResponse](ctx, rem.cc, sourceService, "List", &ListRequest{
		CallID: id,
		Target: *toTarget(t),
		State:  State{ETag: st.ETag, LastModified: st.LastModified, Cursor: st.Cursor},
	})
	if v := cs.violation(); v != nil {
		return source.Result{}, fmt.Errorf("goplugin %s: list: %w", s.opts.Kind, v)
	}
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return source.Result{}, fmt.Errorf("goplugin %s: list: %w (%w)", s.opts.Kind, cerr, err)
		}
		return source.Result{}, fmt.Errorf("goplugin %s: list: %w", s.opts.Kind, err)
	}
	return toResult(out, st)
}

func toTarget(t source.Target) *Target {
	c := t.Coordinates
	if c == nil {
		c = map[string]string{}
	}
	return &Target{Kind: t.Kind, Coordinates: c}
}

var errPluginOutput = errors.New("goplugin: invalid plugin output")

func toResult(w *ListResponse, in source.FetchState) (source.Result, error) {
	res := source.Result{
		State:       source.FetchState{ETag: w.State.ETag, LastModified: w.State.LastModified, Cursor: w.State.Cursor},
		NotModified: w.NotModified,
	}
	if w.NotModified {
		if len(w.Versions) != 0 {
			return source.Result{}, fmt.Errorf("%w: not_modified with %d versions", errPluginOutput, len(w.Versions))
		}
		if res.State.IsZero() {
			res.State = in
		}
		return res, nil
	}
	if len(w.Versions) > 0 {
		res.Versions = make([]source.Version, len(w.Versions))
	}
	for i, v := range w.Versions {
		if v.Raw == "" {
			return source.Result{}, fmt.Errorf("%w: version %d has an empty raw", errPluginOutput, i)
		}
		var pub time.Time
		if v.PublishedAt != "" {
			t, err := time.Parse(time.RFC3339Nano, v.PublishedAt)
			if err != nil {
				return source.Result{}, fmt.Errorf("%w: version %q: %w", errPluginOutput, v.Raw, err)
			}
			pub = t.UTC()
		}
		res.Versions[i] = source.Version{
			Raw: v.Raw, PublishedAt: pub, Prerelease: v.Prerelease, Yanked: v.Yanked,
			URL: v.URL, NotesURL: v.NotesURL, Metadata: v.Metadata,
		}
	}
	return res, nil
}

// callState is the host's view of one List call.
type callState struct {
	ctx      context.Context //nolint:containedctx // the call's context, for the HTTP requests the plugin makes during it
	mu       sync.Mutex
	requests int
	violated error
}

func (c *callState) violate(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.violated == nil {
		c.violated = err
	}
}

func (c *callState) violation() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.violated
}

func (c *callState) take(limit int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests++
	return c.requests <= limit
}

type callKey struct{}

func failure(code, msg string) *HTTPResponse {
	return &HTTPResponse{Error: &HTTPError{Code: code, Message: msg}}
}

// HTTP implements the Host service. It is the go-plugin counterpart of the
// WASM http_request host function, with the same rules, condensed.
func (s *Source) HTTP(_ context.Context, r *HTTPRequest) (*HTTPResponse, error) {
	v, ok := s.calls.Load(r.CallID)
	if !ok {
		return failure("not_allowed_here", "no List call in progress with this id"), nil
	}
	cs := v.(*callState)
	if !cs.take(s.opts.MaxRequestsPerCall) {
		err := fmt.Errorf("%w: more than %d HTTP requests", ErrPolicy, s.opts.MaxRequestsPerCall)
		cs.violate(err)
		return failure("too_many_requests", err.Error()), nil
	}
	// The call's context, not the RPC's: the request ends with the call.
	ctx := context.WithValue(cs.ctx, callKey{}, cs)
	req, err := newRequest(ctx, r) //nolint:contextcheck // the List call's context, see above
	if err != nil {
		return failure("bad_request", err.Error()), nil
	}
	resp, err := s.client.Do(req) //nolint:gosec // G704: every hop goes through the allowlist and the injected netguard client.
	if err != nil {
		switch {
		case errors.Is(err, ErrHostNotAllowed):
			return failure("host_not_allowed", err.Error()), nil
		case errors.Is(err, ErrPolicy):
			return failure("too_many_requests", err.Error()), nil
		case errors.Is(err, netguard.ErrBlocked):
			cs.violate(err)
			return failure("blocked", err.Error()), nil
		case ctx.Err() != nil:
			return failure("canceled", err.Error()), nil
		}
		return failure("transport", err.Error()), nil
	}
	defer func() { _ = resp.Body.Close() }()
	out := &HTTPResponse{Status: resp.StatusCode, Headers: make(map[string][]string, len(resp.Header))}
	for k, v := range resp.Header {
		out.Headers[strings.ToLower(k)] = v
	}
	got := Validators{ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified")}
	if resp.StatusCode == http.StatusNotModified {
		out.NotModified = true
		if r.Validators != nil {
			out.Validators = *r.Validators
		}
		if got.ETag != "" {
			out.Validators.ETag = got.ETag
		}
		if got.LastModified != "" {
			out.Validators.LastModified = got.LastModified
		}
		return out, nil
	}
	out.Validators = got
	body, err := io.ReadAll(io.LimitReader(resp.Body, s.opts.MaxResponseBytes+1))
	switch {
	case errors.Is(err, netguard.ErrBodyTooLarge), err == nil && int64(len(body)) > s.opts.MaxResponseBytes:
		return failure("too_large", fmt.Sprintf("response body exceeds %d bytes", s.opts.MaxResponseBytes)), nil
	case err != nil:
		return failure("transport", err.Error()), nil
	}
	out.Body = body
	return out, nil
}

// reservedHeaders are owned by the transport (ADR 0011).
var reservedHeaders = map[string]bool{
	"Connection": true, "Content-Length": true, "Expect": true, "Host": true, "Keep-Alive": true,
	"Proxy-Authorization": true, "Proxy-Connection": true, "Te": true, "Transfer-Encoding": true, "Upgrade": true,
}

func newRequest(ctx context.Context, r *HTTPRequest) (*http.Request, error) {
	method := strings.ToUpper(r.Method)
	switch method {
	case "":
		method = http.MethodGet
	case http.MethodGet, http.MethodHead, http.MethodPost:
	default:
		return nil, fmt.Errorf("method %q not allowed (GET, HEAD, POST)", r.Method)
	}
	var body io.Reader
	if len(r.Body) > 0 {
		body = bytes.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.URL, body)
	if err != nil {
		return nil, err
	}
	if req.URL.User != nil {
		return nil, errors.New("credentials in the URL are not allowed")
	}
	for k, v := range r.Headers {
		ck := http.CanonicalHeaderKey(k)
		if reservedHeaders[ck] {
			return nil, fmt.Errorf("header %q is set by the host", k)
		}
		if _, dup := req.Header[ck]; dup {
			return nil, fmt.Errorf("header %q given twice", ck)
		}
		req.Header.Set(ck, v) // net/http's transport rejects invalid names and values
	}
	if v := r.Validators; v != nil {
		if v.ETag != "" && req.Header.Get("If-None-Match") == "" {
			req.Header.Set("If-None-Match", v.ETag)
		}
		if v.LastModified != "" && req.Header.Get("If-Modified-Since") == "" {
			req.Header.Set("If-Modified-Since", v.LastModified)
		}
	}
	return req, nil
}

// allowlist refuses every hop, redirects included, to a host that is not
// allowed, before netguard (and DNS) sees it, and records the refusal in
// the call so that the call fails even if the plugin ignores it.
type allowlist struct {
	base    http.RoundTripper
	allowed map[string]bool
}

func (a *allowlist) RoundTrip(r *http.Request) (*http.Response, error) {
	scheme := strings.ToLower(r.URL.Scheme)
	h := normHost(r.URL.Hostname())
	if (scheme != "http" && scheme != "https") || !isASCII(h) || !a.allowed[h] {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		err := fmt.Errorf("%w: %s://%s", ErrHostNotAllowed, r.URL.Scheme, r.URL.Host)
		if cs, ok := r.Context().Value(callKey{}).(*callState); ok {
			cs.violate(err)
		}
		return nil, err
	}
	return a.base.RoundTrip(r)
}

func normHost(h string) string { return strings.TrimSuffix(strings.ToLower(h), ".") }

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return false
		}
	}
	return s != ""
}

var errInsecureRedirect = errors.New("redirect downgrades https")

func (s *Source) checkRedirect(req *http.Request, via []*http.Request, inner func(*http.Request, []*http.Request) error) error {
	if !strings.EqualFold(req.URL.Scheme, "https") {
		for _, v := range via {
			if strings.EqualFold(v.URL.Scheme, "https") {
				return fmt.Errorf("%w: to %s://%s", errInsecureRedirect, req.URL.Scheme, req.URL.Host)
			}
		}
	}
	if cs, ok := req.Context().Value(callKey{}).(*callState); ok && !cs.take(s.opts.MaxRequestsPerCall) {
		err := fmt.Errorf("%w: more than %d HTTP requests, redirects included", ErrPolicy, s.opts.MaxRequestsPerCall)
		cs.violate(err)
		return err
	}
	if inner != nil {
		return inner(req, via)
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return nil
}
