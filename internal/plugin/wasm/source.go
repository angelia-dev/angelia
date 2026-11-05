package wasm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	extism "github.com/extism/go-sdk"
	"github.com/tetratelabs/wazero"

	"github.com/angelia-dev/angelia/internal/source"
)

// Defaults applied by New to zero Options fields.
const (
	DefaultMaxRequestsPerCall = 100
	DefaultCallTimeout        = 2 * time.Minute
	DefaultValidateTimeout    = 2 * time.Second
	DefaultMaxMemoryPages     = 1024 // 64 MiB
	DefaultMaxResponseBytes   = 10 << 20
	DefaultMaxOutputBytes     = 16 << 20
)

// Options configures a Source.
type Options struct {
	// HTTPClient sends every hop of every http_request. It must be the
	// netguard client (netguard.Client.HTTPClient), so that the SSRF guard,
	// the per-host rate limits and the retries apply: its Transport must
	// implement CredentialHeaders, which decides the headers reserved for
	// host-injected credentials. The host wraps the Transport to enforce
	// allowed_hosts on every hop and drops its Jar. Required.
	HTTPClient *http.Client
	// Config is the plugin's config map (Extism config). Every key must be
	// declared in the manifest's config_schema properties. It is visible
	// to the plugin: put tokens in Credentials, not here.
	Config map[string]string
	// AllowedHosts extends the manifest's allowed_hosts with hosts the
	// operator configured when installing the plugin, for example a
	// registry mirror. Same rules as the manifest's entries. Optional.
	AllowedHosts []string
	// Credentials are injected by the host into the requests to the hosts
	// they are bound to; the host never hands them to the plugin. Optional.
	Credentials []Credential
	// Logger receives the plugin's log host function calls and the host's
	// warnings. Default: discard.
	Logger *slog.Logger
	// MaxRequestsPerCall bounds the HTTP requests of one List call:
	// every http_request and every redirect hop it follows (pagination
	// included). Default 100.
	MaxRequestsPerCall int
	// CallTimeout bounds one List call, HTTP included; the plugin is
	// interrupted when it expires. Default 2m.
	CallTimeout time.Duration
	// ValidateTimeout bounds one Validate call, which has no network
	// access and should be quick. Default 2s.
	ValidateTimeout time.Duration
	// MaxMemoryPages caps the plugin's linear memory in 64 KiB pages.
	// Default 1024 (64 MiB).
	MaxMemoryPages uint32
	// MaxResponseBytes caps one response body handed to the plugin.
	// Default 10 MiB.
	MaxResponseBytes int64
	// MaxOutputBytes caps the output of one call. Default 16 MiB.
	MaxOutputBytes int
	// MaxIdle is the number of idle plugin instances kept for reuse.
	// Default GOMAXPROCS.
	MaxIdle int
	// CompilationCache, if set, is shared by the Sources of a process so
	// that a plugin's machine code is compiled once. Optional.
	CompilationCache wazero.CompilationCache
}

// CredentialHeaders is implemented by the transport of the netguard client
// (*netguard.Client): it reports whether a header name puts a request on a
// host's authenticated budget. The host reserves those headers for the
// credentials it injects (ADR 0011, amendment of #184).
type CredentialHeaders interface {
	IsCredentialHeader(name string) bool
}

// ErrHostNotAllowed is wrapped by the error of a call in which the plugin
// tried to reach a host that is not in its manifest's allowed_hosts,
// whatever the plugin did with the http_request error.
var ErrHostNotAllowed = errors.New("wasm: host not in allowed_hosts")

// ErrPolicy is wrapped by the other host-side refusals that fail a call
// even if the plugin ignores them: network access during Validate and an
// exhausted request budget.
var ErrPolicy = errors.New("wasm: plugin violated host policy")

// Source is a source.Source implemented by a WASM plugin. It is safe for
// concurrent use: each call runs on its own plugin instance, taken from a
// small pool.
type Source struct {
	kind     string
	allowed  map[string]bool
	grants   map[string]*HostGrant
	creds    map[string]Credential
	credHdrs CredentialHeaders
	opts     Options
	client   *http.Client
	log      *slog.Logger
	compiled *extism.CompiledPlugin

	mu     sync.Mutex
	idle   []*extism.Plugin
	closed bool
}

var _ source.Source = (*Source)(nil)

// New compiles the plugin. The context only bounds compilation.
func New(ctx context.Context, m Manifest, wasm []byte, opts Options) (*Source, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if opts.HTTPClient == nil || opts.HTTPClient.Transport == nil {
		return nil, errors.New("wasm: Options.HTTPClient with a Transport is required")
	}
	// Fail closed: without the client's own credential-header rule, a
	// plugin could claim a host's authenticated budget with a header the
	// host does not know to reserve.
	creds, ok := opts.HTTPClient.Transport.(CredentialHeaders)
	if !ok {
		return nil, errors.New("wasm: Options.HTTPClient must be the netguard client: its Transport does not implement CredentialHeaders")
	}
	declared, err := m.configKeys()
	if err != nil {
		return nil, err
	}
	for k := range opts.Config {
		if !declared[k] {
			return nil, fmt.Errorf("wasm %s: config key %q is not declared in config_schema", m.Kind, k)
		}
	}
	setDefault(&opts.MaxRequestsPerCall, DefaultMaxRequestsPerCall)
	setDefault(&opts.CallTimeout, DefaultCallTimeout)
	setDefault(&opts.ValidateTimeout, DefaultValidateTimeout)
	setDefault(&opts.MaxMemoryPages, DefaultMaxMemoryPages)
	setDefault(&opts.MaxResponseBytes, DefaultMaxResponseBytes)
	setDefault(&opts.MaxOutputBytes, DefaultMaxOutputBytes)
	setDefault(&opts.MaxIdle, runtime.GOMAXPROCS(0))
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}

	s := &Source{
		kind:     m.Kind,
		credHdrs: creds,
		opts:     opts,
		log:      opts.Logger.With("plugin", m.Kind),
	}
	if err := s.install(m, opts); err != nil {
		return nil, err
	}
	hc := opts.HTTPClient
	inner := hc.CheckRedirect
	s.client = &http.Client{
		Transport: &allowlistTransport{base: hc.Transport, allowed: s.allowed, creds: s.creds},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return s.checkRedirect(req, via, inner)
		},
		Timeout: hc.Timeout,
		// No Jar: cookies must not leak between plugins or targets.
	}

	em := extism.Manifest{
		Wasm:   []extism.Wasm{extism.WasmData{Data: wasm}},
		Config: opts.Config,
		Memory: &extism.ManifestMemory{MaxPages: opts.MaxMemoryPages},
		// AllowedHosts stays empty: Extism's own http_request, which would
		// bypass netguard, then refuses every host.
	}
	rc := wazero.NewRuntimeConfig().WithCloseOnContextDone(true) // interrupt the guest when the call's context ends
	if opts.CompilationCache != nil {
		rc = rc.WithCompilationCache(opts.CompilationCache)
	}
	cfg := extism.PluginConfig{RuntimeConfig: rc}
	s.compiled, err = extism.NewCompiledPlugin(ctx, em, cfg, s.hostFunctions())
	if err != nil {
		return nil, fmt.Errorf("wasm %s: compile: %w", m.Kind, err)
	}
	s.log.InfoContext(ctx, "wasm plugin installed", "allowed_hosts", s.Hosts())
	return s, nil
}

func setDefault[T comparable](p *T, v T) {
	var zero T
	if *p == zero {
		*p = v
	}
}

// Kind returns the manifest's kind.
func (s *Source) Kind() string { return s.kind }

// Validate rejects a target of another kind, then asks the plugin's
// validate export. The plugin gets no network access.
func (s *Source) Validate(t source.Target) error {
	if t.Kind != s.kind {
		return fmt.Errorf("%w: kind %q, want %q", source.ErrInvalidTarget, t.Kind, s.kind)
	}
	in, err := json.Marshal(wireTarget(t))
	if err != nil {
		return fmt.Errorf("wasm %s: validate: %w", s.kind, err)
	}
	out, err := s.call(context.Background(), "validate", in, false, s.opts.ValidateTimeout)
	if err != nil {
		return err
	}
	var v wireValidateOutput
	if err := json.Unmarshal(out, &v); err != nil {
		return fmt.Errorf("wasm %s: validate: %w: %w", s.kind, errPluginOutput, err)
	}
	if v.Reason != "" {
		return fmt.Errorf("%w: %s", source.ErrInvalidTarget, v.Reason)
	}
	return nil
}

// List calls the plugin's list export.
func (s *Source) List(ctx context.Context, t source.Target, st source.FetchState) (source.Result, error) {
	in, err := json.Marshal(wireListInput{
		Target: wireTarget(t),
		State:  wireState{ETag: st.ETag, LastModified: st.LastModified, Cursor: st.Cursor},
	})
	if err != nil {
		return source.Result{}, fmt.Errorf("wasm %s: list: %w", s.kind, err)
	}
	out, err := s.call(ctx, "list", in, true, s.opts.CallTimeout)
	if err != nil {
		return source.Result{}, err
	}
	res, err := toResult(out, st)
	if err != nil {
		return source.Result{}, fmt.Errorf("wasm %s: list: %w", s.kind, err)
	}
	return res, nil
}

// Close releases the compiled plugin and its idle instances. Calls in
// flight fail.
func (s *Source) Close(ctx context.Context) error {
	s.mu.Lock()
	idle := s.idle
	s.idle, s.closed = nil, true
	s.mu.Unlock()
	errs := make([]error, 0, len(idle)+1)
	for _, p := range idle {
		errs = append(errs, p.Close(ctx))
	}
	errs = append(errs, s.compiled.Close(ctx))
	return errors.Join(errs...)
}

type callKey struct{}

// Per-call log budget, shared by the log host function and Extism's own
// log functions. Once either bound is crossed, further entries are dropped
// with a single warning.
const (
	maxLogEntriesPerCall = 256
	maxLogBytesPerCall   = 256 << 10
)

// callState is the per-call state the host functions see through the
// context: whether the network is allowed, the request count, the log
// budget and the first policy violation.
type callState struct {
	allowNet bool
	mu       sync.Mutex
	requests int
	violated error

	secrets []string // tokens injected during the call, scrubbed from responses

	logEntries   int
	logBytes     int64
	logExhausted bool
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

// takeRequest counts one request and reports whether the budget allows it.
func (c *callState) takeRequest(limit int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests++
	return c.requests <= limit
}

// takeLog charges one log entry of n bytes to the budget. It reports
// whether to emit the entry, and whether this is the first refused entry,
// which gets the single "budget exhausted" warning.
func (c *callState) takeLog(n int64) (ok, first bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.logExhausted {
		return false, false
	}
	c.logEntries++
	c.logBytes += n
	if c.logEntries > maxLogEntriesPerCall || c.logBytes > maxLogBytesPerCall {
		c.logExhausted = true
		return false, true
	}
	return true, false
}

func (s *Source) call(ctx context.Context, fn string, in []byte, allowNet bool, timeout time.Duration) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("wasm %s: %s: %w", s.kind, fn, err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cs := &callState{allowNet: allowNet}
	ctx = context.WithValue(ctx, callKey{}, cs)

	p, err := s.instance(ctx)
	if err != nil {
		return nil, fmt.Errorf("wasm %s: %s: %w", s.kind, fn, err)
	}
	// Extism's logger has no context: bind it to this call's budget.
	p.SetLogger(func(l extism.LogLevel, msg string) { s.extismLog(ctx, cs, l, msg) })
	_, out, err := p.CallWithContext(ctx, fn, in)
	if err != nil {
		// A trap or an interrupted call can leave the instance in any
		// state: never reuse it.
		_ = p.Close(context.WithoutCancel(ctx))
	} else {
		s.release(ctx, p)
	}
	if v := cs.violation(); v != nil {
		if err != nil {
			return nil, fmt.Errorf("wasm %s: %s: %w (plugin: %v)", s.kind, fn, v, err) //nolint:errorlint // the violation is the cause; the plugin's error is context
		}
		return nil, fmt.Errorf("wasm %s: %s: %w", s.kind, fn, v)
	}
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, fmt.Errorf("wasm %s: %s: %w (%w)", s.kind, fn, cerr, err)
		}
		return nil, fmt.Errorf("wasm %s: %s: %w", s.kind, fn, err)
	}
	// The output is already in host memory here: this bounds what the
	// caller decodes, not what the plugin may allocate (MaxMemoryPages does).
	if len(out) > s.opts.MaxOutputBytes {
		return nil, fmt.Errorf("wasm %s: %s: %w: output of %d bytes exceeds %d", s.kind, fn, errPluginOutput, len(out), s.opts.MaxOutputBytes)
	}
	return out, nil
}

func (s *Source) instance(ctx context.Context) (*extism.Plugin, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("source closed")
	}
	if n := len(s.idle); n > 0 {
		p := s.idle[n-1]
		s.idle = s.idle[:n-1]
		s.mu.Unlock()
		return p, nil
	}
	s.mu.Unlock()
	p, err := s.compiled.Instance(ctx, extism.PluginInstanceConfig{})
	if err != nil {
		return nil, fmt.Errorf("instantiate: %w", err)
	}
	return p, nil
}

func (s *Source) release(ctx context.Context, p *extism.Plugin) {
	s.mu.Lock()
	if !s.closed && len(s.idle) < s.opts.MaxIdle {
		s.idle = append(s.idle, p)
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	_ = p.Close(context.WithoutCancel(ctx))
}

// extismLog receives Extism's own log functions (extism:host/env log_*),
// which are off unless the process sets extism.SetLogLevel. They share the
// call's log budget with the log host function.
func (s *Source) extismLog(ctx context.Context, cs *callState, level extism.LogLevel, msg string) {
	if !s.chargeLog(ctx, cs, int64(len(msg))) {
		return
	}
	s.log.Log(ctx, slogLevel(level.String()), truncate(msg, maxLogMessage))
}

// chargeLog charges one entry of n bytes to the call's log budget and
// reports whether to emit it. The first refusal logs one warning.
func (s *Source) chargeLog(ctx context.Context, cs *callState, n int64) bool {
	ok, first := cs.takeLog(n)
	if first {
		s.log.WarnContext(ctx, "wasm plugin log budget exhausted; dropping further entries of this call",
			"max_entries", maxLogEntriesPerCall, "max_bytes", maxLogBytesPerCall)
	}
	return ok
}

// checkRedirect runs before every redirect hop is sent. It refuses a
// downgrade from https, which would send the Authorization header and the
// rest of the request in clear text (net/http keeps them on a same-host
// redirect), and counts the hop against the call's request budget, then
// applies the injected client's own check.
func (s *Source) checkRedirect(req *http.Request, via []*http.Request, inner func(*http.Request, []*http.Request) error) error {
	if !strings.EqualFold(req.URL.Scheme, "https") {
		for _, v := range via {
			if strings.EqualFold(v.URL.Scheme, "https") {
				return fmt.Errorf("%w: from https to %s://%s", errInsecureRedirect, req.URL.Scheme, req.URL.Host)
			}
		}
	}
	if cs, ok := req.Context().Value(callKey{}).(*callState); ok && !cs.takeRequest(s.opts.MaxRequestsPerCall) {
		err := fmt.Errorf("%w: more than %d HTTP requests, redirects included", ErrPolicy, s.opts.MaxRequestsPerCall)
		cs.violate(err)
		return err
	}
	if inner != nil {
		return inner(req, via)
	}
	if len(via) >= defaultMaxRedirects {
		return fmt.Errorf("stopped after %d redirects", defaultMaxRedirects)
	}
	return nil
}

// defaultMaxRedirects is net/http's own limit, used when the injected
// client has no CheckRedirect.
const defaultMaxRedirects = 10

// errInsecureRedirect refuses a redirect from https to another scheme.
var errInsecureRedirect = errors.New("redirect downgrades https")

// sortedKeys is used for deterministic error messages.
func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
