// Package ghapi holds the plumbing shared by the GitHub-API based sources
// (github-release, github-tag): the redacting token type, token options, the
// redirect guard that never carries a token to another host or to plain http,
// the "owner/name" coordinate parser and the authenticated GET helper.
//
// It is internal to internal/source and carries no source-specific logic.
package ghapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
)

// DefaultBaseURL is the GitHub.com API. For GitHub Enterprise use
// "https://HOST/api/v3".
const DefaultBaseURL = "https://api.github.com"

var (
	ownerRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	nameRe  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
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
// blank, Get fails rather than silently falling back to the anonymous limit.
func EnvToken(name string) Token {
	return Token{set: true, fn: func() (Secret, error) {
		t := Secret(strings.TrimSpace(os.Getenv(name)))
		if t == "" {
			return "", fmt.Errorf("token_env %q is unset or empty", name)
		}
		return t, nil
	}}
}

// Get returns the current token.
func (t Token) Get() (Secret, error) {
	if t.fn == nil {
		return "", nil
	}
	return t.fn()
}

// Client sends GitHub API requests.
type Client struct {
	// Base is the API base URL without a trailing slash.
	Base string
	// CfgErr is an invalid configuration, to be reported by the source's List.
	CfgErr error

	hc *http.Client
}

// New returns a Client for baseURL (DefaultBaseURL if empty) using hc,
// normally the netguard client. hc must not be nil. An invalid baseURL, or a
// token with a plain-http non-loopback base URL, does not panic: it is left
// in CfgErr.
//
// New works on a shallow copy of hc whose CheckRedirect refuses to follow a
// redirect that would carry the token to another host or to plain http
// (net/http only compares host names, so it would keep the header across
// ports and subdomains). kind prefixes that refusal message.
func New(kind, baseURL string, hc *http.Client, tok Token) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	c := &Client{Base: strings.TrimRight(baseURL, "/")}
	cp := *hc
	orig := cp.CheckRedirect
	cp.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.Header.Get("Authorization") != "" {
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
	c.hc = &cp
	u, err := url.Parse(c.Base)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil:
		c.CfgErr = errors.New("invalid base URL: want an absolute http(s) URL without credentials")
	case tok.set && !TokenSafe(u):
		c.CfgErr = ErrBasePlain
	}
	return c
}

// ParseRepo checks that t is of the given kind and splits its "repo"
// coordinate into owner and name.
func ParseRepo(kind string, t source.Target) (owner, name string, err error) {
	if t.Kind != kind {
		return "", "", fmt.Errorf("%w: kind %q, want %q", source.ErrInvalidTarget, t.Kind, kind)
	}
	repo := t.Coordinates["repo"]
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || !ownerRe.MatchString(owner) || !nameRe.MatchString(name) || name == "." || name == ".." {
		return "", "", fmt.Errorf("%w: coordinate repo must be owner/name, got %q", source.ErrInvalidTarget, repo)
	}
	return owner, name, nil
}

// Get fetches u with the GitHub headers and, if tok is not empty, a bearer
// token (never over plain http to a non-loopback host). A 404 is wrapped with
// notFound followed by " (or not visible to the token)", a 401 with a hint
// that the token is invalid or expired.
func (c *Client) Get(ctx context.Context, u string, v netguard.Validators, tok Secret, notFound string) (*netguard.Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, errors.New("build request: invalid base URL")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if tok != "" {
		if !TokenSafe(req.URL) {
			return nil, ErrBasePlain
		}
		req.Header.Set("Authorization", "Bearer "+string(tok))
	}
	res, err := netguard.Fetch(ctx, c.hc, req, v)
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
