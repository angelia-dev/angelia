package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/angelia-dev/angelia/internal/source/internal/apiclient"
)

// challenge is a parsed WWW-Authenticate challenge.
type challenge struct {
	scheme string // lower-case: "bearer" or "basic"
	params map[string]string
}

var paramRE = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_-]*)\s*=\s*(?:"([^"]*)"|([^\s,"]+))`)

// parseChallenges parses the WWW-Authenticate header values of a response.
func parseChallenges(h http.Header) []challenge {
	var out []challenge
	for _, v := range h.Values("Www-Authenticate") {
		v = strings.TrimSpace(v)
		scheme, rest, _ := strings.Cut(v, " ")
		if scheme == "" {
			continue
		}
		c := challenge{scheme: strings.ToLower(scheme), params: map[string]string{}}
		for _, m := range paramRE.FindAllStringSubmatch(rest, -1) {
			val := m[2]
			if val == "" {
				val = m[3]
			}
			c.params[strings.ToLower(m[1])] = val
		}
		out = append(out, c)
	}
	return out
}

// pickChallenge returns the Bearer challenge if there is one, else the first.
func pickChallenge(cs []challenge) (challenge, bool) {
	for _, c := range cs {
		if c.scheme == "bearer" {
			return c, true
		}
	}
	if len(cs) > 0 {
		return cs[0], true
	}
	return challenge{}, false
}

// realmURL validates the realm of a Bearer challenge: https, or http only for
// a host of allow_http_hosts (D43), and no credentials in the URL.
func (s *Source) realmURL(realm string) (*url.URL, error) {
	u, err := url.Parse(realm)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("the token realm of the registry is not an absolute http(s) URL")
	}
	if u.Scheme == "http" && !s.httpHosts[strings.ToLower(u.Hostname())] {
		return nil, fmt.Errorf("the token realm uses plain http on %s, which is not in http.allow_http_hosts", u.Hostname())
	}
	return u, nil
}

// token fetches a registry token from the realm of c. The request goes
// through the credential client when the source has one (D44): registryauth
// then sends the credential only if the stored secret is bound to the realm's
// host, and fails the request otherwise. Without one the request is anonymous
// and carries no Authorization header.
func (s *Source) token(ctx context.Context, c challenge, name string) (apiclient.Secret, error) {
	u, err := s.realmURL(c.params["realm"])
	if err != nil {
		return "", err
	}
	scope := c.params["scope"]
	if scope == "" {
		scope = "repository:" + name + ":pull"
	}
	q := u.Query()
	if svc := c.params["service"]; svc != "" {
		q.Set("service", svc)
	}
	q.Set("scope", scope)
	u.RawQuery = q.Encode()
	hc := s.hc
	if s.authHC != nil {
		hc = s.authHC
	}
	r, err := do(ctx, hc, http.MethodGet, u.String(), "", "application/json")
	if err != nil {
		return "", fmt.Errorf("token request to %s: %w", u.Host, err)
	}
	if s.authHC != nil && (r.status == http.StatusUnauthorized || r.status == http.StatusForbidden) {
		return "", fmt.Errorf("token request to %s: the realm refused the configured credential (sources.<name>.auth): %w", u.Host, statusError(r))
	}
	if r.status != http.StatusOK {
		return "", fmt.Errorf("token request to %s refused: %w", u.Host, statusError(r))
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(r.body, &body); err != nil {
		return "", fmt.Errorf("token response from %s: not JSON", u.Host)
	}
	tok := body.Token
	if tok == "" {
		tok = body.AccessToken
	}
	if tok == "" {
		return "", fmt.Errorf("token response from %s carries no token", u.Host)
	}
	return apiclient.Secret(tok), nil
}

var linkRE = regexp.MustCompile(`<([^>]*)>([^<]*)`)

// nextLink returns the target of the rel="next" Link, "" if there is none.
func nextLink(h http.Header) string {
	for _, v := range h.Values("Link") {
		for _, m := range linkRE.FindAllStringSubmatch(v, -1) {
			for _, p := range strings.Split(m[2], ";") {
				k, val, ok := strings.Cut(strings.TrimSpace(p), "=")
				if !ok || !strings.EqualFold(k, "rel") {
					continue
				}
				for _, rel := range strings.Fields(strings.Trim(val, `"`)) {
					if strings.EqualFold(rel, "next") {
						return m[1]
					}
				}
			}
		}
	}
	return ""
}
