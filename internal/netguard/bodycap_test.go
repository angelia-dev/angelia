package netguard

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

var anyLimit = Limit{Rate: 1000, Burst: 1000}

func hostLimit(maxBody int64) HostLimit {
	return HostLimit{Authenticated: anyLimit, Anonymous: anyLimit, MaxBodyBytes: maxBody}
}

func bigBodyServer(t *testing.T, size int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("a"), size))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func readAll(t *testing.T, c *Client, rawURL string) ([]byte, error) {
	t.Helper()
	resp, err := c.HTTPClient().Do(get(t, rawURL))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(resp.Body)
}

func mustHost(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname()
}

// AC: a host's MaxBodyBytes overrides the global cap, up and down.
func TestHostBodyCapOverridesGlobal(t *testing.T) {
	const size = 2 << 10
	srv := bigBodyServer(t, size)
	host := mustHost(t, srv.URL)

	raised, _ := newTestClient(t, func(c *Config) {
		c.MaxBodyBytes = 1 << 10
		c.HostLimits = map[string]HostLimit{host: hostLimit(4 << 10)}
	})
	if body, err := readAll(t, raised, srv.URL); err != nil || len(body) != size {
		t.Fatalf("under host cap: got %d bytes, err %v; want %d, nil", len(body), err, size)
	}

	lowered, _ := newTestClient(t, func(c *Config) {
		c.MaxBodyBytes = 1 << 20
		c.HostLimits = map[string]HostLimit{host: hostLimit(1 << 10)}
	})
	body, err := readAll(t, lowered, srv.URL)
	if !errors.Is(err, ErrBodyTooLarge) || len(body) != 1<<10 {
		t.Fatalf("over host cap: got %d bytes, err %v; want 1024, ErrBodyTooLarge", len(body), err)
	}
	if _, err := Fetch(t.Context(), lowered.HTTPClient(), get(t, srv.URL), Validators{}); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("Fetch over host cap: got %v, want ErrBodyTooLarge", err)
	}
}

// AC: zero means the global cap, and hosts without an entry keep it.
func TestHostBodyCapZeroAndOtherHostsUseGlobal(t *testing.T) {
	srv := bigBodyServer(t, 2<<10)
	host := mustHost(t, srv.URL)
	for name, limits := range map[string]map[string]HostLimit{
		"zero in entry":   {host: hostLimit(0)},
		"other host only": {"registry.example.test": hostLimit(1 << 20)},
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := newTestClient(t, func(c *Config) {
				c.MaxBodyBytes = 1 << 10
				c.HostLimits = limits
			})
			if _, err := readAll(t, c, srv.URL); !errors.Is(err, ErrBodyTooLarge) {
				t.Fatalf("got %v, want ErrBodyTooLarge at the global cap", err)
			}
		})
	}
}

// AC: the cap applies to the decompressed body, not the wire size.
func TestHostBodyCapAppliesToDecompressedBody(t *testing.T) {
	const size = 1 << 20
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		_, _ = zw.Write(bytes.Repeat([]byte("a"), size)) // gzips to about 1 KiB
		_ = zw.Close()
	}))
	defer srv.Close()
	host := mustHost(t, srv.URL)

	over, _ := newTestClient(t, func(c *Config) {
		c.HostLimits = map[string]HostLimit{host: hostLimit(size / 2)}
	})
	if _, err := readAll(t, over, srv.URL); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("gzip body over cap: got %v, want ErrBodyTooLarge", err)
	}
	under, _ := newTestClient(t, func(c *Config) {
		c.HostLimits = map[string]HostLimit{host: hostLimit(2 * size)}
	})
	if body, err := readAll(t, under, srv.URL); err != nil || len(body) != size {
		t.Fatalf("gzip body under cap: got %d bytes, err %v", len(body), err)
	}
}

// AC: a redirect does not carry the cap of one host to the other; the host
// serving the body decides.
func TestHostBodyCapFollowsServingHostAcrossRedirect(t *testing.T) {
	const size = 2 << 10
	const bigHost, smallHost = "big.test", "small.test"
	serve := func(r *http.Request) *http.Response {
		resp := &http.Response{Header: http.Header{}, Request: r, StatusCode: http.StatusOK}
		switch r.URL.Path {
		case "/redirect-to-small":
			resp.StatusCode = http.StatusFound
			resp.Header.Set("Location", "https://"+smallHost+"/doc")
			resp.Body = http.NoBody
		case "/redirect-to-big":
			resp.StatusCode = http.StatusFound
			resp.Header.Set("Location", "https://"+bigHost+"/doc")
			resp.Body = http.NoBody
		default:
			resp.Body = io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("a"), size)))
		}
		return resp
	}
	newC := func() *Client {
		c, _ := newTestClient(t, func(c *Config) {
			c.MaxBodyBytes = 1 << 10
			c.Resolver = fakeResolver{bigHost: addrs("203.0.113.1"), smallHost: addrs("203.0.113.2")}
			c.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) { return serve(r), nil })
			c.HostLimits = map[string]HostLimit{bigHost: hostLimit(1 << 20)}
		})
		return c
	}

	// big -> small: the large cap must not follow the redirect.
	if _, err := readAll(t, newC(), "https://"+bigHost+"/redirect-to-small"); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("big -> small: got %v, want ErrBodyTooLarge (small host keeps the global cap)", err)
	}
	// small -> big: the big host's cap applies to its own body.
	if body, err := readAll(t, newC(), "https://"+smallHost+"/redirect-to-big"); err != nil || len(body) != size {
		t.Fatalf("small -> big: got %d bytes, err %v; want %d, nil", len(body), err, size)
	}
}

func TestDefaultHostBodyCaps(t *testing.T) {
	limits := DefaultHostLimits()
	for _, h := range []string{"registry.npmjs.org", "index.crates.io"} {
		if got := limits[h].MaxBodyBytes; got != 64<<20 {
			t.Errorf("%s default cap = %d, want 64 MiB", h, got)
		}
		if err := limits[h].validate(); err != nil {
			t.Errorf("%s default limit invalid: %v", h, err)
		}
	}
	if DefaultMaxBodyBytes != 10<<20 {
		t.Errorf("global default = %d, want 10 MiB", DefaultMaxBodyBytes)
	}
	c, _ := newTestClient(t, nil)
	if got := c.limiter.maxBodyBytes("REGISTRY.npmjs.org.", c.cfg.MaxBodyBytes); got != 64<<20 {
		t.Errorf("npm cap in a default client = %d, want 64 MiB", got)
	}
	if got := c.limiter.maxBodyBytes("pypi.org", c.cfg.MaxBodyBytes); got != 10<<20 {
		t.Errorf("pypi cap in a default client = %d, want the 10 MiB global", got)
	}
}

func TestNegativeHostBodyCapRejected(t *testing.T) {
	if _, err := New(Config{HostLimits: map[string]HostLimit{"x.test": hostLimit(-1)}}); err == nil {
		t.Fatal("New accepted a negative host body cap")
	}
}
