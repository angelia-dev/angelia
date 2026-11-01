// Package apiclient holds the token and redirect plumbing shared by the
// sources that call an authenticated HTTP API (github-release, github-tag,
// gitlab-release): the redacting token type, token providers, the test for
// where credentials may travel, the redirect guard that never carries
// credentials to another host or to plain http, and the fetch helper that
// adds the 404 and 401 hints.
//
// It is internal to internal/source and carries no provider-specific logic:
// header names, base URLs and coordinate parsing stay with the callers.
package apiclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/angelia-dev/angelia/internal/netguard"
)

// ErrBasePlain is reported when a token would travel over plain http to a
// non-loopback host.
var ErrBasePlain = errors.New("refusing to send the token over plain http to a non-loopback host")

// Secret hides a token from fmt, slog and JSON.
type Secret string

// String implements fmt.Stringer.
func (Secret) String() string { return "[REDACTED]" }

// GoString implements fmt.GoStringer.
func (Secret) GoString() string { return "[REDACTED]" }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

// MarshalText implements encoding.TextMarshaler (JSON and text encodings).
func (Secret) MarshalText() ([]byte, error) { return []byte("[REDACTED]"), nil }

// Token is a configured token provider. The zero value means "no token": Get
// returns an empty secret and no error.
type Token struct {
	fn  func() (Secret, error)
	set bool
}

// FixedToken is a Token with a fixed value. A blank value makes Get fail.
func FixedToken(token string) Token {
	return Token{set: true, fn: func() (Secret, error) {
		t := Secret(strings.TrimSpace(token))
		if t == "" {
			return "", errors.New("token is configured but blank")
		}
		return t, nil
	}}
}

// EnvToken reads the token from the environment variable name on every Get
// (so a rotated token is picked up). If the variable is unset, empty or
// blank, Get fails rather than silently falling back to anonymous access.
func EnvToken(name string) Token {
	return Token{set: true, fn: func() (Secret, error) {
		t := Secret(strings.TrimSpace(os.Getenv(name)))
		if t == "" {
			return "", fmt.Errorf("token_env %q is unset or empty", name)
		}
		return t, nil
	}}
}

// Set reports whether a token was configured.
func (t Token) Set() bool { return t.set }

// Get returns the current token.
func (t Token) Get() (Secret, error) {
	if t.fn == nil {
		return "", nil
	}
	return t.fn()
}

// TokenSafe reports whether credentials may be sent to u.
func TokenSafe(u *url.URL) bool {
	if u.Scheme == "https" {
		return true
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(u.Hostname(), "localhost")
}

// GuardRedirects returns a shallow copy of hc whose CheckRedirect refuses to
// follow a redirect that would carry credentials to another host or to plain
// http (net/http only compares host names, so it would keep a header across
// ports and subdomains). kind prefixes the refusal message.
//
// hasCredentials says whether the request chain carries credentials; req is
// the request about to be sent and via the requests already made, oldest
// first. Which to read depends on the credential header. net/http drops
// Authorization when a redirect goes to another domain, so req may no longer
// show it: that is fine when the guard only has to stop a header that is
// still there, and reading req is enough. A header that net/http forwards
// to any host, such as GitLab's PRIVATE-TOKEN, survives such hops, but a
// guard that looks at req alone would also lose track of Authorization sent
// first; read via[0], the request that carried the credentials, to judge
// the whole chain. After the guard, the caller's own CheckRedirect runs, or a cap of 10
// redirects if hc has none. hc itself is not modified.
func GuardRedirects(kind string, hc *http.Client, hasCredentials func(req *http.Request, via []*http.Request) bool) *http.Client {
	cp := *hc
	orig := cp.CheckRedirect
	cp.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if hasCredentials(req, via) {
			if !TokenSafe(req.URL) || !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
				return fmt.Errorf("%s: refusing to follow a redirect to %s with credentials", kind, req.URL.Host)
			}
		}
		if orig != nil {
			return orig(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &cp
}

// Fetch is netguard.Fetch with hints: a 404 is wrapped with notFound followed
// by " (or not visible to the token)", a 401 with a note that the token is
// invalid or expired.
func Fetch(ctx context.Context, hc *http.Client, req *http.Request, v netguard.Validators, notFound string) (*netguard.Result, error) {
	res, err := netguard.Fetch(ctx, hc, req, v)
	if err != nil {
		var se *netguard.StatusError
		if errors.As(err, &se) && se.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%s (or not visible to the token): %w", notFound, err)
		}
		if errors.As(err, &se) && se.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("the token is invalid or expired: %w", err)
		}
		return nil, err
	}
	return res, nil
}
