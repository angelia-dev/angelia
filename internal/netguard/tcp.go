package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// tcpDialTimeout bounds the connect of DialContext when the caller's
// context has no earlier deadline.
const tcpDialTimeout = 30 * time.Second

// DialContext opens a TCP connection to address ("host:port") for a sink
// that does not speak HTTP, such as an MQTT broker (ADR 0039). It applies
// the same rules as an HTTP hop:
//
//   - network must be "tcp", "tcp4" or "tcp6"; anything else is refused
//     with an error wrapping ErrInvalidURL, as is an address that is not
//     host:port or whose host is an IP address in a non-canonical form
//     (NonCanonicalIP);
//   - it waits on the host's anonymous rate limit bucket, so
//     http.host_limits applies to the host by name;
//   - it checks the host first, as CheckURL does: a hostname is resolved
//     with Config.Resolver and every address must be allowed;
//   - it then dials with the dial-time check of the HTTP transport: the
//     address the socket actually connects to, after the dialer's own DNS
//     lookup, must be allowed, which closes the DNS rebinding gap.
//
// It never goes through a proxy (HTTPS_PROXY does not apply to raw TCP),
// and the proxy exemption of the HTTP dialer does not apply: a configured
// proxy address is checked like any other. A refusal is an error wrapping
// a *BlockedError and ErrBlocked whose message names the host, never the
// address it resolved to. The caller owns the returned connection.
func (c *Client) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, invalidURL("netguard: dial: network %q not allowed", network)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" || port == "" {
		return nil, invalidURL("netguard: dial: the address must be host:port")
	}
	if NonCanonicalIP(host) {
		return nil, invalidURL("netguard: dial: host %q is not a canonical IP address", host)
	}
	key := normHost(host)

	waitStart := c.clock.Now()
	if err := c.limiter.wait(ctx, key, false); err != nil {
		if c.cfg.Observer != nil && errors.Is(err, errSlotPastDeadline) {
			c.cfg.Observer.DeadlineRefusal(c.limiter.observedHost(key))
		}
		return nil, err
	}
	if c.cfg.Observer != nil {
		c.cfg.Observer.RateLimitWait(c.limiter.observedHost(key), max(0, c.clock.Now().Sub(waitStart)))
	}

	if err := c.guard.checkHost(ctx, host); err != nil {
		return nil, dialRefusal(host, err)
	}
	d := net.Dialer{Timeout: tcpDialTimeout, KeepAlive: 30 * time.Second}
	d.ControlContext = func(_ context.Context, _, addr string, _ syscall.RawConn) error {
		return c.guard.checkDial(host, addr)
	}
	conn, err := d.DialContext(ctx, network, address)
	if err != nil {
		return nil, dialRefusal(host, err)
	}
	return conn, nil
}

// dialRefusal rewrites a guard refusal so that its message names the host
// only; any other error is returned as it is.
func dialRefusal(host string, err error) error {
	if !errors.Is(err, ErrBlocked) {
		return err
	}
	return &dialBlockedError{host: host, err: err}
}

// dialBlockedError is a TCP destination the guard refused. Error names the
// host, never the resolved address; Unwrap keeps the *BlockedError (and so
// ErrBlocked) in the chain.
type dialBlockedError struct {
	host string
	err  error
}

func (e *dialBlockedError) Error() string {
	return fmt.Sprintf("netguard: dial %s: %v", e.host, ErrBlocked)
}
func (e *dialBlockedError) Unwrap() error { return e.err }

// checkHost is the host part of CheckURL: an IP literal is checked as it
// is, a hostname is resolved and every address it returns must pass. A
// non-canonical IPv4 form is refused with ErrInvalidURL; a lookup failure
// is an error that does not match it.
func (g *Guard) checkHost(ctx context.Context, host string) error {
	if addr, err := netip.ParseAddr(host); err == nil {
		return g.CheckAddr(addr.String(), addr)
	}
	if isNumericHost(host) {
		// Decimal, octal, hex or short IPv4 forms such as 2130706433,
		// 0x7f000001 or 127.1: some resolvers read them as addresses.
		return invalidURL("netguard: host %q is not a canonical IP address", host)
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
