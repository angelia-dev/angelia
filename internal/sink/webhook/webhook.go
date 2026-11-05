// Package webhook is the "webhook" sink (ADR 0023): it POSTs a delivery's
// message to a URL, signed per Standard Webhooks v1, through the netguard
// client.
//
// The body is the rendered message byte for byte: by default the stored
// CloudEvents JSON in structured mode. Headers: webhook-id (the delivery's
// idempotency key, the same on every attempt and replay), webhook-timestamp
// (Unix seconds of this attempt) and, when the sink has a secret,
// webhook-signature with one "v1,<base64 HMAC-SHA256>" entry per secret.
//
// The sink never reads the environment: the core resolves secret_env into
// sink.Request.Secrets. It logs nothing, and its errors carry no secret,
// signature, resolved address or response body.
package webhook

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/sink"
)

// Kind is the registry key of this sink.
const Kind = "webhook"

// Timeout bounds of the timeout key (spec WebhookSinkConfig).
const (
	DefaultTimeout = 10 * time.Second
	MinTimeout     = time.Second
	MaxTimeout     = 2 * time.Minute
)

// SecretPointer is the key of the signing secret in sink.Request.Secrets:
// the JSON Pointer of secret_env in the configuration.
const SecretPointer = "/secret_env"

// maxResponseRead is how much of a response body is read (and discarded)
// so that the connection can be reused.
const maxResponseRead = 64 << 10

// Config is the configuration of a webhook sink (watch-file.md 2.4). It
// holds the name of the variable with the secret, never the secret.
type Config struct {
	URL       string `json:"url"`
	SecretEnv string `json:"secret_env,omitempty"`
	Timeout   string `json:"timeout,omitempty"`
}

// Options configures a Sink.
type Options struct {
	// Now is the clock read for webhook-timestamp just before the request
	// is handed to the client (before netguard's per-host rate-limit wait,
	// which the delivery deadline bounds). Default time.Now.
	Now func() time.Time
	// AllowHTTPHosts is the instance's http.allow_http_hosts: the hosts a
	// webhook may reach over plain http:// (decision D12). It is checked at
	// every delivery, so removing a host stops cleartext deliveries to it
	// even for sinks stored earlier. Empty: https only.
	AllowHTTPHosts []string
	// Logger receives one warning per unsigned sink (no secret_env), the
	// first time this process delivers for it. Nil discards.
	Logger *slog.Logger
}

// Sink is the webhook sink. It is safe for concurrent use.
type Sink struct {
	client    *http.Client
	now       func() time.Time
	httpHosts []string
	log       *slog.Logger
	// unsigned holds the ids of the unsigned sinks already warned about.
	unsigned sync.Map
}

var _ sink.Sink = (*Sink)(nil)

// New returns a webhook sink sending through client, which must be the
// netguard client (netguard.Client.HTTPClient). The sink uses a copy of it
// that does not follow redirects. New panics if client is nil.
func New(client *http.Client, opts Options) *Sink {
	if client == nil {
		panic("webhook.New: nil client")
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Sink{client: &c, now: opts.Now, httpHosts: slices.Clone(opts.AllowHTTPHosts), log: opts.Logger}
}

// Kind implements sink.Sink.
func (*Sink) Kind() string { return Kind }

// Validate implements sink.Sink. It checks the shape of the configuration;
// the watch file and the API apply their stricter rules before storing it.
func (*Sink) Validate(cfg sink.Config) error {
	_, _, err := parseConfig(cfg)
	return err
}

type parsed struct {
	Config
	url     *url.URL
	timeout time.Duration
}

func parseConfig(raw sink.Config) (parsed, string, error) {
	var p parsed
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p.Config); err != nil {
		return p, "", errors.New("webhook: invalid configuration: want an object with url, secret_env and timeout")
	}
	u, err := url.Parse(p.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
		return p, "", errors.New("webhook: invalid configuration: url must be an absolute http(s) URL without credentials")
	}
	if netguard.NonCanonicalIP(u.Hostname()) {
		return p, "", errors.New("webhook: invalid configuration: url host is an IP address in a non-canonical form; write it as a.b.c.d")
	}
	p.url = u
	origin := u.Scheme + "://" + u.Host
	if p.SecretEnv != "" && (!config.IsEnvName(p.SecretEnv) || config.LooksLikeToken(p.SecretEnv)) {
		return p, origin, errors.New("webhook: invalid configuration: secret_env must be an environment variable name")
	}
	p.timeout = DefaultTimeout
	if p.Timeout != "" {
		d, err := time.ParseDuration(p.Timeout)
		if err != nil || d < MinTimeout || d > MaxTimeout {
			return p, origin, fmt.Errorf("webhook: invalid configuration: timeout must be a duration from %v to %v", MinTimeout, MaxTimeout)
		}
		p.timeout = d
	}
	return p, origin, nil
}

// Deliver implements sink.Sink: one signed POST. The request is bounded by
// min(timeout, the deadline of ctx, which is what is left of the
// dispatcher's deliver timeout).
func (s *Sink) Deliver(ctx context.Context, req sink.Request) (sink.Result, error) {
	cfg, origin, err := parseConfig(req.Config)
	if err != nil {
		return sink.Result{}, sink.Permanent(err)
	}
	id := req.IdempotencyKey
	if id == "" || strings.ContainsAny(id, ". \t\r\n\v\f") {
		return sink.Result{}, sink.Permanent(errors.New("webhook: invalid webhook-id: empty, or holds '.' or white space"))
	}
	if cfg.url.Scheme == "http" && !slices.ContainsFunc(s.httpHosts, func(h string) bool { return strings.EqualFold(h, cfg.url.Hostname()) }) {
		return sink.Result{}, sink.Permanent(fmt.Errorf("webhook: %s: http:// is not allowed for this host (http.allow_http_hosts); use https://", origin))
	}
	var keys [][]byte
	if cfg.SecretEnv == "" {
		s.warnUnsigned(req.SinkID, origin)
	} else {
		v, ok := req.Secrets[SecretPointer]
		if !ok {
			return sink.Result{}, fmt.Errorf("webhook: secret_env %s: not resolved", cfg.SecretEnv)
		}
		if keys, err = ParseSecrets(string(v)); err != nil {
			// Retried: the operator fixes it by setting the value.
			return sink.Result{}, fmt.Errorf("webhook: secret_env %s: %w", cfg.SecretEnv, err)
		}
	}
	contentType := req.Rendered.ContentType
	if contentType == "" {
		contentType = sink.CloudEventsContentType
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.url.String(), bytes.NewReader(req.Rendered.Body))
	if err != nil {
		return sink.Result{}, sink.Permanent(fmt.Errorf("webhook: %s: invalid request", origin))
	}
	ts := strconv.FormatInt(s.now().Unix(), 10)
	hreq.Header.Set("Content-Type", contentType)
	hreq.Header.Set(HeaderID, id)
	hreq.Header.Set(HeaderTimestamp, ts)
	if len(keys) > 0 {
		hreq.Header.Set(HeaderSignature, sign(keys, id, ts, req.Rendered.Body))
	}

	resp, err := s.client.Do(hreq) //nolint:gosec // G704: the client is netguard's, whose RoundTrip checks every hop.
	if err != nil {
		return sink.Result{}, requestError(origin, cfg.url.Hostname(), err)
	}
	_, _ = io.CopyN(io.Discard, resp.Body, maxResponseRead)
	_ = resp.Body.Close()
	res := sink.Result{StatusCode: resp.StatusCode}
	if err := sink.HTTPStatus(resp.StatusCode, netguard.ParseRetryAfter(resp.Header.Get("Retry-After"), s.now())); err != nil {
		return res, &statusError{origin: origin, err: err}
	}
	return res, nil
}

// warnUnsigned logs once per sink id that the sink delivers unsigned.
func (s *Sink) warnUnsigned(sinkID, origin string) {
	if _, seen := s.unsigned.LoadOrStore(sinkID, struct{}{}); seen {
		return
	}
	s.log.Warn("webhook sink has no secret_env: deliveries are unsigned and receivers cannot verify them",
		"sink", sinkID, "destination", origin)
}

// requestError classifies a failed request. An SSRF refusal is permanent and
// names the URL host only, never the resolved address; so is a URL no retry
// can fix (netguard.ErrInvalidURL). Anything else is retried, with a fixed
// description (connection refused or reset, timeout, DNS, TLS) that never
// holds the dialled address or port. The cause stays in the chain.
func requestError(origin, host string, err error) error {
	var be *netguard.BlockedError
	if errors.As(err, &be) || errors.Is(err, netguard.ErrBlocked) {
		return sink.Permanent(blockedError{host: host})
	}
	if errors.Is(err, netguard.ErrInvalidURL) {
		return sink.Permanent(&requestFailure{origin: origin, class: "invalid URL (scheme or host not allowed)", err: err})
	}
	return &requestFailure{origin: origin, class: failureClass(err), err: err}
}

// failureClass describes a transport error without its addresses.
func failureClass(err error) string {
	var (
		dnsErr  *net.DNSError
		ne      net.Error
		certErr *tls.CertificateVerificationError
		recErr  tls.RecordHeaderError
		alert   tls.AlertError
		unknown x509.UnknownAuthorityError
		badHost x509.HostnameError
		invalid x509.CertificateInvalidError
	)
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &dnsErr):
		return "DNS lookup failed"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection reset"
	case errors.As(err, &certErr), errors.As(err, &recErr), errors.As(err, &alert),
		errors.As(err, &unknown), errors.As(err, &badHost), errors.As(err, &invalid):
		return "TLS handshake failed"
	case strings.Contains(err.Error(), "netguard: resolve "):
		return "DNS lookup failed"
	default:
		return "request failed"
	}
}

// requestFailure is a request that got no response: "webhook:
// <scheme://host>: <class>". Unwrap gives the cause, for errors.Is and
// spans; Error never repeats it.
type requestFailure struct {
	origin, class string
	err           error
}

func (e *requestFailure) Error() string { return "webhook: " + e.origin + ": " + e.class }
func (e *requestFailure) Unwrap() error { return e.err }

// blockedError is a destination the SSRF guard refused.
type blockedError struct{ host string }

func (e blockedError) Error() string { return "webhook: destination not allowed (" + e.host + ")" }
func (blockedError) Unwrap() error   { return netguard.ErrBlocked }

// statusError is a response that is not a 2xx: "webhook: <scheme://host>:
// HTTP <code> <text>". It wraps sink.HTTPStatus's error, which carries
// the class (permanent or not) and Retry-After.
type statusError struct {
	origin string
	err    error
}

func (e *statusError) Error() string {
	return "webhook: " + e.origin + ": " + strings.TrimPrefix(e.err.Error(), "sink: ")
}
func (e *statusError) Unwrap() error { return e.err }
