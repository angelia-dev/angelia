package netguard

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
)

// noProxy keeps a test independent of the *_PROXY variables of the machine
// running it.
func noProxy(*http.Request) (*url.URL, error) { return nil, nil }

// dialClient is a netguard client on the default transport (with its
// dial-time check), a stub resolver for the URL check, and proxy as the
// proxy function.
func dialClient(t *testing.T, resolver Resolver, allow []netip.Prefix, proxy func(*http.Request) (*url.URL, error)) *http.Client {
	t.Helper()
	c, err := New(Config{Resolver: resolver, Allow: allow, Proxy: proxy, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.transport.(*http.Transport).CloseIdleConnections)
	return c.HTTPClient()
}

// needLocalhost skips a test when the system resolver does not answer
// "localhost" with loopback addresses only: the rebinding tests use it as
// the name whose dial-time answer differs from the stub's.
func needLocalhost(t *testing.T) {
	t.Helper()
	as, err := net.DefaultResolver.LookupNetIP(t.Context(), "ip", "localhost")
	if err != nil || len(as) == 0 {
		t.Skipf("localhost does not resolve here: %v", err)
	}
	for _, a := range as {
		if !a.Unmap().IsLoopback() {
			t.Skipf("localhost resolves to %s, not loopback", a)
		}
	}
}

func countingServer(t *testing.T, body string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func port(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	_, p, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// AC (#108, ADR 0023 section 5, test a): DNS rebinding. The URL check's
// resolver answers a public address, the dialer's own lookup of "localhost"
// answers 127.0.0.1, where a server is listening: the dial is refused.
func TestDialRefusesRebindingToLoopback(t *testing.T) {
	needLocalhost(t)
	srv, hits := countingServer(t, "reached")
	hc := dialClient(t, fakeResolver{"localhost": addrs("203.0.113.10")}, nil, noProxy)
	resp, err := hc.Get("http://localhost:" + port(t, srv) + "/")
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("got %v, want ErrBlocked from the dial-time check", err)
	}
	var be *BlockedError
	if !errors.As(err, &be) || !be.Addr.IsLoopback() || be.Host != "localhost" {
		t.Fatalf("BlockedError = %+v, want localhost refused at a loopback address", be)
	}
	if hits.Load() != 0 {
		t.Fatalf("server reached %d times", hits.Load())
	}
}

// An allowlisted CIDR is accepted by the URL check and at dial time.
func TestDialAllowlistedCIDR(t *testing.T) {
	needLocalhost(t)
	srv, hits := countingServer(t, "reached")
	allow := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	hc := dialClient(t, fakeResolver{"localhost": addrs("127.0.0.1")}, allow, noProxy)
	resp, err := hc.Get("http://localhost:" + port(t, srv) + "/")
	if err != nil {
		t.Fatalf("allowlisted loopback: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "reached" || hits.Load() != 1 {
		t.Fatalf("body %q, hits %d", b, hits.Load())
	}
}

func TestDialControlFailsClosed(t *testing.T) {
	g := NewGuard(nil, fakeResolver{})
	if err := g.checkDial("h", "not-an-address"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("unparsable address: got %v, want ErrBlocked", err)
	}
	if err := g.checkDial("h", "127.0.0.1:80"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("loopback: got %v, want ErrBlocked", err)
	}
	if err := g.checkDial("h", "[fe80::1%eth0]:80"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("zoned link-local: got %v, want ErrBlocked", err)
	}
	if err := g.checkDial("h", "203.0.113.10:443"); err != nil {
		t.Fatalf("public: %v", err)
	}
	d := &dialer{guard: g, proxies: map[string]bool{}}
	if _, err := d.DialContext(t.Context(), "tcp", "no-port"); err == nil {
		t.Fatal("address without a port: want an error")
	}
}

// proxyServer is an HTTP forward proxy on loopback that answers every
// absolute-form request itself and records the target host.
func proxyServer(t *testing.T) (*httptest.Server, *atomic.Value) {
	t.Helper()
	var target atomic.Value
	target.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target.Store(r.URL.Host)
		_, _ = io.WriteString(w, "via proxy")
	}))
	t.Cleanup(srv.Close)
	return srv, &target
}

// AC (#108, ADR 0023 section 5, test b): the proxy, on loopback, is exempt
// from the dial-time check; a public-hostname target reaches it, and a
// private-literal target is still refused by the URL check before any dial.
func TestDialProxyExempt(t *testing.T) {
	proxy, target := proxyServer(t)
	pu, _ := url.Parse(proxy.URL)
	hc := dialClient(t, fakeResolver{"hooks.example.test": addrs("203.0.113.10")}, nil, http.ProxyURL(pu))
	assertProxied(t, hc, target)
}

func assertProxied(t *testing.T, hc *http.Client, target *atomic.Value) {
	t.Helper()
	resp, err := hc.Get("http://hooks.example.test/hook")
	if err != nil {
		t.Fatalf("through the proxy: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "via proxy" || target.Load() != "hooks.example.test" {
		t.Fatalf("body %q, proxy saw host %q", b, target.Load())
	}
	target.Store("")
	resp, err = hc.Get("http://10.0.0.1/hook")
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("private literal through the proxy: got %v, want ErrBlocked", err)
	}
	if target.Load() != "" {
		t.Fatalf("private literal reached the proxy (%q)", target.Load())
	}
}

// The same as TestDialProxyExempt with the proxy taken from HTTP_PROXY, as in
// production. net/http reads the proxy variables once per process, so the
// check runs in a child process with a controlled environment.
func TestDialProxyFromEnvironment(t *testing.T) {
	if os.Getenv("NETGUARD_PROXY_CHILD") == "1" {
		// The parent's proxy records the target; the child checks the
		// client's view: "via proxy" proves the proxy answered.
		hc := dialClient(t, fakeResolver{"hooks.example.test": addrs("203.0.113.10")}, nil, nil)
		resp, err := hc.Get("http://hooks.example.test/hook")
		if err != nil {
			t.Fatalf("through the proxy: %v", err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(b) != "via proxy" {
			t.Fatalf("body %q", b)
		}
		resp, err = hc.Get("http://10.0.0.1/hook")
		if resp != nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, ErrBlocked) {
			t.Fatalf("private literal: got %v, want ErrBlocked", err)
		}
		return
	}
	proxy, target := proxyServer(t)
	env := []string{"NETGUARD_PROXY_CHILD=1", "HTTP_PROXY=" + proxy.URL, "NO_PROXY="}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasSuffix(strings.ToUpper(k), "_PROXY") || k == "REQUEST_METHOD" {
			continue
		}
		env = append(env, kv)
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestDialProxyFromEnvironment$", "-test.count=1") //nolint:gosec // G204: re-runs this test binary.
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	if target.Load() != "hooks.example.test" {
		t.Fatalf("proxy saw host %q, want hooks.example.test", target.Load())
	}
}

func TestProxyAddrs(t *testing.T) {
	fixed := func(raw string) func(*http.Request) (*url.URL, error) {
		return func(*http.Request) (*url.URL, error) { return url.Parse(raw) }
	}
	cases := map[string]string{
		"http://Proxy.Corp:3128":   "proxy.corp:3128",
		"http://10.1.2.3":          "10.1.2.3:80",
		"https://proxy.corp":       "proxy.corp:443",
		"socks5://127.0.0.1":       "127.0.0.1:1080",
		"http://[fd00::1]:8080/":   "[fd00::1]:8080",
		"socks5h://user:pw@sock:9": "sock:9",
	}
	for raw, want := range cases {
		got := proxyAddrs(fixed(raw))
		if len(got) != 1 || !got[want] {
			t.Errorf("proxyAddrs(%s) = %v, want %s", raw, got, want)
		}
	}
	if got := proxyAddrs(noProxy); len(got) != 0 {
		t.Errorf("no proxy: %v", got)
	}
	if got := proxyAddrs(func(*http.Request) (*url.URL, error) { return nil, errors.New("bad") }); len(got) != 0 {
		t.Errorf("proxy error: %v", got)
	}
	// Only the proxy's address is exempt, not other loopback ports.
	d := &dialer{guard: NewGuard(nil, fakeResolver{}), proxies: proxyAddrs(fixed("http://127.0.0.1:1"))}
	if _, err := d.DialContext(t.Context(), "tcp", "127.0.0.1:2"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("non-proxy loopback port: got %v, want ErrBlocked", err)
	}
}
