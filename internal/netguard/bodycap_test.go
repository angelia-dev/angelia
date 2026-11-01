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
	"time"
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
	for _, h := range []string{"registry.npmjs.org", "pypi.org", "index.crates.io"} {
		if got := defaultHostAllowances[h]; got.maxBodyBytes != 64<<20 || got.timeout != 2*time.Minute {
			t.Errorf("%s default allowance = %+v, want 64 MiB and 2m", h, got)
		}
		if _, ok := limits[h]; ok {
			t.Errorf("%s has a default rate entry; rates must come from the fallback", h)
		}
	}
	if DefaultMaxBodyBytes != 10<<20 {
		t.Errorf("global default = %d, want 10 MiB", DefaultMaxBodyBytes)
	}
	c, _ := newTestClient(t, nil)
	if got := c.limiter.maxBodyBytes("REGISTRY.npmjs.org.", c.cfg.MaxBodyBytes); got != 64<<20 {
		t.Errorf("npm cap in a default client = %d, want 64 MiB", got)
	}
	if got := c.limiter.maxBodyBytes("gitlab.com", c.cfg.MaxBodyBytes); got != 10<<20 {
		t.Errorf("gitlab cap in a default client = %d, want the 10 MiB global", got)
	}
}

func TestNegativeHostBodyCapRejected(t *testing.T) {
	if _, err := New(Config{HostLimits: map[string]HostLimit{"x.test": hostLimit(-1)}}); err == nil {
		t.Fatal("New accepted a negative host body cap")
	}
}

// Caps and rates are independent: a custom fallback sets the rate of a host
// with a built-in cap, which keeps its cap.
func TestBuiltinCapKeepsCustomFallbackRate(t *testing.T) {
	fb := HostLimit{Authenticated: Limit{Rate: 7, Burst: 3}, Anonymous: Limit{Rate: 7, Burst: 3}}
	c, _ := newTestClient(t, func(c *Config) { c.FallbackLimit = fb })
	c.limiter.mu.Lock()
	b := c.limiter.bucketLocked("registry.npmjs.org", false, c.clock.Now())
	rate, burst := b.limit.Rate, b.limit.Burst
	c.limiter.mu.Unlock()
	if rate != 7 || burst != 3 {
		t.Errorf("npm rate = %v burst %d, want the custom fallback 7/3", rate, burst)
	}
	if got := c.limiter.maxBodyBytes("registry.npmjs.org", c.cfg.MaxBodyBytes); got != 64<<20 {
		t.Errorf("npm cap = %d, want 64 MiB", got)
	}
}

// Resolution order: operator cap, built-in cap, global. A rates-only
// operator entry keeps the built-in cap.
func TestBodyCapResolutionOrder(t *testing.T) {
	c, _ := newTestClient(t, func(c *Config) {
		c.HostLimits = map[string]HostLimit{
			"registry.npmjs.org": hostLimit(0),       // rates only
			"index.crates.io":    hostLimit(1 << 20), // operator cap wins
		}
	})
	if got := c.limiter.maxBodyBytes("registry.npmjs.org", 1); got != 64<<20 {
		t.Errorf("rates-only entry: cap %d, want built-in 64 MiB", got)
	}
	if got := c.limiter.maxBodyBytes("index.crates.io", 1); got != 1<<20 {
		t.Errorf("operator cap: %d, want 1 MiB", got)
	}
	if got := c.limiter.maxBodyBytes("other.test", 5); got != 5 {
		t.Errorf("unknown host: cap %d, want global 5", got)
	}
}

func TestFallbackBodyCapRejected(t *testing.T) {
	if _, err := New(Config{FallbackLimit: hostLimit(1 << 20)}); err == nil {
		t.Fatal("New accepted MaxBodyBytes in FallbackLimit")
	}
}

func TestTimeoutResolutionOrder(t *testing.T) {
	c, _ := newTestClient(t, func(c *Config) {
		c.Timeout = 30 * time.Second
		c.HostLimits = map[string]HostLimit{
			"registry.npmjs.org": {Authenticated: anyLimit, Anonymous: anyLimit},                     // rates only
			"pypi.org":           {Authenticated: anyLimit, Anonymous: anyLimit, Timeout: time.Hour}, // operator wins
			"mine.test":          {Authenticated: anyLimit, Anonymous: anyLimit, Timeout: time.Minute},
		}
	})
	for host, want := range map[string]time.Duration{
		"registry.npmjs.org": 2 * time.Minute,
		"index.crates.io":    2 * time.Minute,
		"pypi.org":           time.Hour,
		"mine.test":          time.Minute,
		"other.test":         30 * time.Second,
	} {
		if got := c.limiter.timeout(host, c.cfg.Timeout); got != want {
			t.Errorf("timeout(%s) = %v, want %v", host, got, want)
		}
	}
}

func TestNegativeHostTimeoutRejected(t *testing.T) {
	l := hostLimit(0)
	l.Timeout = -time.Second
	if _, err := New(Config{HostLimits: map[string]HostLimit{"x.test": l}}); err == nil {
		t.Fatal("New accepted a negative host timeout")
	}
}

// AC: a body that takes longer than the global timeout fails on a normal
// host and succeeds on a host with a longer timeout. Durations are scaled
// down: global 100ms, slow body 250ms, host timeout 2s.
func TestHostTimeoutLetsSlowBodyFinish(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "first")
		w.(http.Flusher).Flush()
		time.Sleep(250 * time.Millisecond)
		_, _ = io.WriteString(w, "second")
	}))
	defer srv.Close()
	host := mustHost(t, srv.URL)

	normal, _ := newTestClient(t, func(c *Config) {
		c.Timeout = 100 * time.Millisecond
		c.MaxAttempts = 1
	})
	if _, err := readAll(t, normal, srv.URL); err == nil {
		t.Fatal("slow body finished on a host with the global timeout, want a timeout error")
	}

	large, _ := newTestClient(t, func(c *Config) {
		c.Timeout = 100 * time.Millisecond
		c.MaxAttempts = 1
		l := hostLimit(0)
		l.Timeout = 2 * time.Second
		c.HostLimits = map[string]HostLimit{host: l}
	})
	body, err := readAll(t, large, srv.URL)
	if err != nil || string(body) != "firstsecond" {
		t.Fatalf("host with longer timeout: got %q, err %v", body, err)
	}
}
