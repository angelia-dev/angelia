package secrets

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var (
	nameRE      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	hostLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// MaxHosts bounds the hosts a secret is bound to.
const MaxHosts = 32

// ValidName reports whether s is a valid secret name, the value of a *_ref
// key: ^[a-z0-9][a-z0-9._-]{0,62}$.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// ValidHost reports whether s is a hosts entry (ADR 0024 §2), in the syntax
// of a plugin credential key: "host" (port 443) or "host:port", the host a
// lower-case hostname (IDNA as punycode) without a trailing dot, or a
// canonical IP literal ([addr]:port for IPv6 with a port). No wildcards.
func ValidHost(s string) bool {
	_, ok := hostKey(s)
	return ok
}

// hostKey is the canonical "host:port" of a hosts entry, with port 443 when
// none is written.
func hostKey(s string) (string, bool) {
	if canonicalHost(s) {
		return net.JoinHostPort(s, "443"), true
	}
	h, port, err := net.SplitHostPort(s)
	if err != nil {
		return "", false
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port || !canonicalHost(h) {
		return "", false
	}
	return net.JoinHostPort(h, port), true
}

// canonicalHost reports whether h is a lower-case hostname without a
// trailing dot, or an IP literal in its canonical form without a zone.
func canonicalHost(h string) bool {
	if a, err := netip.ParseAddr(h); err == nil {
		return a.Zone() == "" && a.String() == h
	}
	if h == "" || len(h) > 253 {
		return false
	}
	for l := range strings.SplitSeq(h, ".") {
		if !hostLabelRE.MatchString(l) {
			return false
		}
	}
	return true
}

// ErrHostMismatch is wrapped when a secret bound to hosts is refused for a
// destination outside them.
var ErrHostMismatch = errors.New("secrets: host not allowed")

// Destination is the canonical "host:port" of an http(s) URL: the host
// lower-cased, the port explicit (443 for https, 80 for http). A host that
// is not ASCII must be written as punycode; it is refused, never converted.
func Destination(u *url.URL) (string, error) {
	if u == nil {
		return "", errors.New("secrets: no destination URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", fmt.Errorf("secrets: destination scheme %q is not http or https", scheme)
	}
	host := strings.ToLower(u.Hostname())
	for i := range len(host) {
		if host[i] >= 0x80 {
			return "", errors.New("secrets: destination host is not ASCII; write it as punycode")
		}
	}
	if host == "" {
		return "", errors.New("secrets: destination URL has no host")
	}
	if a, err := netip.ParseAddr(host); err == nil {
		host = a.String()
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if scheme == "http" {
			port = "80"
		}
	}
	return net.JoinHostPort(host, port), nil
}

// Bound is a resolved secret with the hosts it may be sent to. Its error
// messages name the secret and the destination's scheme and host, never
// the value.
type Bound struct {
	Name  string
	Value Value
	// Hosts is the secret's binding; empty means unbound.
	Hosts []string
}

// Allows checks that the secret may be sent to u: always when it is
// unbound, otherwise only when u's canonical host and port equal one entry
// of Hosts. A refusal wraps ErrHostMismatch.
func (b Bound) Allows(u *url.URL) error { return allows(b.Name, b.Hosts, u) }

func allows(name string, hosts []string, u *url.URL) error {
	if len(hosts) == 0 {
		return nil
	}
	dest, err := Destination(u)
	if err != nil {
		return fmt.Errorf("%w: secret %s is bound to hosts: %w", ErrHostMismatch, name, err)
	}
	for _, h := range hosts {
		if k, ok := hostKey(h); ok && k == dest {
			return nil
		}
	}
	return fmt.Errorf("%w: secret %s is bound to %s; refused for %s", ErrHostMismatch, name, strings.Join(hosts, ", "), dest)
}

// CheckRedirect returns an http.Client CheckRedirect that refuses every
// redirect hop the secret may not follow, then applies next (nil: the
// default limit of 10 hops). A source that follows redirects with a bound
// secret uses it, so the secret never leaves its hosts.
func (b Bound) CheckRedirect(next func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if err := b.Allows(req.URL); err != nil {
			return err
		}
		if next != nil {
			return next(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
}
