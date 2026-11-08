package netguard

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/angelia-dev/angelia/internal/buildinfo"
	"github.com/angelia-dev/angelia/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
)

// Defaults applied by New to zero Config fields.
const (
	DefaultTimeout      = 30 * time.Second
	DefaultMaxBodyBytes = 10 << 20 // 10 MiB
	DefaultMaxAttempts  = 4
	DefaultBaseBackoff  = 500 * time.Millisecond
	DefaultMaxBackoff   = 30 * time.Second
	DefaultMaxRetryWait = 2 * time.Minute
	maxRedirects        = 10 // net/http's own default
)

// ErrBodyTooLarge is returned when reading a response body past the cap.
var ErrBodyTooLarge = errors.New("response body exceeds size cap")

// Config configures a Client. The zero value is usable.
type Config struct {
	// UserAgent defaults to "angelia/<version> (+https://github.com/angelia-dev/angelia)".
	UserAgent string
	// Timeout bounds one attempt, including reading the body. Default 30s.
	// A non-zero HostLimit.Timeout, or the built-in 2 minutes of the large
	// registry hosts (see HostLimit), overrides it for that host.
	Timeout time.Duration
	// MaxBodyBytes caps response bodies (after transparent gzip decoding). Default
	// 10 MiB. A non-zero HostLimit.MaxBodyBytes, or a built-in cap for the
	// host (registry.npmjs.org, pypi.org, index.crates.io: 64 MiB), overrides it.
	MaxBodyBytes int64
	// MaxAttempts is the total number of attempts for a retryable request,
	// including the first. Default 4; 1 disables retries.
	MaxAttempts int
	// BaseBackoff and MaxBackoff bound the exponential backoff with full
	// jitter used when the server gives no Retry-After. Defaults 500ms, 30s.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	// MaxRetryWait is the longest server-requested wait (Retry-After or a
	// rate limit reset, X-RateLimit-* or RateLimit-*) the client sleeps through before retrying.
	// A longer wait, or one past the context deadline, ends the call with
	// the last response; the host stays paused. Default 2m.
	MaxRetryWait time.Duration
	// HostLimits extend DefaultHostLimits, keyed by hostname, and may
	// lower them: the rates and bursts of a host of DefaultHostLimits are
	// capped at its built-in limit, bucket by bucket (ADR 0027 §4); its
	// MaxBodyBytes and Timeout are kept. Client.Limits returns the result.
	// `angelia serve` fills it from http.host_limits.hosts.
	HostLimits map[string]HostLimit
	// FallbackLimit applies to other hosts. Default DefaultFallbackLimit().
	// `angelia serve` fills it from http.host_limits.default.
	FallbackLimit HostLimit
	// ExtraAuthHeaders names request headers, besides Authorization and
	// PRIVATE-TOKEN, whose presence (non-empty value) makes the limiter use a
	// host's Authenticated bucket. Only presence is read; values are never
	// logged. Go-level option: it has no YAML key.
	//
	// net/http forwards custom credential headers on cross-host redirects
	// (it strips only Authorization), so a caller that sends one must guard
	// redirects itself (see apiclient.GuardRedirects, which stops
	// Authorization and PRIVATE-TOKEN but not custom headers).
	ExtraAuthHeaders []string
	// Allow lists prefixes the SSRF guard accepts even though they are
	// loopback, private or link-local. Empty by default.
	Allow []netip.Prefix
	// Resolver is used by the SSRF guard's URL check. Default
	// net.DefaultResolver. The dial-time check sees the addresses the
	// transport's own dialer resolves (see the package doc).
	Resolver Resolver
	// Proxy chooses the proxy of each request of the default transport, as
	// http.Transport.Proxy does. Default http.ProxyFromEnvironment
	// (HTTPS_PROXY, HTTP_PROXY, NO_PROXY). The proxy addresses exempt from
	// the dial-time check are read from it once, in New, by asking it for
	// an http and an https probe URL. Go-level option: it has no YAML key.
	Proxy func(*http.Request) (*url.URL, error)
	// RootCAs, when not nil, is the pool that verifies server certificates
	// of the default transport, for the origin and for an https proxy
	// alike. The caller builds it from the system roots plus
	// http.ca_file (config.LoadCAPool), so it extends the system roots and
	// never replaces them. Certificate verification is never switched
	// off. Ignored when Transport is set.
	RootCAs *x509.CertPool
	// Transport sends each attempt. By default it is an *http.Transport
	// dialling through netguard's own dialer, which checks the address
	// actually connected (dial-time check, see the package doc). A custom
	// Transport, as used in tests, bypasses the dial-time check; the URL
	// checks still apply.
	Transport http.RoundTripper
	// Clock defaults to the system clock.
	Clock Clock
	// Logger defaults to a logger that discards everything.
	Logger *slog.Logger
	// Observer receives measurements for metrics. Nil discards them.
	Observer Observer
}

// Observer receives netguard measurements. Phase 1 exports them to
// Prometheus (internal/metrics). Methods must be safe for concurrent use and
// must not block.
type Observer interface {
	// RateLimitWait is how long one attempt (each retry and each redirect
	// hop is an attempt) waited for the per-host rate limit before it was
	// sent, zero when it did not wait. It is called once per attempt, after
	// the wait ended; a wait cut short by the context is not reported.
	//
	// host is a host of the limits table (DefaultHostLimits plus
	// Config.HostLimits), lower case, or "other" for any other host. Sinks
	// share this client, and their hosts can carry secrets (webhook URLs),
	// so they never reach an observer. Implementations must not be a typed
	// nil pointer stored in the interface: that is not a nil Observer and
	// would panic on the first call.
	RateLimitWait(host string, d time.Duration)
	// ServerPause is called each time a response pauses its host's bucket:
	// reason is PauseRetryAfter (a Retry-After header) or
	// PauseRateLimitReset (an X-RateLimit-* or RateLimit-* pair with
	// Remaining at 0). host is reported as for RateLimitWait.
	ServerPause(host, reason string)
	// Response is called once per attempt that was sent, with the class of
	// its outcome: ClassOK ("2xx"), ClassNotModified ("304"), "3xx", "4xx",
	// ClassTooManyRequests ("429"), "5xx", or ClassError when the attempt
	// got no response (a transport error or a timeout). host is reported as
	// for RateLimitWait.
	Response(host, class string)
	// DeadlineRefusal is called when the limiter refuses a wait at once
	// because the host's next slot comes after the caller's deadline: the
	// sign that a host's limit is too low for the demand (ADR 0027 §8).
	// host is reported as for RateLimitWait.
	DeadlineRefusal(host string)
}

// Reasons of Observer.ServerPause.
const (
	PauseRetryAfter     = "retry_after"
	PauseRateLimitReset = "ratelimit_reset"
)

// Classes of Observer.Response.
const (
	ClassOK              = "2xx"
	ClassNotModified     = "304"
	ClassRedirect        = "3xx"
	ClassClientError     = "4xx"
	ClassTooManyRequests = "429"
	ClassServerError     = "5xx"
	ClassError           = "error"
)

// responseClass classifies the outcome of one attempt for Observer.Response.
// A status outside 200-599 (an informational status a transport should not
// return as final) counts as ClassError.
func responseClass(resp *http.Response, err error) string {
	if err != nil || resp == nil {
		return ClassError
	}
	switch code := resp.StatusCode; {
	case code == http.StatusNotModified:
		return ClassNotModified
	case code == http.StatusTooManyRequests:
		return ClassTooManyRequests
	case code >= 200 && code < 300:
		return ClassOK
	case code >= 300 && code < 400:
		return ClassRedirect
	case code >= 400 && code < 500:
		return ClassClientError
	case code >= 500 && code < 600:
		return ClassServerError
	}
	return ClassError
}

// Client is the shared HTTP transport: an http.RoundTripper that applies
// the SSRF guard, the per-host rate limit, retries, the User-Agent, the
// per-attempt timeout and the body cap to every hop, redirects included.
// Use HTTPClient to get an *http.Client built on it. It is safe for
// concurrent use.
type Client struct {
	cfg       Config
	transport http.RoundTripper
	guard     *Guard
	limiter   *limiter
	table     []EffectiveLimit
	clock     Clock
	log       *slog.Logger
	jitter    func(time.Duration) time.Duration
}

var _ http.RoundTripper = (*Client)(nil)

// New validates cfg, fills in defaults and returns a Client.
func New(cfg Config) (*Client, error) {
	if cfg.MaxAttempts < 0 || cfg.MaxBodyBytes < 0 || cfg.Timeout < 0 ||
		cfg.BaseBackoff < 0 || cfg.MaxBackoff < 0 || cfg.MaxRetryWait < 0 {
		return nil, errors.New("netguard: negative value in config")
	}
	setDefault(&cfg.UserAgent, "angelia/"+buildinfo.Version()+" (+https://github.com/angelia-dev/angelia)")
	setDefault(&cfg.Timeout, DefaultTimeout)
	setDefault(&cfg.MaxBodyBytes, DefaultMaxBodyBytes)
	setDefault(&cfg.MaxAttempts, DefaultMaxAttempts)
	setDefault(&cfg.BaseBackoff, DefaultBaseBackoff)
	setDefault(&cfg.MaxBackoff, DefaultMaxBackoff)
	setDefault(&cfg.MaxRetryWait, DefaultMaxRetryWait)
	table, limits, fallback, err := resolveLimits(cfg.HostLimits, cfg.FallbackLimit)
	if err != nil {
		return nil, err
	}
	cfg.FallbackLimit = fallback
	if cfg.Resolver == nil {
		cfg.Resolver = net.DefaultResolver
	}
	guard := NewGuard(cfg.Allow, cfg.Resolver)
	if cfg.Transport == nil {
		cfg.Transport = newTransport(guard, cfg.Proxy, cfg.RootCAs)
	}
	if cfg.Clock == nil {
		cfg.Clock = realClock{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}

	cfg.ExtraAuthHeaders = slices.Clone(cfg.ExtraAuthHeaders)

	return &Client{
		cfg:       cfg,
		table:     table,
		transport: cfg.Transport,
		guard:     guard,
		limiter:   newLimiter(cfg.Clock, cfg.FallbackLimit, limits),
		clock:     cfg.Clock,
		log:       cfg.Logger,
		jitter:    fullJitter,
	}, nil
}

// newTransport returns the default transport, dialling through netguard's
// guarded dialer rather than sharing http.DefaultTransport.
func newTransport(g *Guard, proxy func(*http.Request) (*url.URL, error), roots *x509.CertPool) *http.Transport {
	if proxy == nil {
		proxy = http.ProxyFromEnvironment
	}
	d := &dialer{
		base:    net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second},
		guard:   g,
		proxies: proxyAddrs(proxy),
	}
	var tlsCfg *tls.Config
	if roots != nil {
		tlsCfg = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	return &http.Transport{
		TLSClientConfig:       tlsCfg,
		Proxy:                 proxy,
		DialContext:           d.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

func setDefault[T comparable](p *T, v T) {
	var zero T
	if *p == zero {
		*p = v
	}
}

// HTTPClient returns an *http.Client that sends every hop through c. It
// follows at most 10 redirects; each hop is checked by the guard in
// RoundTrip. Its Timeout is unset because c bounds each attempt.
func (c *Client) HTTPClient() *http.Client {
	return &http.Client{
		Transport: c,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("netguard: stopped after %d redirects", maxRedirects)
			}
			return nil
		},
	}
}

// CheckURL applies the URL-level SSRF check of every hop (scheme http or
// https, a host, every address it resolves to allowed by the guard and
// http.allow_networks) to u without sending anything. The core uses it on
// the origins a sink derives from a secret value before the sink sends
// (ADR 0026 §2). The dial-time check still applies when the request is
// sent. Errors are those of RoundTrip's check: a *BlockedError,
// ErrInvalidURL or a lookup failure.
func (c *Client) CheckURL(ctx context.Context, u *url.URL) error {
	return c.guard.CheckURL(ctx, u)
}

// RoundTrip sends one hop after the SSRF check and the hop host's rate
// limit. Requests without a body and with an idempotent method are retried
// on 429, 5xx and timeouts. The response body is capped at the serving
// host's HostLimit.MaxBodyBytes, else its built-in cap, else Config.MaxBodyBytes:
// reading past the cap returns an error wrapping ErrBodyTooLarge. As with
// any RoundTripper, a non-2xx status is not an error.
func (c *Client) RoundTrip(req *http.Request) (*http.Response, error) {
	// The span has the method and the host only if it is in the limits table
	// (else "other", as for metrics): never the URL, which can carry a token
	// in its path or query, nor any header, nor the host of a sink (story #118).
	ctx, span := telemetry.Start(req.Context(), "angelia.http",
		attribute.String("http.request.method", req.Method), attribute.String("server.address", c.limiter.observedHost(req.URL.Hostname())))
	defer span.End()
	resp, err := c.roundTrip(req.WithContext(ctx))
	if resp != nil {
		span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	}
	if errors.Is(err, ErrBodyTooLarge) {
		telemetry.RecordErrorAs(span, err, "body_too_large", "request failed")
	} else {
		telemetry.RecordError(span, err, "request failed")
	}
	return resp, err
}

func (c *Client) roundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	r := req.Clone(ctx)
	if r.Header.Get("User-Agent") == "" {
		r.Header.Set("User-Agent", c.cfg.UserAgent)
	}
	if err := c.guard.CheckURL(ctx, r.URL); err != nil {
		closeRequestBody(req)
		return nil, err
	}
	host := normHost(r.URL.Hostname())
	authed := c.isAuthenticated(r.Header)
	canRetry := isIdempotent(r.Method) && (r.Body == nil || r.Body == http.NoBody)

	for attempt := 1; ; attempt++ {
		waitStart := c.clock.Now()
		if err := c.limiter.wait(ctx, host, authed); err != nil {
			if c.cfg.Observer != nil && errors.Is(err, errSlotPastDeadline) {
				c.cfg.Observer.DeadlineRefusal(c.limiter.observedHost(host))
			}
			closeRequestBody(req)
			return nil, err
		}
		if c.cfg.Observer != nil {
			c.cfg.Observer.RateLimitWait(c.limiter.observedHost(host), max(0, c.clock.Now().Sub(waitStart)))
		}
		attemptCtx, cancel := context.WithTimeout(ctx, c.limiter.timeout(host, c.cfg.Timeout))
		resp, err := c.transport.RoundTrip(r.WithContext(attemptCtx))
		if c.cfg.Observer != nil {
			c.cfg.Observer.Response(c.limiter.observedHost(host), responseClass(resp, err))
		}
		retry, serverWait := c.inspect(ctx, attemptCtx, host, authed, resp, err)
		wait := serverWait
		if wait <= 0 {
			wait = c.backoff(attempt)
		}
		final := !canRetry || !retry || attempt >= c.cfg.MaxAttempts || serverWait > c.cfg.MaxRetryWait
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < wait {
			final = true // sleeping would only end in a deadline error
		}
		if final {
			if err != nil {
				cancel()
				return nil, err
			}
			maxBody := c.limiter.maxBodyBytes(host, c.cfg.MaxBodyBytes)
			resp.Body = &cappedBody{rc: resp.Body, remaining: maxBody, limit: maxBody, cancel: cancel}
			return resp, nil
		}
		attrs := []any{"host", host, "attempt", attempt, "wait", wait}
		if resp != nil {
			drainAndClose(resp.Body)
			attrs = append(attrs, "status", resp.StatusCode)
		} else {
			attrs = append(attrs, "error", err)
		}
		cancel()
		c.log.DebugContext(ctx, "netguard: retrying request", attrs...)
		if err := c.clock.Sleep(ctx, wait); err != nil {
			return nil, fmt.Errorf("netguard: retry wait for %s: %w", host, err)
		}
	}
}

// closeRequestBody honours the RoundTripper contract of closing the body
// even when no request is sent.
func closeRequestBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}

// isAuthenticated reports whether h carries a credential header.
func (c *Client) isAuthenticated(h http.Header) bool {
	for k, v := range h {
		if len(v) == 0 || v[0] == "" {
			continue
		}
		if c.IsCredentialHeader(k) {
			return true
		}
	}
	return false
}

// IsCredentialHeader reports whether this client counts the header name as
// a credential, which puts a request on a host's Authenticated budget: the
// package-level IsCredentialHeader set plus Config.ExtraAuthHeaders. The
// match is case-insensitive. The WASM plugin host derives the headers it
// reserves for host-injected credentials from it, so the two cannot drift.
func (c *Client) IsCredentialHeader(name string) bool {
	if IsCredentialHeader(name) {
		return true
	}
	for _, n := range c.cfg.ExtraAuthHeaders {
		if strings.EqualFold(name, n) {
			return true
		}
	}
	return false
}

// inspect applies rate limit headers to the limiter and decides whether the
// outcome is worth a retry, and after how long the server asked us to wait.
func (c *Client) inspect(ctx, attemptCtx context.Context, host string, authed bool, resp *http.Response, err error) (retry bool, serverWait time.Duration) {
	if err != nil {
		if ctx.Err() != nil {
			return false, 0
		}
		if errors.Is(attemptCtx.Err(), context.DeadlineExceeded) {
			return true, 0
		}
		var ne net.Error
		return errors.As(err, &ne) && ne.Timeout(), 0
	}
	now := c.clock.Now()
	retryAt, reason := retryAtReason(resp.Header, now)
	if !retryAt.IsZero() {
		c.limiter.pause(host, authed, retryAt)
		if c.cfg.Observer != nil {
			c.cfg.Observer.ServerPause(c.limiter.observedHost(host), reason)
		}
		serverWait = retryAt.Sub(now)
	}
	switch code := resp.StatusCode; {
	case code == http.StatusTooManyRequests:
		return true, serverWait
	case code == http.StatusForbidden:
		// GitHub signals its rate limits with 403 plus rate limit headers.
		return !retryAt.IsZero(), serverWait
	case code >= 500 && code != http.StatusNotImplemented && code != http.StatusHTTPVersionNotSupported:
		return true, serverWait
	}
	return false, 0
}

// retryAt returns when the server allows the next request, from
// Retry-After or from a rate limit header pair: X-RateLimit-Remaining: 0
// plus X-RateLimit-Reset (GitHub), or the unprefixed RateLimit-Remaining and
// RateLimit-Reset (GitLab). Both Reset headers are read as a Unix timestamp
// in seconds, which is what GitHub and GitLab send. A Reset in delta seconds
// (the IETF draft meaning) reads as a time in the past and is ignored: only
// Retry-After or the backoff applies then. Remaining is read as an integer
// (any suffix such as Docker's "0;w=60" is dropped) and 0 or less counts as
// exhausted.
// The X- pair takes precedence if both are present. It returns the zero time
// if the server set no limit.
func retryAt(h http.Header, now time.Time) time.Time {
	t, _ := retryAtReason(h, now)
	return t
}

// retryAtReason is retryAt plus the header that set the time:
// PauseRetryAfter or PauseRateLimitReset ("" with the zero time).
func retryAtReason(h http.Header, now time.Time) (time.Time, string) {
	if d := ParseRetryAfter(h.Get("Retry-After"), now); d > 0 {
		return now.Add(d), PauseRetryAfter
	}
	for _, p := range [][2]string{{"X-RateLimit-Remaining", "X-RateLimit-Reset"}, {"RateLimit-Remaining", "RateLimit-Reset"}} {
		if n, ok := leadingInt(h.Get(p[0])); !ok || n > 0 {
			continue
		}
		if reset, err := strconv.ParseInt(h.Get(p[1]), 10, 64); err == nil {
			if t := time.Unix(reset, 0); t.After(now) {
				return t, PauseRateLimitReset
			}
		}
	}
	return time.Time{}, ""
}

// leadingInt parses the integer at the start of v, ignoring parameters after
// a ';' or space.
func leadingInt(v string) (int64, bool) {
	v, _, _ = strings.Cut(strings.TrimSpace(v), ";")
	v, _, _ = strings.Cut(v, " ")
	n, err := strconv.ParseInt(v, 10, 64)
	return n, err == nil
}

// ParseRetryAfter reads a Retry-After value in seconds or as an HTTP date,
// relative to now, capped at one hour. It returns 0 for a missing, invalid
// or past value. Sinks use it to classify a response (sink.StatusError).
func ParseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	var d time.Duration
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		if secs > int64(defaultMaxPause/time.Second) {
			return defaultMaxPause
		}
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(v); err == nil && t.After(now) {
		d = t.Sub(now)
	}
	return min(d, defaultMaxPause)
}

// backoff returns the jittered delay before retry number attempt (from 1).
func (c *Client) backoff(attempt int) time.Duration {
	ceiling := c.cfg.MaxBackoff
	if attempt < 31 {
		if d := c.cfg.BaseBackoff << (attempt - 1); d > 0 && d < ceiling {
			ceiling = d
		}
	}
	return c.jitter(ceiling)
}

// fullJitter returns a uniformly random duration in [0, d].
func fullJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d) + 1)) //nolint:gosec // G404: jitter needs no cryptographic randomness.
}

func isIdempotent(method string) bool {
	switch method {
	case "", http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

func drainAndClose(rc io.ReadCloser) {
	_, _ = io.CopyN(io.Discard, rc, 64<<10)
	_ = rc.Close()
}

// cappedBody returns at most limit bytes, then an error wrapping
// ErrBodyTooLarge if the underlying body has more. It releases the
// attempt's timeout context on Close or at the end of the body.
type cappedBody struct {
	rc        io.ReadCloser
	remaining int64
	limit     int64
	cancel    context.CancelFunc
	once      sync.Once
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		var probe [1]byte
		n, err := b.rc.Read(probe[:])
		if n > 0 {
			return 0, fmt.Errorf("netguard: %w (%d bytes)", ErrBodyTooLarge, b.limit)
		}
		if err != nil {
			b.release()
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.rc.Read(p)
	b.remaining -= int64(n)
	if err != nil {
		b.release()
	}
	return n, err
}

func (b *cappedBody) Close() error {
	err := b.rc.Close()
	b.release()
	return err
}

func (b *cappedBody) release() { b.once.Do(b.cancel) }

// Result is the outcome of a successful Fetch.
type Result struct {
	StatusCode int
	Header     http.Header
	// Body is the full response body; nil when NotModified.
	Body []byte
	// NotModified is true for a 304. The body is not read.
	NotModified bool
	// Validators to store for the next Fetch: the response's, or the
	// previous ones updated by any sent with a 304.
	Validators Validators
}

// StatusError is returned by Fetch for a status other than 2xx or 304.
type StatusError struct {
	StatusCode int
	Status     string
	// RetryAt is when the server allows the next request (Retry-After or a
	// rate limit reset); zero if it did not say.
	RetryAt time.Time
}

func (e *StatusError) Error() string {
	if e.RetryAt.IsZero() {
		return "netguard: unexpected status " + e.Status
	}
	return fmt.Sprintf("netguard: unexpected status %s, retry at %s", e.Status, e.RetryAt.UTC().Format(time.RFC3339))
}

// Fetch performs a conditional GET-style request with hc, normally the
// HTTPClient of a netguard Client injected into the caller: prev's
// validators are sent as If-None-Match and If-Modified-Since. A 304 is
// reported as NotModified without reading the body; a 2xx body is read in
// full (up to the cap); any other status is a *StatusError, whose RetryAt
// is computed against the response's Date header when present.
func Fetch(ctx context.Context, hc *http.Client, req *http.Request, prev Validators) (*Result, error) {
	req = req.Clone(ctx)
	prev.apply(req.Header)
	resp, err := hc.Do(req) //nolint:gosec // G704: hc is a netguard HTTPClient, whose RoundTrip checks every hop.
	if err != nil {
		return nil, err
	}

	switch {
	case resp.StatusCode == http.StatusNotModified:
		_ = resp.Body.Close()
		return &Result{
			StatusCode:  resp.StatusCode,
			Header:      resp.Header,
			NotModified: true,
			Validators:  prev.merge(ValidatorsFrom(resp.Header)),
		}, nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("read body from %s: %w", req.URL.Host, err)
		}
		return &Result{
			StatusCode: resp.StatusCode,
			Header:     resp.Header,
			Body:       body,
			Validators: ValidatorsFrom(resp.Header),
		}, nil
	default:
		drainAndClose(resp.Body)
		now := time.Now()
		if t, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
			now = t
		}
		return nil, &StatusError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			RetryAt:    retryAt(resp.Header, now),
		}
	}
}
