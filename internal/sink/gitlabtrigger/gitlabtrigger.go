// Package gitlabtrigger is the "gitlab-pipeline-trigger" sink (spec
// GitlabPipelineTriggerSinkConfig, docs/gitlab-pipeline-trigger.md): it
// starts a GitLab CI/CD pipeline in one project with a pipeline trigger
// token, through the netguard client.
//
// One POST {base_url}/api/v4/projects/{project}/trigger/pipeline carries the
// token, the ref, and the pipeline variables and inputs in a JSON body. With
// neither variables nor inputs configured it sends the ANGELIA_* variables of
// defaultVariables; otherwise only the configured entries, each a template
// cut to 2048 bytes. The subscription's template does not apply.
//
// The sink never reads the environment or the store: the core resolves
// token_env and token_ref into sink.Request.Secrets. The token is sent only
// in the body, to base_url, the host the core resolved a stored token for,
// and the sink follows no redirect. It logs nothing, and its errors carry no
// token, project path or response body.
package gitlabtrigger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/render"
	"github.com/angelia-dev/angelia/internal/sink"
)

const (
	// MaxValueBytes is the longest value of a variable or input.
	MaxValueBytes = 2048

	// maxResponseRead is how much of a response body is read (and
	// discarded) so that the connection can be reused.
	maxResponseRead = 64 << 10
	// maxBodyPeek is how much of a 400 body is searched for the
	// pipeline-variables refusal.
	maxBodyPeek = 4 << 10
	// maxRateLimitWait caps the wait read from RateLimit-Reset, as
	// netguard.ParseRetryAfter caps Retry-After.
	maxRateLimitWait = time.Hour

	// variablesRefusal is the text of GitLab's 400 when the project
	// restricts who may set pipeline variables.
	variablesRefusal = "Insufficient permissions to set pipeline variables"
)

// Options configures a Sink.
type Options struct {
	// Now is the clock for Retry-After and RateLimit-Reset. Default
	// time.Now.
	Now func() time.Time
	// AllowHTTPHosts is the instance's http.allow_http_hosts: the hosts a
	// base_url may reach over plain http:// (decision D72). It is checked at
	// every delivery, so removing a host stops cleartext deliveries to it
	// even for sinks stored earlier. Empty: https only.
	AllowHTTPHosts []string
}

// Sink is the gitlab-pipeline-trigger sink. It is safe for concurrent use.
type Sink struct {
	client    *http.Client
	now       func() time.Time
	httpHosts []string
}

var _ sink.Sink = (*Sink)(nil)

// New returns a gitlab-pipeline-trigger sink sending through client, which
// must be the netguard client (netguard.Client.HTTPClient). The sink uses a
// copy of it that does not follow redirects. New panics if client is nil.
func New(client *http.Client, opts Options) *Sink {
	if client == nil {
		panic("gitlabtrigger.New: nil client")
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Sink{client: &c, now: opts.Now, httpHosts: slices.Clone(opts.AllowHTTPHosts)}
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
	if cfg.base.Scheme == "http" && !slices.ContainsFunc(s.httpHosts, func(h string) bool { return strings.EqualFold(h, cfg.base.Hostname()) }) {
		return sink.Result{}, sink.Permanent(fmt.Errorf("gitlab-pipeline-trigger: %s: http:// is not allowed for this host (http.allow_http_hosts); use https://", origin))
	}
	envelope := req.Event.JSON
	if len(envelope) == 0 {
		if envelope, err = json.Marshal(req.Event.Envelope); err != nil {
			return sink.Result{}, sink.Permanent(errors.New("gitlab-pipeline-trigger: the event cannot be encoded"))
		}
	}
	vars, inputs, err := payload(ctx, cfg, req, envelope)
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
		return sink.Result{}, fmt.Errorf("gitlab-pipeline-trigger: %s: not resolved", ref)
	}
	token := strings.TrimSpace(string(v.Reveal()))
	if !validToken(token) {
		// Retried: the operator fixes it by setting the value.
		return sink.Result{}, fmt.Errorf("gitlab-pipeline-trigger: %s: invalid token (empty, or holds white space or control characters)", ref)
	}
	body, err := encode(struct {
		Token     string            `json:"token"`
		Ref       string            `json:"ref"`
		Variables map[string]string `json:"variables,omitempty"`
		Inputs    map[string]string `json:"inputs,omitempty"`
	}{token, cfg.Ref, vars, inputs})
	if err != nil {
		return sink.Result{}, sink.Permanent(errors.New("gitlab-pipeline-trigger: the payload cannot be encoded"))
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()
	target := strings.TrimRight(cfg.base.String(), "/") + apiSuffix + "/projects/" + url.PathEscape(cfg.Project) + "/trigger/pipeline"
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return sink.Result{}, sink.Permanent(fmt.Errorf("gitlab-pipeline-trigger: %s: invalid request", origin))
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(hreq) //nolint:gosec // G704: the client is netguard's, whose RoundTrip checks every hop.
	if err != nil {
		return sink.Result{}, requestError(origin, cfg.base.Hostname(), err)
	}
	peek, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyPeek))
	_, _ = io.CopyN(io.Discard, resp.Body, maxResponseRead)
	_ = resp.Body.Close()
	res := sink.Result{StatusCode: resp.StatusCode}
	if err := s.classify(resp); err != nil {
		return res, &statusError{origin: origin, code: resp.StatusCode, variables: resp.StatusCode == http.StatusBadRequest && bytes.Contains(peek, []byte(variablesRefusal)), err: err}
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

// validToken reports whether t is printable ASCII with no white space.
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

// classify maps a response to nil (success), a permanent error or a retried
// one (spec decision 56, D74): 2xx is success, 3xx and 4xx other than
// 408/429 are permanent, 408, 429 and 5xx are retried after the larger of
// Retry-After and RateLimit-Reset.
func (s *Sink) classify(resp *http.Response) error {
	now := s.now()
	wait := netguard.ParseRetryAfter(resp.Header.Get("Retry-After"), now)
	if reset, err := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("RateLimit-Reset")), 10, 64); err == nil {
		wait = max(wait, min(time.Unix(reset, 0).Sub(now), maxRateLimitWait))
	}
	return sink.HTTPStatus(resp.StatusCode, wait)
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

// payload builds the variables and inputs to send. With neither configured it
// is the default variables.
func payload(ctx context.Context, cfg parsed, req sink.Request, envelope []byte) (vars, inputs map[string]string, err error) {
	if len(cfg.Variables) == 0 && len(cfg.Inputs) == 0 {
		return defaultVariables(req), nil, nil
	}
	render1 := func(kind string, m map[string]string) (map[string]string, error) {
		if len(m) == 0 {
			return nil, nil
		}
		out := make(map[string]string, len(m))
		for k, src := range m {
			v, err := renderTemplate(ctx, kind+" "+k, src, envelope)
			if err != nil {
				return nil, err
			}
			out[k], _ = cut(v, MaxValueBytes)
		}
		return out, nil
	}
	if vars, err = render1("variable", cfg.Variables); err != nil {
		return nil, nil, err
	}
	if inputs, err = render1("input", cfg.Inputs); err != nil {
		return nil, nil, err
	}
	return vars, inputs, nil
}

// defaultVariables is what a pipeline gets when no variables or inputs are
// configured (D71); a null field is the empty string. ANGELIA_TRUNCATED is
// set only when a value was cut.
func defaultVariables(req sink.Request) map[string]string {
	e := req.Event.Envelope
	d := e.Data
	opt := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	watch := d.Watch.ID
	if d.Watch.Name != nil {
		watch = *d.Watch.Name
	}
	all := map[string]string{
		"ANGELIA_EVENT_ID":         e.ID,
		"ANGELIA_EVENT_TYPE":       event.ShortType(e.Type),
		"ANGELIA_SOURCE":           d.Source,
		"ANGELIA_PACKAGE":          d.Package,
		"ANGELIA_VERSION":          d.Version,
		"ANGELIA_PREVIOUS_VERSION": opt(d.PreviousVersion),
		"ANGELIA_BUMP":             opt(d.Bump),
		"ANGELIA_PURL":             opt(d.PURL),
		"ANGELIA_RELEASE_URL":      opt(d.URLs.Release),
		"ANGELIA_WATCH":            watch,
		"ANGELIA_IDEMPOTENCY_KEY":  req.IdempotencyKey,
	}
	var truncated bool
	for k, v := range all {
		var c bool
		all[k], c = cut(v, MaxValueBytes)
		truncated = truncated || c
	}
	if truncated {
		all["ANGELIA_TRUNCATED"] = "true"
	}
	return all
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

// renderTemplate renders one variable or input template on the event; what
// names it for errors ("variable NAME"), never the template text.
func renderTemplate(ctx context.Context, what, src string, envelope []byte) (string, error) {
	if src == "" {
		return "", nil // render.Compile refuses an empty template
	}
	t, err := render.Compile(src, render.Limits{})
	if err != nil {
		return "", fmt.Errorf("gitlab-pipeline-trigger: %s template: %w", what, err)
	}
	r, err := t.Render(ctx, envelope)
	if err != nil {
		return "", fmt.Errorf("gitlab-pipeline-trigger: %s template: %w", what, err)
	}
	return string(r.Body), nil
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
	return &requestFailure{origin: origin, class: sink.TransportClass(err), err: err}
}

// requestFailure is a request that got no response: "gitlab-pipeline-trigger:
// <scheme://host>: <class>". Unwrap gives the cause; Error never repeats it.
type requestFailure struct {
	origin, class string
	err           error
}

func (e *requestFailure) Error() string {
	return "gitlab-pipeline-trigger: " + e.origin + ": " + e.class
}
func (e *requestFailure) Unwrap() error { return e.err }

// blockedError is a destination the SSRF guard refused.
type blockedError struct{ host string }

func (e blockedError) Error() string {
	return "gitlab-pipeline-trigger: destination not allowed (" + e.host + ")"
}
func (blockedError) Unwrap() error { return netguard.ErrBlocked }

// statusError is a response that is not a 2xx: "gitlab-pipeline-trigger:
// <scheme://host>: HTTP <code> <text> (<hint>)". It wraps the error that
// carries the class (permanent or not) and Retry-After.
type statusError struct {
	origin string
	code   int
	// variables is set for a 400 whose body says that pipeline variables are
	// not permitted.
	variables bool
	err       error
}

func (e *statusError) Error() string {
	msg := "gitlab-pipeline-trigger: " + e.origin + ": " + strings.TrimPrefix(e.err.Error(), "sink: ")
	if h := hint(e.code, e.variables); h != "" {
		msg += " (" + h + ")"
	}
	return msg
}
func (e *statusError) Unwrap() error { return e.err }

// hint says what GitLab means by a status, for the operator.
func hint(code int, variables bool) string {
	switch code {
	case http.StatusBadRequest:
		if variables {
			return "GitLab does not let this token set pipeline variables: use inputs, or lower the project's \"Minimum role to use pipeline variables\""
		}
		return "GitLab refused the ref, variables or inputs"
	case http.StatusUnauthorized, http.StatusForbidden:
		return "the trigger token is not accepted or lacks access"
	case http.StatusNotFound:
		return "project not found or token without access"
	}
	return ""
}
