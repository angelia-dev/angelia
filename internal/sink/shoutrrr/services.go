package shoutrrr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"regexp"
	"strings"

	"github.com/nicholas-fedor/shoutrrr/pkg/services/chat/discord"
	"github.com/nicholas-fedor/shoutrrr/pkg/services/chat/matrix"
	"github.com/nicholas-fedor/shoutrrr/pkg/services/chat/slack"
	"github.com/nicholas-fedor/shoutrrr/pkg/services/chat/telegram"
	"github.com/nicholas-fedor/shoutrrr/pkg/services/push/gotify"
	"github.com/nicholas-fedor/shoutrrr/pkg/services/push/ntfy"
	"github.com/nicholas-fedor/shoutrrr/pkg/types"

	"github.com/angelia-dev/angelia/internal/secrets"
)

// Services are the allowed shoutrrr services (URL schemes), ADR 0026 §3.
// Adding one needs its origin derivation, its injection test and a docs
// entry.
var Services = []string{"slack", "discord", "telegram", "ntfy", "gotify", "matrix"}

// sender is what the sink needs of a shoutrrr service: every allowed
// service implements it.
type sender interface {
	Initialize(serviceURL *url.URL, logger types.StdLogger) error
	SendContext(ctx context.Context, message string, params *types.Params) error
	SetHTTPClient(client types.HTTPClient)
}

// service is one allowed scheme: how to build a fresh service value, and
// how to derive the origins it sends to from the initialized value.
type service struct {
	name    string
	new     func() sender
	origins func(sender) ([]*url.URL, error)
}

var services = map[string]service{
	"slack": {"slack", func() sender { return &slack.Service{} }, func(s sender) ([]*url.URL, error) {
		// A webhook token posts to hooks.slack.com; an xoxb/xoxp token to the
		// Web API on slack.com.
		if s.(*slack.Service).Config.Token.IsAPIToken() {
			return fixed("https://slack.com"), nil
		}
		return fixed("https://hooks.slack.com"), nil
	}},
	"discord": {"discord", func() sender { return &discord.Service{} }, func(sender) ([]*url.URL, error) {
		return fixed("https://discord.com"), nil
	}},
	"telegram": {"telegram", func() sender { return &telegram.Service{} }, func(s sender) ([]*url.URL, error) {
		c := s.(*telegram.Service).Config
		if c.SkipTLSVerify {
			return nil, errSkipTLS
		}
		if c.APIURL == "" {
			return fixed("https://api.telegram.org"), nil
		}
		u, err := url.Parse(c.APIURL)
		if err != nil || u.Host == "" || u.User != nil {
			return nil, errors.New("the apiurl option is not an http(s) URL with a host")
		}
		return origin(u.Scheme, u.Host)
	}},
	"ntfy": {"ntfy", func() sender { return &ntfy.Service{} }, func(s sender) ([]*url.URL, error) {
		c := s.(*ntfy.Service).Config
		if c.SkipTLSVerify || c.DisableTLSVerification { //nolint:staticcheck // SA1019: the deprecated alias still turns verification off
			return nil, errSkipTLS
		}
		// GetAPIURL is where the service publishes: the URL host (ntfy.sh by
		// default), over http with disabletls=yes or the deprecated scheme=http.
		u, err := url.Parse(c.GetAPIURL())
		if err != nil {
			return nil, errors.New("the server URL does not parse")
		}
		return origin(u.Scheme, u.Host)
	}},
	"gotify": {"gotify", func() sender { return &gotify.Service{} }, func(s sender) ([]*url.URL, error) {
		c := s.(*gotify.Service).Config
		if c.SkipTLSVerify || c.InsecureSkipVerify { //nolint:staticcheck // SA1019: the deprecated alias still turns verification off
			return nil, errSkipTLS
		}
		raw, err := (&gotify.DefaultURLBuilder{}).BuildURL(c)
		if err != nil {
			return nil, errors.New("the server URL cannot be built")
		}
		u, err := url.Parse(raw)
		if err != nil {
			return nil, errors.New("the server URL does not parse")
		}
		return origin(u.Scheme, u.Host)
	}},
	"matrix": {"matrix", func() sender { return &matrix.Service{} }, func(s sender) ([]*url.URL, error) {
		c := s.(*matrix.Service).Config
		if c.SkipTLSVerify {
			return nil, errSkipTLS
		}
		if c.User != "" {
			// A password login would run on every delivery (the service is
			// not cached) and send the password each time (decided question 2).
			return nil, errors.New("only the access-token form matrix://:TOKEN@host is allowed, not a user name and password")
		}
		scheme := "https"
		if c.DisableTLS {
			scheme = "http"
		}
		return origin(scheme, c.Host)
	}},
}

var errSkipTLS = errors.New("skiptlsverify is not supported: TLS is always verified; add the server's CA to the system trust store")

func fixed(raw string) []*url.URL {
	u, _ := url.Parse(raw)
	return []*url.URL{u}
}

func origin(scheme, host string) ([]*url.URL, error) {
	scheme = strings.ToLower(scheme)
	if (scheme != "https" && scheme != "http") || host == "" {
		return nil, errors.New("the server is not an http(s) host")
	}
	return []*url.URL{{Scheme: scheme, Host: strings.ToLower(host)}}, nil
}

// schemeRE is what a scheme may look like to be named in an error.
var schemeRE = regexp.MustCompile(`^[a-z][a-z0-9+.-]{0,31}$`)

// discardLogger receives the library's own logging, which can quote
// configuration values.
var discardLogger = log.New(io.Discard, "", 0)

// parsed is a shoutrrr URL read by its service.
type parsed struct {
	svc     service
	sender  sender
	origins []*url.URL
}

// parseServiceURL reads a shoutrrr URL with a new value of its service and
// derives its origins. It does no I/O. Its errors name the service at
// most, never the URL or the text of a url.Parse or library error, which
// quote their input.
func parseServiceURL(v secrets.Value) (parsed, error) {
	u, err := url.Parse(strings.TrimSpace(string(v.Reveal())))
	if err != nil || u.Scheme == "" {
		return parsed{}, errors.New("shoutrrr: the value is not a shoutrrr service URL")
	}
	scheme := strings.ToLower(u.Scheme)
	svc, ok := services[scheme]
	if !ok {
		name := "with this scheme"
		if schemeRE.MatchString(scheme) {
			name = fmt.Sprintf("%q", scheme)
		}
		return parsed{}, fmt.Errorf("shoutrrr: service %s is not allowed; use one of %s", name, strings.Join(Services, ", "))
	}
	u.Scheme = scheme
	s := svc.new()
	if err := s.Initialize(u, discardLogger); err != nil {
		return parsed{}, fmt.Errorf("shoutrrr: %s: the service URL is invalid", svc.name)
	}
	origins, err := svc.origins(s)
	if err != nil {
		return parsed{}, fmt.Errorf("shoutrrr: %s: %w", svc.name, err)
	}
	return parsed{svc: svc, sender: s, origins: origins}, nil
}
