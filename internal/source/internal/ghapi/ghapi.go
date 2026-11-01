// Package ghapi holds the GitHub-specific plumbing shared by the GitHub-API
// based sources (github-release, github-tag): the default base URL, the
// "owner/name" coordinate parser and the authenticated GET helper. The token
// and redirect plumbing is in package apiclient.
//
// It is internal to internal/source and carries no source-specific logic.
package ghapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/internal/apiclient"
)

// DefaultBaseURL is the GitHub.com API. For GitHub Enterprise use
// "https://HOST/api/v3".
const DefaultBaseURL = "https://api.github.com"

var (
	ownerRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	nameRe  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

// Client sends GitHub API requests.
type Client struct {
	base   string
	cfgErr error
	hc     *http.Client
}

// Base is the API base URL without a trailing slash.
func (c *Client) Base() string { return c.base }

// Err is an invalid configuration, to be reported by the source's List.
func (c *Client) Err() error { return c.cfgErr }

// New returns a Client for baseURL (DefaultBaseURL if empty) using hc,
// normally the netguard client. hc must not be nil. An invalid baseURL, or a
// token with a plain-http non-loopback base URL, does not panic: it is left
// in Err.
//
// New works on a shallow copy of hc whose CheckRedirect refuses to follow a
// redirect that would carry the token to another host or to plain http (see
// apiclient.GuardRedirects). kind prefixes that refusal message.
//
// New only reads whether tok is set; the sources keep the Token themselves to
// resolve it per List call or per page.
func New(kind, baseURL string, hc *http.Client, tok apiclient.Token) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	c := &Client{base: strings.TrimRight(baseURL, "/")}
	c.hc = apiclient.GuardRedirects(kind, hc, func(req *http.Request, _ []*http.Request) bool {
		return req.Header.Get("Authorization") != ""
	})
	u, err := url.Parse(c.base)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil:
		c.cfgErr = errors.New("invalid base URL: want an absolute http(s) URL without credentials")
	case tok.Set() && !apiclient.TokenSafe(u):
		c.cfgErr = apiclient.ErrBasePlain
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
func (c *Client) Get(ctx context.Context, u string, v netguard.Validators, tok apiclient.Secret, notFound string) (*netguard.Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, errors.New("build request: invalid base URL")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if tok != "" {
		if !apiclient.TokenSafe(req.URL) {
			return nil, apiclient.ErrBasePlain
		}
		req.Header.Set("Authorization", "Bearer "+string(tok))
	}
	return apiclient.Fetch(ctx, c.hc, req, v, notFound)
}
