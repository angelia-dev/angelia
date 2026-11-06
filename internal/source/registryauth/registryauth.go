// Package registryauth adds the credential of a private registry or mirror
// (PRD F16) to every request of a source's HTTP client, so the sources
// themselves stay credential-free.
//
// The credential is a stored secret resolved by name on every request (ADR
// 0024: values are not cached, and a reference needs a host binding) or a
// token read from the environment at startup. Each hop of a redirect is a
// request of its own, so it is checked too: a stored secret is sent only to
// the hosts it is bound to, an environment token only to the host of the
// source's base URL, and nothing that carries a credential goes in clear
// text through a proxy. A refusal is an error naming the secret and the
// host, never the value.
package registryauth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/angelia-dev/angelia/internal/secrets"
)

// ErrUnbound is wrapped when a secret referenced by a source has no hosts:
// a source credential must be bound (ADR 0024 §2).
var ErrUnbound = errors.New("registryauth: secret has no host binding")

// ErrClearProxy is wrapped when a credential would reach a proxy as plain
// http.
var ErrClearProxy = errors.New("registryauth: credential refused over plain http through a proxy")

// Resolver turns a secret name into its value for a destination;
// *secrets.Resolver implements it.
type Resolver interface {
	Resolve(ctx context.Context, name string, dest *url.URL) (secrets.Bound, error)
}

// Config is the credential of one source.
type Config struct {
	// TokenRef names the stored secret sent as "Authorization: Bearer"
	// (type bearer and npm_auth_token, whose header is the same).
	TokenRef string
	// PasswordRef names the stored secret sent with Username as HTTP basic
	// authentication. At most one of TokenRef and PasswordRef is set.
	PasswordRef string
	// Username is the basic-auth user name.
	Username string
	// Token is a token already read from the environment (token_env). It
	// is sent only to the host of BaseURL.
	Token secrets.Value
	// Resolver resolves the *Ref names; nil makes every request that
	// needs one fail.
	Resolver Resolver
	// BaseURL is the source's base URL, whose host an environment Token is
	// bound to.
	BaseURL *url.URL
	// Proxy chooses the proxy of a request, as http.Transport.Proxy does.
	// Default http.ProxyFromEnvironment, like the netguard client.
	Proxy func(*http.Request) (*url.URL, error)
}

// Enabled reports whether c carries a credential.
func (c Config) Enabled() bool {
	return c.TokenRef != "" || c.PasswordRef != "" || !c.Token.IsZero()
}

// Wrap returns a client like hc whose requests carry the credential. hc is
// not changed. With no credential configured it returns hc.
func Wrap(hc *http.Client, c Config) *http.Client {
	if !c.Enabled() {
		return hc
	}
	if c.Proxy == nil {
		c.Proxy = http.ProxyFromEnvironment
	}
	base := hc.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	out := *hc
	out.Transport = &transport{base: base, c: c}
	return &out
}

type transport struct {
	base http.RoundTripper
	c    Config
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	if err := t.authorize(r); err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	return t.base.RoundTrip(r)
}

// authorize sets the Authorization header of r, or fails.
func (t *transport) authorize(r *http.Request) error {
	c := t.c
	var user string
	var secret secrets.Value
	switch {
	case c.TokenRef != "" || c.PasswordRef != "":
		name := c.TokenRef
		if c.PasswordRef != "" {
			name, user = c.PasswordRef, c.Username
		}
		if c.Resolver == nil {
			return fmt.Errorf("registryauth: secret %s cannot be read: no secrets resolver", name)
		}
		b, err := c.Resolver.Resolve(r.Context(), name, r.URL)
		if err != nil {
			return fmt.Errorf("registryauth: %w", err)
		}
		if len(b.Hosts) == 0 {
			return fmt.Errorf("%w: %s must carry hosts naming the registry", ErrUnbound, name)
		}
		if err := b.Allows(r.URL); err != nil {
			return fmt.Errorf("registryauth: %w", err)
		}
		secret = b.Value
	default:
		// An environment token is not bound to hosts: it goes to the
		// host of the base URL and to no other hop.
		if !sameHost(c.BaseURL, r.URL) {
			return nil
		}
		secret = c.Token
	}
	if p, err := c.Proxy(r); err == nil && p != nil && r.URL.Scheme != "https" {
		return fmt.Errorf("%w: %s", ErrClearProxy, r.URL.Scheme+"://"+r.URL.Host)
	}
	if c.PasswordRef != "" {
		r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+string(secret.Reveal()))))
	} else {
		r.Header.Set("Authorization", "Bearer "+string(secret.Reveal()))
	}
	return nil
}

func sameHost(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	da, err1 := secrets.Destination(a)
	db, err2 := secrets.Destination(b)
	return err1 == nil && err2 == nil && da == db
}
