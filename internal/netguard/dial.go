package netguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
)

// proxyProbeHost is the host of the URLs New hands to Config.Proxy to learn
// the proxy addresses. The .invalid top-level domain never resolves.
const proxyProbeHost = "angelia-proxy-probe.invalid"

// dialer is the default transport's dialer. A direct connection is checked
// by the guard on the address actually connected, after the dialer's own
// DNS lookup, which closes the DNS rebinding gap between the URL check and
// the dial. A connection to a configured proxy is not checked: proxies
// usually have private addresses and resolve the target themselves, so
// through a proxy only the URL check applies.
type dialer struct {
	base  net.Dialer
	guard *Guard
	// proxies holds the lower-case host:port of each configured proxy.
	// Read-only after newTransport.
	proxies map[string]bool
}

// DialContext dials addr ("host:port"), refusing a direct connection to an
// address the guard does not allow with an error wrapping a *BlockedError.
func (d *dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if d.proxies[strings.ToLower(addr)] {
		return d.base.DialContext(ctx, network, addr)
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("netguard: dial %q: %w", addr, err)
	}
	nd := d.base // a copy: ControlContext is per dial, to name the host
	nd.ControlContext = func(_ context.Context, _, address string, _ syscall.RawConn) error {
		return d.guard.checkDial(host, address)
	}
	return nd.DialContext(ctx, network, addr)
}

// checkDial checks the "ip:port" a socket is about to connect to. An
// address that does not parse is refused: the check fails closed.
func (g *Guard) checkDial(host, address string) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("netguard: dial %s: unparsable address: %w", host, ErrBlocked)
	}
	return g.CheckAddr(host, ap.Addr())
}

// proxyAddrs returns the host:port the transport dials for each proxy that
// proxy picks for an http and an https probe URL.
func proxyAddrs(proxy func(*http.Request) (*url.URL, error)) map[string]bool {
	out := map[string]bool{}
	for _, scheme := range []string{"http", "https"} {
		u, err := proxy(&http.Request{Method: http.MethodGet, URL: &url.URL{Scheme: scheme, Host: proxyProbeHost}, Header: http.Header{}})
		if err != nil || u == nil || u.Hostname() == "" {
			continue
		}
		port := u.Port()
		if port == "" {
			switch strings.ToLower(u.Scheme) {
			case "https":
				port = "443"
			case "socks5", "socks5h":
				port = "1080"
			default:
				port = "80"
			}
		}
		out[strings.ToLower(net.JoinHostPort(u.Hostname(), port))] = true
	}
	return out
}
