package dispatch

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/sink"
)

// URLChecker is the SSRF guard's URL check: *netguard.Client.
type URLChecker interface {
	CheckURL(ctx context.Context, u *url.URL) error
}

// checkSecretDestinations derives, for each reference marked
// x-angelia-secret-destination, the origins the sink will send to and checks
// each one before Deliver (ADR 0026 §2):
//   - https, or http only for a host in http.allow_http_hosts;
//   - the SSRF guard's URL check (Options.URLCheck);
//   - the secret's hosts, when it has any.
//
// It returns the canonical "host:port" of the checked origins, for
// sink.Request.Destinations. Every failure is retried: the operator fixes
// it by rewriting the secret. Errors start with the sink kind ("shoutrrr:
// secret /url_ref (secret chat): https://host: ...") and name the pointer,
// the secret or variable and the origin; the sink's own derivation errors
// add the service. Never the value or the address a host resolves to.
func (d *Dispatcher) checkSecretDestinations(ctx context.Context, kind string, s sink.Sink, refs []selfAddressed) ([]string, error) {
	sd, ok := s.(sink.SecretDestinations)
	if !ok {
		return nil, fmt.Errorf("dispatch: sink kind %s marks x-angelia-secret-destination but does not derive its destinations", kind)
	}
	var out []string
	for _, r := range refs {
		prefix := fmt.Sprintf("%s: secret %s (%s): ", kind, r.ptr, r.label)
		fail := func(format string, a ...any) error {
			return &checkError{msg: prefix + fmt.Sprintf(format, a...)}
		}
		origins, err := sd.SecretDestinations(r.ptr, r.bound.Value)
		if err != nil {
			// The sink's error names the service and the origin only; its
			// own kind prefix is not repeated.
			return nil, &checkError{msg: prefix + strings.TrimPrefix(err.Error(), kind+": "), err: err}
		}
		if len(origins) == 0 {
			return nil, fail("no destination derived")
		}
		for _, o := range origins {
			if o == nil || o.Host == "" || o.User != nil || o.Opaque != "" || (o.Path != "" && o.Path != "/") || o.RawQuery != "" || o.Fragment != "" {
				return nil, fail("the sink derived a destination that is not an origin")
			}
			origin := strings.ToLower(o.Scheme) + "://" + o.Host
			switch strings.ToLower(o.Scheme) {
			case "https":
			case "http":
				if !slices.ContainsFunc(d.opts.AllowHTTPHosts, func(h string) bool { return strings.EqualFold(h, o.Hostname()) }) {
					return nil, fail("%s: http:// is not allowed for this host (http.allow_http_hosts)", origin)
				}
			default:
				return nil, fail("%s: scheme not allowed", origin)
			}
			if d.opts.URLCheck == nil {
				return nil, fail("%s: no SSRF guard configured for this dispatcher", origin)
			}
			if err := d.opts.URLCheck.CheckURL(ctx, o); err != nil {
				return nil, &checkError{msg: prefix + origin + ": " + guardClass(err), err: err}
			}
			if err := r.bound.Allows(o); err != nil {
				return nil, &checkError{msg: prefix + origin + ": " + err.Error(), err: err}
			}
			key, err := secrets.Destination(o)
			if err != nil {
				return nil, fail("%s: invalid destination", origin)
			}
			if !slices.Contains(out, key) {
				out = append(out, key)
			}
		}
	}
	return out, nil
}

// guardClass describes an SSRF guard refusal without the address the host
// resolved to.
func guardClass(err error) string {
	var be *netguard.BlockedError
	switch {
	case errors.As(err, &be), errors.Is(err, netguard.ErrBlocked):
		return "destination not allowed by the SSRF guard (http.allow_networks)"
	case errors.Is(err, netguard.ErrInvalidURL):
		return "invalid destination (scheme or host not allowed)"
	default:
		return "DNS lookup failed"
	}
}

// checkError is a failed destination check. Its message is built from
// fixed text, the reference and the origin (never an address a host
// resolved to); Unwrap gives the cause, for errors.Is, or nil.
type checkError struct {
	msg string
	err error
}

func (e *checkError) Error() string { return e.msg }
func (e *checkError) Unwrap() error { return e.err }
