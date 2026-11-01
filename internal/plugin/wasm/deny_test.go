package wasm_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/plugin/wasm"
	"github.com/angelia-dev/angelia/internal/source"
)

// AC: a request to a host not in allowed_hosts fails. Every case must fail
// before anything reaches the network.
func TestDenyHostNotInAllowedHosts(t *testing.T) {
	handlers := map[string]http.Handler{
		"allowed.test": okHandler("ok"),
		"denied.test":  okHandler("secret"),
		"other.test":   okHandler("secret"),
	}
	net, hc := newTestNet(t, handlers)
	s := newHostile(t, hostileManifest(t, "allowed.test"), wasm.Options{HTTPClient: hc})

	cases := map[string]string{
		"other host":            "http://denied.test/",
		"suffix of allowed":     "http://evilallowed.test/",
		"allowed as subdomain":  "http://allowed.test.denied.test/",
		"subdomain of allowed":  "http://www.allowed.test/",
		"allowed as userinfo":   "http://allowed.test@denied.test/",
		"ip of allowed host":    "http://203.0.113.10/",
		"loopback ipv6":         "http://[::1]/",
		"metadata ip":           "http://169.254.169.254/latest/meta-data/",
		"file scheme":           "file:///etc/passwd",
		"ftp scheme":            "ftp://allowed.test/",
		"no host":               "http:///path",
		"relative":              "/api/v1",
		"backslash trick":       "http://denied.test\\@allowed.test/",
		"percent-encoded colon": "http://denied.test%3a80@allowed.test/",
	}
	for name, u := range cases {
		t.Run(name, func(t *testing.T) {
			before := len(net.requests())
			_, err := s.List(t.Context(), hostileTarget("fetch", "url", u), source.FetchState{})
			if err == nil {
				t.Fatalf("List(%q) succeeded", u)
			}
			if got := net.requests()[before:]; len(got) != 0 {
				t.Fatalf("List(%q) reached the network: %v", u, got)
			}
		})
	}

	t.Run("error wraps ErrHostNotAllowed", func(t *testing.T) {
		_, err := s.List(t.Context(), hostileTarget("fetch", "url", "http://denied.test/x"), source.FetchState{})
		var hna *wasm.HostNotAllowedError
		if !errors.Is(err, wasm.ErrHostNotAllowed) || !errors.As(err, &hna) || hna.Host != "denied.test" {
			t.Fatalf("err = %v, want a HostNotAllowedError for denied.test", err)
		}
	})

	t.Run("plugin cannot swallow the denial", func(t *testing.T) {
		_, err := s.List(t.Context(), hostileTarget("fetch_ignore_error", "url", "http://denied.test/"), source.FetchState{})
		if !errors.Is(err, wasm.ErrHostNotAllowed) {
			t.Fatalf("err = %v, want ErrHostNotAllowed although the plugin ignored it", err)
		}
	})

	t.Run("plugin sees the host_not_allowed code", func(t *testing.T) {
		// The call fails, but the plugin got a structured error first.
		_, err := s.List(t.Context(), hostileTarget("fetch", "url", "http://denied.test/"), source.FetchState{})
		if err == nil || !strings.Contains(err.Error(), "host_not_allowed") {
			t.Fatalf("err = %v, want the plugin's message with code host_not_allowed", err)
		}
	})

	t.Run("allowed host passes, any case, port or trailing dot", func(t *testing.T) {
		for _, u := range []string{"http://allowed.test/", "https://ALLOWED.test:8443/x", "http://allowed.test./"} {
			res, err := s.List(t.Context(), hostileTarget("fetch", "url", u), source.FetchState{})
			if err != nil {
				t.Fatalf("List(%q): %v", u, err)
			}
			if res.Versions[0].Raw != "200" {
				t.Fatalf("List(%q) status = %s", u, res.Versions[0].Raw)
			}
		}
	})
}

// A redirect to a host that is not allowed fails, and the redirect target
// is never contacted.
func TestDenyRedirectToHostNotAllowed(t *testing.T) {
	cases := map[string]string{
		"other host":          "http://denied.test/steal",
		"metadata ip":         "http://169.254.169.254/latest/meta-data/",
		"loopback":            "http://127.0.0.1:8080/admin",
		"subdomain":           "http://www.allowed.test/",
		"scheme change":       "file:///etc/passwd",
		"protocol-relative":   "//denied.test/x",
		"userinfo to allowed": "http://allowed.test@denied.test/",
	}
	for name, loc := range cases {
		t.Run(name, func(t *testing.T) {
			var hits atomic.Int64
			net, hc := newTestNet(t, map[string]http.Handler{
				"allowed.test": redirectHandler(loc),
				"denied.test": http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
					hits.Add(1)
				}),
			})
			s := newHostile(t, hostileManifest(t, "allowed.test"), wasm.Options{HTTPClient: hc})
			_, err := s.List(t.Context(), hostileTarget("fetch_ignore_error", "url", "http://allowed.test/start"), source.FetchState{})
			if !errors.Is(err, wasm.ErrHostNotAllowed) {
				t.Fatalf("err = %v, want ErrHostNotAllowed", err)
			}
			if got := net.requests(); len(got) != 1 || hits.Load() != 0 {
				t.Fatalf("network saw %v (denied hits %d), want only the first hop", got, hits.Load())
			}
		})
	}

	t.Run("redirect between allowed hosts works", func(t *testing.T) {
		_, hc := newTestNet(t, map[string]http.Handler{
			"allowed.test": redirectHandler("http://other.test/final"),
			"other.test":   okHandler("done"),
		})
		s := newHostile(t, hostileManifest(t, "allowed.test", "other.test"), wasm.Options{HTTPClient: hc})
		res, err := s.List(t.Context(), hostileTarget("fetch", "url", "http://allowed.test/"), source.FetchState{})
		if err != nil {
			t.Fatal(err)
		}
		if res.Versions[0].Raw != "200" || res.Versions[0].Metadata["body"] != "done" {
			t.Fatalf("got %+v", res.Versions[0])
		}
	})
}

// SSRF through the host function: a host in allowed_hosts that resolves to
// a private address, or a private IP literal listed in allowed_hosts, is
// still refused by netguard, on the first hop and on redirects.
func TestDenySSRFThroughHostFunction(t *testing.T) {
	cases := []struct {
		name    string
		allowed []string
		url     string
		hops    int // requests that may reach the network (allowed public hops)
	}{
		{"name resolving to metadata ip", []string{"metadata.test"}, "http://metadata.test/latest/meta-data/", 0},
		{"name resolving to private ip", []string{"internal.test"}, "http://internal.test/", 0},
		{"name with one loopback address", []string{"rebind.test"}, "http://rebind.test/", 0},
		{"name resolving to ipv6 loopback", []string{"localhost.test"}, "http://localhost.test/", 0},
		{"private ip literal allowed", []string{"10.0.0.1"}, "http://10.0.0.1/", 0},
		{"loopback literal allowed", []string{"127.0.0.1"}, "http://127.0.0.1:9000/", 0},
		{"metadata literal allowed", []string{"169.254.169.254"}, "http://169.254.169.254/", 0},
		{"cgnat literal allowed", []string{"100.100.100.200"}, "http://100.100.100.200/", 0},
		{"mapped ipv6 allowed", []string{"::ffff:127.0.0.1"}, "http://[::ffff:127.0.0.1]/", 0},
		{"redirect to allowed private name", []string{"allowed.test", "internal.test"}, "http://allowed.test/", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			net, hc := newTestNet(t, map[string]http.Handler{
				"allowed.test":  redirectHandler("http://internal.test/admin"),
				"internal.test": okHandler("internal"),
			})
			s := newHostile(t, hostileManifest(t, c.allowed...), wasm.Options{HTTPClient: hc})
			_, err := s.List(t.Context(), hostileTarget("fetch_ignore_error", "url", c.url), source.FetchState{})
			if !errors.Is(err, netguard.ErrBlocked) {
				t.Fatalf("err = %v, want netguard.ErrBlocked", err)
			}
			if got := net.requests(); len(got) != c.hops {
				t.Fatalf("network saw %v, want %d request(s)", got, c.hops)
			}
		})
	}

	t.Run("plugin sees the blocked code", func(t *testing.T) {
		_, hc := newTestNet(t, nil)
		s := newHostile(t, hostileManifest(t, "internal.test"), wasm.Options{HTTPClient: hc})
		res, err := s.List(t.Context(), hostileTarget("fetch_raw", "url", "http://internal.test/"), source.FetchState{})
		if !errors.Is(err, netguard.ErrBlocked) {
			t.Fatalf("err = %v, res = %+v", err, res)
		}
	})
}

// Extism's built-in http_request uses http.DefaultClient and would bypass
// netguard: the host must keep it denied for every host.
func TestDenyExtismBuiltinHTTP(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	_, hc := newTestNet(t, nil)
	// Even with the server's host in allowed_hosts.
	s := newHostile(t, hostileManifest(t, u.Hostname()), wasm.Options{HTTPClient: hc})
	_, err := s.List(t.Context(), hostileTarget("builtin_http", "url", srv.URL), source.FetchState{})
	if err == nil {
		t.Fatal("Extism's built-in http_request succeeded")
	}
	if hits.Load() != 0 {
		t.Fatalf("the built-in http_request reached the server %d time(s)", hits.Load())
	}
}

func TestDenyNetworkDuringValidate(t *testing.T) {
	net, hc := newTestNet(t, map[string]http.Handler{"allowed.test": okHandler("ok")})
	s := newHostile(t, hostileManifest(t, "allowed.test"), wasm.Options{HTTPClient: hc})
	err := s.Validate(hostileTarget("validate_fetch", "url", "http://allowed.test/"))
	if !errors.Is(err, wasm.ErrPolicy) || errors.Is(err, source.ErrInvalidTarget) {
		t.Fatalf("err = %v, want ErrPolicy and not ErrInvalidTarget", err)
	}
	if got := net.requests(); len(got) != 0 {
		t.Fatalf("Validate reached the network: %v", got)
	}
}

func TestDenyRequestBudget(t *testing.T) {
	net, hc := newTestNet(t, map[string]http.Handler{"allowed.test": okHandler("ok")})
	s := newHostile(t, hostileManifest(t, "allowed.test"), wasm.Options{HTTPClient: hc, MaxRequestsPerCall: 5})
	_, err := s.List(t.Context(), hostileTarget("many_requests", "url", "http://allowed.test/"), source.FetchState{})
	if !errors.Is(err, wasm.ErrPolicy) {
		t.Fatalf("err = %v, want ErrPolicy", err)
	}
	if got := len(net.requests()); got != 5 {
		t.Fatalf("network saw %d requests, want 5", got)
	}
	// The budget is per call.
	if _, err := s.List(t.Context(), hostileTarget("fetch", "url", "http://allowed.test/"), source.FetchState{}); err != nil {
		t.Fatalf("next call: %v", err)
	}
}

func TestDenyBadRequests(t *testing.T) {
	net, hc := newTestNet(t, map[string]http.Handler{"allowed.test": okHandler("ok")})
	s := newHostile(t, hostileManifest(t, "allowed.test"), wasm.Options{HTTPClient: hc})
	cases := map[string]source.Target{
		"method DELETE":   hostileTarget("fetch", "url", "http://allowed.test/", "method", "DELETE"),
		"method CONNECT":  hostileTarget("fetch", "url", "http://allowed.test/", "method", "CONNECT"),
		"credentials":     hostileTarget("fetch", "url", "http://user:pass@allowed.test/"),
		"bad url":         hostileTarget("fetch", "url", "http://allowed.test/%zz"),
		"bad body base64": hostileTarget("fetch_raw", "url", "http://allowed.test/", "method", "POST", "raw_body", "%%%"),
		"bad header":      hostileTarget("fetch", "url", "http://allowed.test/", "authorization", "a\nb"),
	}
	for name, tg := range cases {
		t.Run(name, func(t *testing.T) {
			res, err := s.List(t.Context(), tg, source.FetchState{})
			if name == "bad body base64" {
				// fetch_raw returns the code instead of failing.
				if err != nil || res.Versions[0].Metadata["code"] != "bad_request" {
					t.Fatalf("res = %+v, err = %v, want code bad_request", res, err)
				}
			} else if err == nil {
				t.Fatal("List succeeded")
			}
		})
	}
	for _, r := range net.requests() {
		t.Errorf("a bad request reached the network: %s", r)
	}
}
