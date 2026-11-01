package netguard

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

// ErrBlocked is wrapped by every error for a destination the guard refuses.
var ErrBlocked = errors.New("destination address not allowed")

// BlockedError reports the address that was refused.
type BlockedError struct {
	Host string
	Addr netip.Addr
}

func (e *BlockedError) Error() string {
	if e.Host == e.Addr.String() {
		return fmt.Sprintf("netguard: %s: %v", e.Addr, ErrBlocked)
	}
	return fmt.Sprintf("netguard: %s resolves to %s: %v", e.Host, e.Addr, ErrBlocked)
}

// Unwrap lets errors.Is match ErrBlocked.
func (e *BlockedError) Unwrap() error { return ErrBlocked }

// Resolver looks up the IP addresses of a host. *net.Resolver satisfies it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Guard is the basic SSRF guard. By default it refuses loopback
// (127.0.0.0/8, ::1), private (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16,
// fc00::/7), link-local (169.254.0.0/16, fe80::/10) and unspecified
// (0.0.0.0, ::) addresses. IPv4-mapped IPv6 addresses are checked as IPv4.
// An address inside one of the allowlisted prefixes is accepted.
//
// The guard is not proof against DNS rebinding; see the package doc.
type Guard struct {
	allow    []netip.Prefix
	resolver Resolver
}

// NewGuard returns a guard that accepts the allowlisted prefixes and uses
// resolver to look up hostnames.
func NewGuard(allow []netip.Prefix, resolver Resolver) *Guard {
	a := make([]netip.Prefix, len(allow))
	for i, p := range allow {
		a[i] = p.Masked()
	}
	return &Guard{allow: a, resolver: resolver}
}

// CheckAddr returns a *BlockedError if addr is refused.
func (g *Guard) CheckAddr(host string, addr netip.Addr) error {
	a := addr.Unmap().WithZone("")
	for _, p := range g.allow {
		if p.Contains(a) {
			return nil
		}
	}
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsUnspecified() {
		return &BlockedError{Host: host, Addr: a}
	}
	return nil
}

// CheckURL checks the scheme and host of u. A hostname is resolved and
// every address it returns must pass; a lookup failure is an error.
func (g *Guard) CheckURL(ctx context.Context, u *url.URL) error {
	if s := strings.ToLower(u.Scheme); s != "http" && s != "https" {
		return fmt.Errorf("netguard: scheme %q not allowed", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("netguard: URL %q has no host", u.Redacted())
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return g.CheckAddr(addr.String(), addr)
	}
	if isNumericHost(host) {
		// Decimal, octal, hex or short IPv4 forms such as 2130706433,
		// 0x7f000001 or 127.1: some resolvers read them as addresses.
		return fmt.Errorf("netguard: host %q is not a canonical IP address", host)
	}
	addrs, err := g.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("netguard: resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("netguard: resolve %s: no addresses", host)
	}
	for _, a := range addrs {
		if err := g.CheckAddr(host, a); err != nil {
			return err
		}
	}
	return nil
}

// isNumericHost reports whether the last label of host is all digits or a
// 0x hex number. No real top-level domain looks like that, so such a host
// is a non-canonical IPv4 literal.
func isNumericHost(host string) bool {
	host = strings.TrimSuffix(host, ".")
	label := host[strings.LastIndexByte(host, '.')+1:]
	if label == "" {
		return false
	}
	digits := "0123456789"
	if l := strings.ToLower(label); strings.HasPrefix(l, "0x") {
		label, digits = l[2:], "0123456789abcdef"
	}
	return strings.Trim(label, digits) == ""
}
