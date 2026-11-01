package sourcetest

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Client returns an *http.Client that can only talk to srv. Any request whose
// URL.Host differs from the server's listener address fails the test with a
// clear message and is not sent. As defence in depth the underlying dialer
// refuses to connect to any other address, so even a redirect or a custom
// resolver cannot escape. Requests are counted; see Requests.
//
// Sources receive their client by injection, so pass this one to the
// constructor under test. Nothing global is touched, so tests may run in
// parallel.
func Client(tb testing.TB, srv *httptest.Server) *http.Client {
	tb.Helper()
	allowed := srv.Listener.Addr().String()
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: dialControl(allowed)}
	base := &http.Transport{DialContext: dialer.DialContext, Proxy: nil}
	tb.Cleanup(base.CloseIdleConnections)
	return &http.Client{Transport: &guardTransport{tb: tb, allowed: allowed, base: base}}
}

// Requests returns the number of requests c has sent through the guard, or -1
// if c was not made by Client.
func Requests(c *http.Client) int {
	g, ok := c.Transport.(*guardTransport)
	if !ok {
		return -1
	}
	return int(g.n.Load())
}

type guardTransport struct {
	tb      testing.TB
	allowed string
	base    http.RoundTripper
	n       atomic.Int64
}

func (g *guardTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != g.allowed {
		err := fmt.Errorf("sourcetest: request to %q blocked: only the fake server %q is reachable", r.URL.Host, g.allowed)
		g.tb.Errorf("%v", err)
		return nil, err
	}
	g.n.Add(1)
	return g.base.RoundTrip(r)
}

// dialControl returns a net.Dialer Control function that rejects every
// connection except to allowed ("ip:port").
func dialControl(allowed string) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		if address != allowed {
			return fmt.Errorf("sourcetest: dial %q blocked: only %q is reachable", address, allowed)
		}
		return nil
	}
}
