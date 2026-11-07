// Package shoutrrr is the "shoutrrr" sink (ADR 0026): it sends a plain-text
// message to one chat or push service (slack, discord, telegram, ntfy,
// gotify, matrix) through the github.com/nicholas-fedor/shoutrrr service
// packages, never its router.
//
// The whole shoutrrr service URL is a secret, named by url_ref (a stored
// secret) or url_env (an environment variable) and resolved by the core.
// The kind's config_schema marks both x-angelia-secret-destination: the
// core calls SecretDestinations to derive the HTTP origins the service
// sends to, checks each (https or http.allow_http_hosts, the SSRF guard,
// the secret's hosts) and passes them as sink.Request.Destinations.
//
// Each attempt builds a new service value, initialized with a discarded
// logger, whose HTTP client is the netguard client's transport pinned to
// those origins, without redirects. The outcome is classified from what
// that client saw, never from the library's errors, which can carry
// response bodies, URLs or tokens and are never wrapped. The URL appears
// in no log, error, span, API response or event.
package shoutrrr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/sink"
)

// Kind is the registry key of this sink.
const Kind = "shoutrrr"

// Timeout bounds of the timeout key (spec ShoutrrrSinkConfig).
const (
	DefaultTimeout = 10 * time.Second
	MinTimeout     = time.Second
	MaxTimeout     = 2 * time.Minute
)

// URLRefPointer and URLEnvPointer are the keys of the service URL in
// sink.Request.Secrets.
const (
	URLRefPointer = "/url_ref"
	URLEnvPointer = "/url_env"
)

// Config is the configuration of a shoutrrr sink: the name of the stored
// secret or variable that holds the URL, never the URL.
type Config struct {
	URLRef  string `json:"url_ref,omitempty"`
	URLEnv  string `json:"url_env,omitempty"`
	Service string `json:"service,omitempty"`
	Timeout string `json:"timeout,omitempty"`
}

// Options configures a Sink.
type Options struct {
	// Now is the clock that Retry-After dates are read against. Default
	// time.Now.
	Now func() time.Time
}

// Sink is the shoutrrr sink. It is safe for concurrent use.
type Sink struct {
	client *http.Client
	now    func() time.Time
	// services is the table of allowed services; nil means serviceTable().
	// Tests replace an origin derivation to exercise the pin.
	services map[string]service
}

var (
	_ sink.Sink               = (*Sink)(nil)
	_ sink.SecretDestinations = (*Sink)(nil)
)

// New returns a shoutrrr sink sending through client, which must be the
// netguard client (netguard.Client.HTTPClient): its transport carries every
// request. New panics if client or its Transport is nil: a nil Transport
// would send through http.DefaultTransport, around netguard.
func New(client *http.Client, opts Options) *Sink {
	if client == nil || client.Transport == nil {
		panic("shoutrrr.New: nil client or transport; pass the netguard client")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Sink{client: client, now: opts.Now, services: serviceTable()}
}

// table returns the service table (a zero Sink, as config check uses for
// SecretDestinations, gets the default one).
func (s *Sink) table() map[string]service {
	if s.services == nil {
		return serviceTable()
	}
	return s.services
}

// Kind implements sink.Sink.
func (*Sink) Kind() string { return Kind }

// Validate implements sink.Sink: the shape of the configuration. The URL
// itself is a secret, checked at every delivery.
func (*Sink) Validate(cfg sink.Config) error {
	_, err := ParseConfig(cfg)
	return err
}

// Parsed is a valid configuration.
type Parsed struct {
	Config
	Timeout time.Duration
}

// Ref names the URL's reference for errors ("url_ref NAME" or "url_env
// NAME") and gives its key in sink.Request.Secrets.
func (p Parsed) Ref() (ref, ptr string) {
	if p.URLRef != "" {
		return "url_ref " + p.URLRef, URLRefPointer
	}
	return "url_env " + p.URLEnv, URLEnvPointer
}

// ParseConfig validates a shoutrrr sink configuration.
func ParseConfig(raw sink.Config) (Parsed, error) {
	var p Parsed
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p.Config); err != nil {
		return p, errors.New("shoutrrr: invalid configuration: want an object with url_ref or url_env, service and timeout")
	}
	switch {
	case (p.URLRef == "") == (p.URLEnv == ""):
		return p, errors.New("shoutrrr: invalid configuration: set exactly one of url_ref and url_env")
	case p.URLRef != "" && !secrets.ValidName(p.URLRef):
		return p, errors.New("shoutrrr: invalid configuration: url_ref must be the name of a stored secret")
	case p.URLEnv != "" && (!config.IsEnvName(p.URLEnv) || config.LooksLikeToken(p.URLEnv)):
		return p, errors.New("shoutrrr: invalid configuration: url_env must be an environment variable name")
	case p.Service != "" && !slices.Contains(Services(), p.Service):
		return p, fmt.Errorf("shoutrrr: invalid configuration: service must be one of %s", strings.Join(Services(), ", "))
	}
	p.Timeout = DefaultTimeout
	if p.Config.Timeout != "" {
		d, err := time.ParseDuration(p.Config.Timeout)
		if err != nil || d < MinTimeout || d > MaxTimeout {
			return p, fmt.Errorf("shoutrrr: invalid configuration: timeout must be a duration from %v to %v", MinTimeout, MaxTimeout)
		}
		p.Timeout = d
	}
	return p, nil
}

// SecretDestinations implements sink.SecretDestinations: it reads the URL
// with its service, without I/O, and returns the origins the service sends
// to. A scheme outside Services, skiptlsverify and a matrix user name are
// refused. Errors name the service at most.
func (s *Sink) SecretDestinations(ptr string, v secrets.Value) ([]*url.URL, error) {
	if ptr != URLRefPointer && ptr != URLEnvPointer {
		return nil, fmt.Errorf("shoutrrr: %s is not a service URL key", ptr)
	}
	p, err := parseServiceURL(s.table(), v)
	if err != nil {
		return nil, err
	}
	return p.origins, nil
}

// Deliver implements sink.Sink: one send through a new service value. The
// attempt is bounded by min(timeout, the deadline of ctx).
func (s *Sink) Deliver(ctx context.Context, req sink.Request) (sink.Result, error) {
	cfg, err := ParseConfig(req.Config)
	if err != nil {
		return sink.Result{}, sink.Permanent(err)
	}
	ref, ptr := cfg.Ref()
	v, ok := req.Secrets[ptr]
	if !ok {
		return sink.Result{}, fmt.Errorf("shoutrrr: %s: not resolved", ref)
	}
	// Everything up to the send comes from the secret's value: retried, the
	// operator fixes it by rewriting the secret (ADR 0026 §2).
	p, err := parseServiceURL(s.table(), v)
	if err != nil {
		return sink.Result{}, fmt.Errorf("shoutrrr: %s: %w", ref, err)
	}
	name, first := p.svc.name, p.origins[0].String()
	if cfg.Service != "" && cfg.Service != name {
		return sink.Result{}, fmt.Errorf("shoutrrr: %s: the URL is for %s, but the sink's service is %s", ref, name, cfg.Service)
	}
	allowed := make([]string, 0, len(p.origins))
	for _, o := range p.origins {
		hp, err := secrets.Destination(o)
		if err != nil || !slices.Contains(req.Destinations, hp) {
			// The core checks the origins before Deliver; without them nothing
			// is sent (fail closed).
			return sink.Result{}, fmt.Errorf("shoutrrr: %s %s: destination not checked by the dispatcher", name, o)
		}
		allowed = append(allowed, pinKey(o.Scheme, hp))
	}
	msg, err := message(ctx, req)
	if err != nil {
		return sink.Result{}, sink.Permanent(fmt.Errorf("shoutrrr: %s %s: %w", name, first, err))
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	client, pinned := pinnedClient(s.client, allowed, s.now)
	p.sender.SetHTTPClient(client)
	sendErr := p.sender.SendContext(ctx, msg, nil)
	return outcome(name, first, pinned, sendErr, ctx.Err())
}

// outcome classifies an attempt from what the pinned client observed (ADR
// 0026 §5). The library's error only says whether the send failed; its
// text is never kept.
func outcome(service, first string, p *pin, sendErr, ctxErr error) (sink.Result, error) {
	requests, refused, last := p.seen()
	res := sink.Result{StatusCode: last.status}
	where := first
	if last.origin != "" {
		where = last.origin
	}
	fail := func(class string) error { return &failure{msg: "shoutrrr: " + service + " " + where + ": " + class} }
	switch {
	case refused > 0:
		// Permanent: the library sent somewhere the derivation did not
		// predict, so retrying sends there again.
		return res, sink.Permanent(&failure{msg: "shoutrrr: " + service + " " + where + ": " + errUnpinned.Error() + " (the service's destinations changed; this Angelia version does not support it)", err: errUnpinned})
	case requests > 0 && last.err == nil && (last.status < 200 || last.status > 299):
		// Before the library's verdict: some services return nil on a
		// non-2xx (slack's webhook on a body of "ok", gotify below 400).
		se := sink.HTTPStatus(last.status, last.retryAfter)
		return res, &failure{msg: "shoutrrr: " + service + " " + where + ": " + strings.TrimPrefix(se.Error(), "sink: "), err: se}
	case sendErr == nil:
		return res, nil
	case requests > 0 && last.err != nil:
		return res, &failure{msg: "shoutrrr: " + service + " " + where + ": " + sink.TransportClass(last.err), err: last.err}
	case ctxErr != nil:
		return res, &failure{msg: "shoutrrr: " + service + " " + where + ": " + sink.TransportClass(ctxErr), err: ctxErr}
	case requests > 0:
		// For example telegram's ok:false after a 2xx.
		return res, fail("service error")
	default:
		return res, fail("service error (no request sent)")
	}
}

// failure is a failed attempt: "shoutrrr: <service> <origin>: <class or
// HTTP status>". Unwrap gives the observed cause (a transport error or
// sink.HTTPStatus's error, which carries permanence and Retry-After), never
// the library's error.
type failure struct {
	msg string
	err error
}

func (e *failure) Error() string { return e.msg }
func (e *failure) Unwrap() error { return e.err }
