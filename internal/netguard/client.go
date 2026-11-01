package netguard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/angelia-dev/angelia/internal/buildinfo"
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
	Timeout time.Duration
	// MaxBodyBytes caps response bodies. Default 10 MiB.
	MaxBodyBytes int64
	// MaxAttempts is the total number of attempts for a retryable request,
	// including the first. Default 4; 1 disables retries.
	MaxAttempts int
	// BaseBackoff and MaxBackoff bound the exponential backoff with full
	// jitter used when the server gives no Retry-After. Defaults 500ms, 30s.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	// MaxRetryWait is the longest server-requested wait (Retry-After or a
	// GitHub rate limit reset) the client sleeps through before retrying.
	// A longer wait, or one past the context deadline, ends the call with
	// the last response; the host stays paused. Default 2m.
	MaxRetryWait time.Duration
	// HostLimits override or extend DefaultHostLimits, keyed by hostname.
	HostLimits map[string]HostLimit
	// FallbackLimit applies to other hosts. Default DefaultFallbackLimit().
	FallbackLimit HostLimit
	// Allow lists prefixes the SSRF guard accepts even though they are
	// loopback, private or link-local. Empty by default.
	Allow []netip.Prefix
	// Resolver is used by the SSRF guard. Default net.DefaultResolver.
	Resolver Resolver
	// Transport sends each attempt. By default it is an *http.Transport
	// dialling through netguard's own net.Dialer, the place where
	// dial-time address checks belong (angelia-dev/angelia#76). A custom
	// Transport, as used in tests, bypasses any such dial-level check; the
	// URL checks still apply.
	Transport http.RoundTripper
	// Clock defaults to the system clock.
	Clock Clock
	// Logger defaults to a logger that discards everything.
	Logger *slog.Logger
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
	setDefault(&cfg.FallbackLimit, DefaultFallbackLimit())
	if cfg.Resolver == nil {
		cfg.Resolver = net.DefaultResolver
	}
	if cfg.Transport == nil {
		cfg.Transport = newTransport()
	}
	if cfg.Clock == nil {
		cfg.Clock = realClock{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}

	if err := cfg.FallbackLimit.validate(); err != nil {
		return nil, fmt.Errorf("netguard: fallback limit: %w", err)
	}
	limits := DefaultHostLimits()
	for h, l := range cfg.HostLimits {
		if err := l.validate(); err != nil {
			return nil, fmt.Errorf("netguard: limit for %s: %w", h, err)
		}
		limits[normHost(h)] = l
	}

	return &Client{
		cfg:       cfg,
		transport: cfg.Transport,
		guard:     NewGuard(cfg.Allow, cfg.Resolver),
		limiter:   newLimiter(cfg.Clock, cfg.FallbackLimit, limits),
		clock:     cfg.Clock,
		log:       cfg.Logger,
		jitter:    fullJitter,
	}, nil
}

// newTransport returns the default transport, dialling through a
// net.Dialer owned by netguard rather than sharing http.DefaultTransport.
func newTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
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

// RoundTrip sends one hop after the SSRF check and the hop host's rate
// limit. Requests without a body and with an idempotent method are retried
// on 429, 5xx and timeouts. The response body is capped at MaxBodyBytes:
// reading past the cap returns an error wrapping ErrBodyTooLarge. As with
// any RoundTripper, a non-2xx status is not an error.
func (c *Client) RoundTrip(req *http.Request) (*http.Response, error) {
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
	authed := r.Header.Get("Authorization") != ""
	canRetry := isIdempotent(r.Method) && (r.Body == nil || r.Body == http.NoBody)

	for attempt := 1; ; attempt++ {
		if err := c.limiter.wait(ctx, host, authed); err != nil {
			closeRequestBody(req)
			return nil, err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
		resp, err := c.transport.RoundTrip(r.WithContext(attemptCtx))
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
			resp.Body = &cappedBody{rc: resp.Body, remaining: c.cfg.MaxBodyBytes, limit: c.cfg.MaxBodyBytes, cancel: cancel}
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
	retryAt := retryAt(resp.Header, now)
	if !retryAt.IsZero() {
		c.limiter.pause(host, authed, retryAt)
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
// Retry-After or from GitHub's X-RateLimit-Remaining: 0 plus
// X-RateLimit-Reset. It returns the zero time if the server set no limit.
func retryAt(h http.Header, now time.Time) time.Time {
	if d := parseRetryAfter(h.Get("Retry-After"), now); d > 0 {
		return now.Add(d)
	}
	if h.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			if t := time.Unix(reset, 0); t.After(now) {
				return t
			}
		}
	}
	return time.Time{}
}

// parseRetryAfter reads a Retry-After value in seconds or as an HTTP date,
// capped at defaultMaxPause. It returns 0 for a missing, invalid or past
// value.
func parseRetryAfter(v string, now time.Time) time.Duration {
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
