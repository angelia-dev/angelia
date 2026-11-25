package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var loopbackBoth = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}

// countingListener accepts connections on 127.0.0.1 and counts them.
type countingListener struct {
	net.Listener
	n    atomic.Int64
	done chan struct{}
}

func (c *countingListener) Load() int64 { return c.n.Load() }

// closedCount closes the listener, waits until its accept loop has
// returned, and gives the number of connections accepted.
func (c *countingListener) closedCount() int64 {
	_ = c.Close()
	<-c.done
	return c.n.Load()
}

// tcpListener accepts connections on 127.0.0.1 and counts them.
func tcpListener(t *testing.T) (net.Listener, *countingListener) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	cl := &countingListener{Listener: ln, done: make(chan struct{})}
	n := &cl.n
	go func() {
		defer close(cl.done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			_ = c.Close()
		}
	}()
	return ln, cl
}

func tcpClient(t *testing.T, mutate func(*Config)) *Client {
	t.Helper()
	cfg := Config{Resolver: fakeResolver{}, Proxy: noProxy}
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func lnPort(t *testing.T, ln net.Listener) string {
	t.Helper()
	_, p, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func assertDialBlocked(t *testing.T, err error, host string, hidden ...string) {
	t.Helper()
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("got %v, want ErrBlocked", err)
	}
	var be *BlockedError
	if !errors.As(err, &be) {
		t.Fatalf("%v does not wrap a *BlockedError", err)
	}
	if !strings.Contains(err.Error(), host) {
		t.Fatalf("error %q does not name the host %s", err, host)
	}
	for _, h := range hidden {
		if strings.Contains(err.Error(), h) {
			t.Fatalf("error %q reveals the address %s", err, h)
		}
	}
}

// AC (#93, 93-B): loopback is refused by default and allowed with
// http.allow_networks.
func TestDialContextLoopback(t *testing.T) {
	ln, hits := tcpListener(t)
	conn, err := tcpClient(t, nil).DialContext(t.Context(), "tcp", ln.Addr().String())
	if conn != nil {
		_ = conn.Close()
	}
	assertDialBlocked(t, err, "127.0.0.1")

	c := tcpClient(t, func(cfg *Config) { cfg.Allow = loopbackBoth })
	conn, err = c.DialContext(t.Context(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("allowlisted loopback: %v", err)
	}
	_ = conn.Close()
	waitUntil(t, "accept", func() bool { return hits.Load() == 1 })
}

// A hostname that resolves to a private address is refused by the name
// check, and the error names the host, not the address.
func TestDialContextPrivateName(t *testing.T) {
	c := tcpClient(t, func(cfg *Config) {
		cfg.Resolver = fakeResolver{"broker.example.test": addrs("203.0.113.10", "10.1.2.3")}
	})
	_, err := c.DialContext(t.Context(), "tcp", "broker.example.test:8883")
	assertDialBlocked(t, err, "broker.example.test", "10.1.2.3")
	if _, err := c.DialContext(t.Context(), "tcp", "unknown.example.test:8883"); err == nil || errors.Is(err, ErrBlocked) || errors.Is(err, ErrInvalidURL) {
		t.Fatalf("lookup failure: got %v, want a plain resolve error", err)
	}
}

// AC: DNS rebinding. The name check's resolver answers a public address,
// the dialer's own lookup of "localhost" lands on loopback: refused at
// dial time, and the server is never reached.
func TestDialContextRefusesRebinding(t *testing.T) {
	needLocalhost(t)
	ln, hits := tcpListener(t)
	c := tcpClient(t, func(cfg *Config) { cfg.Resolver = fakeResolver{"localhost": addrs("203.0.113.10")} })
	_, err := c.DialContext(t.Context(), "tcp", "localhost:"+lnPort(t, ln))
	assertDialBlocked(t, err, "localhost", "127.0.0.1", "::1")
	if n := hits.closedCount(); n != 0 {
		t.Fatalf("server reached %d times", n)
	}
}

// AC: a configured HTTP proxy address is not exempt, by literal or by
// name (the name passes the check, the dial is refused).
func TestDialContextProxyNotExempt(t *testing.T) {
	needLocalhost(t)
	ln, hits := tcpListener(t)
	port := lnPort(t, ln)
	for _, raw := range []string{"http://127.0.0.1:" + port, "http://localhost:" + port} {
		pu, _ := url.Parse(raw)
		c := tcpClient(t, func(cfg *Config) {
			cfg.Proxy = http.ProxyURL(pu)
			cfg.Resolver = fakeResolver{"localhost": addrs("203.0.113.10")}
		})
		_, err := c.DialContext(t.Context(), "tcp", pu.Host)
		if !errors.Is(err, ErrBlocked) {
			t.Fatalf("proxy address %s: got %v, want ErrBlocked", pu.Host, err)
		}
	}
	if n := hits.closedCount(); n != 0 {
		t.Fatalf("proxy reached %d times", n)
	}
}

// AC: only TCP, a host:port address and a canonical IP literal.
func TestDialContextInvalid(t *testing.T) {
	c := tcpClient(t, func(cfg *Config) { cfg.Allow = loopbackBoth })
	for _, tc := range []struct{ network, addr string }{
		{"udp", "127.0.0.1:1883"},
		{"unix", "/tmp/sock"},
		{"ip4:icmp", "127.0.0.1"},
		{"tcp", "no-port"},
		{"tcp", ":1883"},
		{"tcp", "127.1:1883"},
		{"tcp", "2130706433:1883"},
		{"tcp", "0x7f000001:1883"},
	} {
		if _, err := c.DialContext(t.Context(), tc.network, tc.addr); !errors.Is(err, ErrInvalidURL) {
			t.Errorf("DialContext(%s, %s) = %v, want ErrInvalidURL", tc.network, tc.addr, err)
		}
	}
}

// AC: the host's limiter bucket is honoured (fake clock), and a context
// ended during the wait returns its error without dialling.
func TestDialContextRateLimit(t *testing.T) {
	ln, hits := tcpListener(t)
	clock := newFakeClock(false)
	one := Limit{Rate: 1, Burst: 1}
	c := tcpClient(t, func(cfg *Config) {
		cfg.Allow = loopbackBoth
		cfg.Clock = clock
		cfg.FallbackLimit = HostLimit{Anonymous: one, Authenticated: one}
	})
	dial := func(ctx context.Context) error {
		conn, err := c.DialContext(ctx, "tcp", ln.Addr().String())
		if conn != nil {
			_ = conn.Close()
		}
		return err
	}
	if err := dial(t.Context()); err != nil {
		t.Fatalf("first dial: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- dial(t.Context()) }()
	waitUntil(t, "second dial waiting", func() bool { return clock.numSleepers() == 1 })
	select {
	case err := <-done:
		t.Fatalf("second dial did not wait: %v", err)
	default:
	}
	clock.Advance(time.Second)
	if err := <-done; err != nil {
		t.Fatalf("second dial: %v", err)
	}
	waitUntil(t, "two accepts", func() bool { return hits.Load() == 2 })

	ctx, cancel := context.WithCancel(t.Context())
	go func() { done <- dial(ctx) }()
	waitUntil(t, "third dial waiting", func() bool { return clock.numSleepers() == 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait: got %v, want context.Canceled", err)
	}
	if n := hits.closedCount(); n != 2 {
		t.Fatalf("a cancelled dial reached the server (%d accepts)", n)
	}
}

func TestDialBlockedErrorPassesOthers(t *testing.T) {
	other := errors.New("boom")
	if got := dialRefusal("h", other); !errors.Is(got, other) || errors.Is(got, ErrBlocked) {
		t.Fatalf("dialRefusal changed a non-guard error: %v", got)
	}
}
