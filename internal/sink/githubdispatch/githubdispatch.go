// Package githubdispatch is the "github-dispatch" sink (spec
// GithubDispatchSinkConfig, docs/github-dispatch.md): it triggers a
// repository_dispatch event or a workflow_dispatch run in one GitHub
// repository, through the netguard client.
//
// repository_dispatch sends event_type (a template) and a flat
// client_payload of at most 10 top-level keys, the GitHub limit; every
// string in it is cut to 2048 bytes at a UTF-8 boundary and truncated is
// true when anything was cut. workflow_dispatch sends ref and inputs, each
// input a template cut to 1024 bytes. The subscription's template does not
// apply: the payload is fixed.
//
// The sink never reads the environment or the store: the core resolves
// token_env and token_ref into sink.Request.Secrets. The token is sent only
// to api_base_url, the host the core resolved a stored token for, and the
// sink follows no redirect. It logs nothing, and its errors carry no token,
// URL path or response body.
package githubdispatch

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/render"
	"github.com/angelia-dev/angelia/internal/sink"
)

const (
	// MaxPayloadStringBytes is the longest string of client_payload.
	MaxPayloadStringBytes = 2048
	// MaxInputBytes is the longest workflow input value.
	MaxInputBytes = 1024
	// MaxBodyBytes is the size the sink keeps the JSON body under, below
	// GitHub's 65535 characters for client_payload. The strings are cut
	// further, deterministically, when escaping would pass it.
	MaxBodyBytes = 60000

	apiVersion = "2022-11-28"
	// maxResponseRead is how much of a response body is read (and
	// discarded) so that the connection can be reused.
	maxResponseRead = 64 << 10
	// maxRateLimitWait caps the wait read from x-ratelimit-reset, as
	// netguard.ParseRetryAfter caps Retry-After.
	maxRateLimitWait = time.Hour
)

// Options configures a Sink.
type Options struct {
	// Now is the clock for Retry-After and x-ratelimit-reset. Default
	// time.Now.
	Now func() time.Time
}

// Sink is the github-dispatch sink. It is safe for concurrent use.
type Sink struct {
	client *http.Client
	now    func() time.Time
}

var _ sink.Sink = (*Sink)(nil)

// New returns a github-dispatch sink sending through client, which must be
// the netguard client (netguard.Client.HTTPClient). The sink uses a copy of
// it that does not follow redirects. New panics if client is nil.
func New(client *http.Client, opts Options) *Sink {
	if client == nil {
		panic("githubdispatch.New: nil client")
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Sink{client: &c, now: opts.Now}
}

// Kind implements sink.Sink.
func (*Sink) Kind() string { return Kind }

// Validate implements sink.Sink. It checks the configuration, including that
// the templates compile and render on the sample events; it does no I/O.
func (*Sink) Validate(cfg sink.Config) error {
	_, err := parseConfig(cfg, true)
	return err
}

// Deliver implements sink.Sink: one POST. The request is bounded by
// min(timeout, the deadline of ctx).
func (s *Sink) Deliver(ctx context.Context, req sink.Request) (sink.Result, error) {
	cfg, err := parseConfig(req.Config, false)
	if err != nil {
		return sink.Result{}, sink.Permanent(err)
	}
	origin := cfg.base.Scheme + "://" + cfg.base.Host
	envelope := req.Event.JSON
	if len(envelope) == 0 {
		if envelope, err = json.Marshal(req.Event.Envelope); err != nil {
			return sink.Result{}, sink.Permanent(errors.New("github-dispatch: the event cannot be encoded"))
		}
	}
	var body []byte
	var path string
	if cfg.Mode == ModeWorkflowDispatch {
		path = "/actions/workflows/" + url.PathEscape(cfg.Workflow) + "/dispatches"
		body, err = workflowBody(ctx, cfg, envelope)
	} else {
		path = "/dispatches"
		body, err = repositoryBody(ctx, cfg, req.Event.Envelope, envelope)
	}
	if err != nil {
		if ctx.Err() != nil {
			// The attempt ran out of time or was canceled, not the template.
			return sink.Result{}, err
		}
		return sink.Result{}, sink.Permanent(err)
	}

	ref, ptr := cfg.tokenRef()
	v, ok := req.Secrets[ptr]
	if !ok {
		return sink.Result{}, fmt.Errorf("github-dispatch: %s: not resolved", ref)
	}
	token := strings.TrimSpace(string(v.Reveal()))
	if !validToken(token) {
		// Retried: the operator fixes it by setting the value.
		return sink.Result{}, fmt.Errorf("github-dispatch: %s: invalid token (empty, or holds white space or control characters)", ref)
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()
	target := strings.TrimRight(cfg.APIBaseURL, "/") + "/repos/" + cfg.Repo + path
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return sink.Result{}, sink.Permanent(fmt.Errorf("github-dispatch: %s: invalid request", origin))
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/vnd.github+json")
	hreq.Header.Set("X-GitHub-Api-Version", apiVersion)
	hreq.Header.Set("Authorization", "Bearer "+token)

	resp, err := s.client.Do(hreq) //nolint:gosec // G704: the client is netguard's, whose RoundTrip checks every hop.
	if err != nil {
		return sink.Result{}, requestError(origin, cfg.base.Hostname(), err)
	}
	_, _ = io.CopyN(io.Discard, resp.Body, maxResponseRead)
	_ = resp.Body.Close()
	res := sink.Result{StatusCode: resp.StatusCode}
	if err := s.classify(resp); err != nil {
		return res, &statusError{origin: origin, code: resp.StatusCode, err: err}
	}
	return res, nil
}

// tokenRef names the token's reference for errors, "token_env NAME" or
// "token_ref NAME", and gives its key in sink.Request.Secrets.
func (p parsed) tokenRef() (ref, ptr string) {
	if p.TokenEnv != "" {
		return "token_env " + p.TokenEnv, TokenPointer
	}
	return "token_ref " + p.TokenRef, TokenRefPointer
}

// validToken reports whether t can be an HTTP header value: printable ASCII
// with no white space.
func validToken(t string) bool {
	if t == "" {
		return false
	}
	for i := range len(t) {
		if t[i] <= 0x20 || t[i] >= 0x7f {
			return false
		}
	}
	return true
}

// classify maps a response to nil (success), a permanent error or a
// retried one. Spec decision 38.4: 404 and 422 are permanent, 5xx retried,
// a 403 or 429 caused by a rate limit is retried after Retry-After or
// x-ratelimit-reset, any other 4xx is permanent; a redirect is permanent.
func (s *Sink) classify(resp *http.Response) error {
	now := s.now()
	wait := netguard.ParseRetryAfter(resp.Header.Get("Retry-After"), now)
	exhausted := false
	if n, err := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining")), 10, 64); err == nil && n <= 0 {
		exhausted = true
		if reset, err := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("X-RateLimit-Reset")), 10, 64); err == nil {
			wait = max(wait, min(time.Unix(reset, 0).Sub(now), maxRateLimitWait))
		}
	}
	if resp.StatusCode == http.StatusForbidden && (wait > 0 || exhausted) {
		// A rate limit, not a refusal: retried.
		return &sink.StatusError{Code: resp.StatusCode, RetryAfter: max(wait, 0)}
	}
	return sink.HTTPStatus(resp.StatusCode, wait)
}

// clientPayload is the client_payload of repository_dispatch: a fixed flat
// subset of the event, 9 keys and truncated, GitHub's 10.
type clientPayload struct {
	EventID         string  `json:"event_id"`
	Source          string  `json:"source"`
	Package         string  `json:"package"`
	Version         string  `json:"version"`
	PreviousVersion *string `json:"previous_version"`
	Bump            *string `json:"bump"`
	PURL            *string `json:"purl"`
	ReleaseURL      *string `json:"release_url"`
	Watch           string  `json:"watch"`
	Truncated       bool    `json:"truncated,omitempty"`
}

// cut returns s cut to at most n bytes at a UTF-8 boundary, and whether it
// was cut.
func cut(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

// payloadFrom builds client_payload with every string cut to n bytes.
func payloadFrom(env eventView, n int) clientPayload {
	var truncated bool
	str := func(s string) string {
		out, c := cut(s, n)
		truncated = truncated || c
		return out
	}
	opt := func(s *string) *string {
		if s == nil {
			return nil
		}
		out := str(*s)
		return &out
	}
	p := clientPayload{
		EventID: str(env.id), Source: str(env.source), Package: str(env.pkg), Version: str(env.version),
		PreviousVersion: opt(env.previous), Bump: opt(env.bump), PURL: opt(env.purl), ReleaseURL: opt(env.releaseURL),
		Watch: str(env.watch),
	}
	p.Truncated = truncated
	return p
}

// encode marshals v without HTML escaping, which would triple the size of
// every & < > in a URL.
func encode(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// fit encodes with build(n) for n = limit, then half of it while the body
// passes MaxBodyBytes (escaping can multiply a control character by six).
func fit(limit int, build func(n int) any) ([]byte, error) {
	for n := limit; ; n /= 2 {
		b, err := encode(build(n))
		if err != nil {
			return nil, fmt.Errorf("github-dispatch: the payload cannot be encoded")
		}
		if len(b) <= MaxBodyBytes || n <= 1 {
			return b, nil
		}
	}
}

// renderTemplate renders one event_type or input template on the event.
func renderTemplate(ctx context.Context, what, src string, envelope []byte) (string, error) {
	if src == "" {
		return "", nil // render.Compile refuses an empty template
	}
	t, err := render.Compile(src, render.Limits{})
	if err != nil {
		return "", fmt.Errorf("github-dispatch: %s template: %w", what, err)
	}
	r, err := t.Render(ctx, envelope)
	if err != nil {
		return "", fmt.Errorf("github-dispatch: %s template: %w", what, err)
	}
	return string(r.Body), nil
}

func repositoryBody(ctx context.Context, cfg parsed, env event.Envelope, envelope []byte) ([]byte, error) {
	et, err := renderTemplate(ctx, "event_type", cfg.EventType, envelope)
	if err != nil {
		return nil, err
	}
	if err := CheckEventType(et); err != nil {
		return nil, fmt.Errorf("github-dispatch: event_type template: the result %w", err)
	}
	view := viewOf(env)
	return fit(MaxPayloadStringBytes, func(n int) any {
		return struct {
			EventType     string        `json:"event_type"`
			ClientPayload clientPayload `json:"client_payload"`
		}{et, payloadFrom(view, n)}
	})
}

func workflowBody(ctx context.Context, cfg parsed, envelope []byte) ([]byte, error) {
	vals := make(map[string]string, len(cfg.Inputs))
	for k, src := range cfg.Inputs {
		v, err := renderTemplate(ctx, "input", src, envelope)
		if err != nil {
			return nil, err
		}
		vals[k] = v
	}
	return fit(MaxInputBytes, func(n int) any {
		inputs := make(map[string]string, len(vals))
		for k, v := range vals {
			inputs[k], _ = cut(v, n)
		}
		return struct {
			Ref    string            `json:"ref"`
			Inputs map[string]string `json:"inputs"`
		}{cfg.Ref, inputs}
	})
}

// requestError classifies a failed request. An SSRF refusal is permanent and
// names the URL host only, never the resolved address; so is a URL no retry
// can fix. Anything else is retried, with a fixed description that never
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

// requestFailure is a request that got no response: "github-dispatch:
// <scheme://host>: <class>". Unwrap gives the cause; Error never repeats it.
type requestFailure struct {
	origin, class string
	err           error
}

func (e *requestFailure) Error() string { return "github-dispatch: " + e.origin + ": " + e.class }
func (e *requestFailure) Unwrap() error { return e.err }

// blockedError is a destination the SSRF guard refused.
type blockedError struct{ host string }

func (e blockedError) Error() string {
	return "github-dispatch: destination not allowed (" + e.host + ")"
}
func (blockedError) Unwrap() error { return netguard.ErrBlocked }

// statusError is a response that is not a 2xx: "github-dispatch:
// <scheme://host>: HTTP <code> <text> (<hint>)". It wraps the error that
// carries the class (permanent or not) and Retry-After.
type statusError struct {
	origin string
	code   int
	err    error
}

func (e *statusError) Error() string {
	msg := "github-dispatch: " + e.origin + ": " + strings.TrimPrefix(e.err.Error(), "sink: ")
	if h := hint(e.code); h != "" {
		msg += " (" + h + ")"
	}
	return msg
}
func (e *statusError) Unwrap() error { return e.err }

// hint says what GitHub means by a status, for the operator.
func hint(code int) string {
	switch code {
	case http.StatusUnauthorized:
		return "the token is not accepted"
	case http.StatusForbidden:
		return "the token lacks the permission: contents: write for repository_dispatch, actions: write for workflow_dispatch"
	case http.StatusNotFound:
		return "repository, workflow or token without access"
	case http.StatusUnprocessableEntity:
		return "GitHub refused the payload or inputs"
	}
	return ""
}
