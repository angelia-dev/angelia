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
	DefaultMaxMemoryPages     = 1024 // 64 MiB
	DefaultMaxResponseBytes   = 10 << 20
	DefaultMaxOutputBytes     = 16 << 20
)

// Options configures a Source.
type Options struct {
	// HTTPClient sends every hop of every http_request. Production passes
	// the netguard client (netguard.Client.HTTPClient), so the SSRF guard,
	// the per-host rate limits and the retries apply. The host wraps its
	// Transport to enforce allowed_hosts on every hop and drops its Jar.
	// Required, with a non-nil Transport.
	HTTPClient *http.Client
	// Config is the plugin's config map (Extism config), for example
	// {"token": "..."}. Every key must be declared in the manifest's
	// config_schema properties.
	Config map[string]string
	// Logger receives the plugin's log host function calls and the host's
	// warnings. Default: discard.
	Logger *slog.Logger
	// MaxRequestsPerCall bounds the http_request calls of one List call
	// (pagination included). Default 100.
	MaxRequestsPerCall int
	// CallTimeout bounds one Validate or List call, HTTP included; the
	// plugin is interrupted when it expires. Default 2m.
	CallTimeout time.Duration
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
	setDefault(&opts.MaxMemoryPages, DefaultMaxMemoryPages)
	setDefault(&opts.MaxResponseBytes, DefaultMaxResponseBytes)
	setDefault(&opts.MaxOutputBytes, DefaultMaxOutputBytes)
	setDefault(&opts.MaxIdle, runtime.GOMAXPROCS(0))
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}

	s := &Source{
		kind:    m.Kind,
		allowed: make(map[string]bool, len(m.AllowedHosts)),
		opts:    opts,
		log:     opts.Logger.With("plugin", m.Kind),
	}
	for _, h := range m.AllowedHosts {
		n, _ := normAllowedHost(h) // checked by Validate
		s.allowed[n] = true
	}
	hc := opts.HTTPClient
	s.client = &http.Client{
		Transport:     &allowlistTransport{base: hc.Transport, allowed: s.allowed},
		CheckRedirect: hc.CheckRedirect,
		Timeout:       hc.Timeout,
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
	out, err := s.call(context.Background(), "validate", in, false)
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
	out, err := s.call(ctx, "list", in, true)
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

// callState is the per-call state the host functions see through the
// context: whether the network is allowed, the request count and the first
// policy violation.
type callState struct {
	allowNet bool
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

// takeRequest counts one request and reports whether the budget allows it.
func (c *callState) takeRequest(limit int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests++
	return c.requests <= limit
}

func (s *Source) call(ctx context.Context, fn string, in []byte, allowNet bool) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("wasm %s: %s: %w", s.kind, fn, err)
	}
	ctx, cancel := context.WithTimeout(ctx, s.opts.CallTimeout)
	defer cancel()
	cs := &callState{allowNet: allowNet}
	ctx = context.WithValue(ctx, callKey{}, cs)

	p, err := s.instance(ctx)
	if err != nil {
		return nil, fmt.Errorf("wasm %s: %s: %w", s.kind, fn, err)
	}
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
	p.SetLogger(s.extismLog)
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
// which are off unless the process sets extism.SetLogLevel.
func (s *Source) extismLog(level extism.LogLevel, msg string) {
	s.log.Log(context.Background(), slogLevel(level.String()), truncate(msg, maxLogMessage))
}

// sortedKeys is used for deterministic error messages.
func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
