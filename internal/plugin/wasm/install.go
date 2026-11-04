package wasm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// Credential is a token the operator configured when installing a plugin,
// bound to one of its allowed hosts (ADR 0011, amendment of #145). The
// host adds it as the Authorization header of every hop to that host, over
// https only; the plugin never receives it: not in its config map, not in
// a response (an echo of the token is replaced by [REDACTED]) and not in
// an error message.
type Credential struct {
	// Host is the hostname or IP literal the credential is bound to. It
	// must be in the merged allowed_hosts. Exact match, any port.
	Host string
	// Scheme, if set, is written before the token: "Bearer" gives
	// "Authorization: Bearer <token>". Empty sends the token as is.
	Scheme string
	// Token returns the token. It is called for every hop to Host, so a
	// rotated value is picked up. It is the seam for the secret store: the
	// instance config resolves token_env today. Its error must not contain
	// the token.
	Token func(ctx context.Context) (string, error)
}

// HostGrant is one entry of a plugin's merged allowed_hosts, as Hosts
// shows it when the plugin is installed.
type HostGrant struct {
	Host string `json:"host"`
	// Manifest and Operator say who allowed the host; both may be true.
	Manifest bool `json:"manifest"`
	Operator bool `json:"operator"`
	// Credential is true when the host injects a credential for it.
	Credential bool `json:"credential"`
}

var schemeRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,31}$`)

// errCredential is wrapped when a credential cannot be attached to a hop.
var errCredential = errors.New("wasm: credential")

// redacted replaces an injected token echoed back to the plugin.
const redacted = "[REDACTED]"

// install merges the manifest's and the operator's allowed hosts and
// checks the credentials against them.
func (s *Source) install(m Manifest, opts Options) error {
	s.allowed = map[string]bool{}
	s.grants = map[string]*HostGrant{}
	grant := func(h string) *HostGrant {
		g := s.grants[h]
		if g == nil {
			g = &HostGrant{Host: h}
			s.grants[h] = g
			s.allowed[h] = true
		}
		return g
	}
	for _, h := range m.AllowedHosts {
		n, _ := normAllowedHost(h) // checked by Validate
		grant(n).Manifest = true
	}
	for _, h := range opts.AllowedHosts {
		n, err := normAllowedHost(h)
		if err != nil {
			return fmt.Errorf("wasm %s: operator allowed_hosts: %w", m.Kind, err)
		}
		grant(n).Operator = true
	}
	s.creds = make(map[string]Credential, len(opts.Credentials))
	for _, c := range opts.Credentials {
		n, err := normAllowedHost(c.Host)
		switch {
		case err != nil:
			return fmt.Errorf("wasm %s: credential: %w", m.Kind, err)
		case !s.allowed[n]:
			return fmt.Errorf("wasm %s: credential for %q: host is not in allowed_hosts", m.Kind, n)
		case s.creds[n].Token != nil:
			return fmt.Errorf("wasm %s: credential for %q given twice", m.Kind, n)
		case c.Token == nil:
			return fmt.Errorf("wasm %s: credential for %q has no Token", m.Kind, n)
		case c.Scheme != "" && !schemeRE.MatchString(c.Scheme):
			return fmt.Errorf("wasm %s: credential for %q: scheme must match %s", m.Kind, n, schemeRE)
		}
		s.creds[n] = c
		s.grants[n].Credential = true
	}
	return nil
}

// Hosts returns the merged allowed_hosts, sorted, with where each entry
// comes from and whether a credential is bound to it. It never holds a
// token.
func (s *Source) Hosts() []HostGrant {
	out := make([]HostGrant, 0, len(s.grants))
	for _, g := range s.grants {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// inject returns the request to send for one hop. The Authorization header
// is the host's on a host with a credential: the plugin's own value is
// replaced. Other hosts get the request unchanged, so a credential never
// follows a redirect to another host, allowed or not. The token is
// recorded in the call's state so that echoes are scrubbed.
func (t *allowlistTransport) inject(r *http.Request, host string) (*http.Request, error) {
	c, ok := t.creds[host]
	if !ok {
		return r, nil
	}
	if !strings.EqualFold(r.URL.Scheme, "https") {
		return nil, fmt.Errorf("%w for %q is only sent over https", errCredential, host)
	}
	tok, err := c.Token(r.Context())
	if err != nil {
		return nil, fmt.Errorf("%w for %q is unavailable: %w", errCredential, host, err)
	}
	v := tok
	if c.Scheme != "" {
		v = c.Scheme + " " + tok
	}
	if strings.TrimSpace(tok) == "" || !validHeader("Authorization", v) {
		return nil, fmt.Errorf("%w for %q is empty or not a valid header value", errCredential, host)
	}
	if cs, ok := r.Context().Value(callKey{}).(*callState); ok {
		cs.addSecret(tok)
	}
	out := r.Clone(r.Context())
	out.Header.Set("Authorization", v)
	return out, nil
}

// addSecret records a token injected during the call.
func (c *callState) addSecret(tok string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.secrets {
		if s == tok {
			return
		}
	}
	c.secrets = append(c.secrets, tok)
}

// scrub replaces every injected token in a response or an error message
// before the plugin sees it. It does not catch a transformed echo (encoded,
// split); the allowlist is what keeps that from leaving the instance.
func (c *callState) scrub(resp *wireHTTPResponse) {
	c.mu.Lock()
	secrets := c.secrets
	c.mu.Unlock()
	for _, sec := range secrets {
		resp.Body = bytes.ReplaceAll(resp.Body, []byte(sec), []byte(redacted))
		for k, vs := range resp.Headers {
			for i, v := range vs {
				resp.Headers[k][i] = strings.ReplaceAll(v, sec, redacted)
			}
		}
		if resp.Error != nil {
			resp.Error.Message = strings.ReplaceAll(resp.Error.Message, sec, redacted)
		}
		resp.Validators.ETag = strings.ReplaceAll(resp.Validators.ETag, sec, redacted)
		resp.Validators.LastModified = strings.ReplaceAll(resp.Validators.LastModified, sec, redacted)
	}
}
