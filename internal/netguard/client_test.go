package netguard

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/buildinfo"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

var loopbackOnly = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}

// newTestClient returns a client with a fake auto-advancing clock, a
// generous rate limit, and 127.0.0.1 allowlisted for httptest servers.
func newTestClient(t *testing.T, mutate func(*Config)) (*Client, *fakeClock) {
	t.Helper()
	clock := newFakeClock(true)
	cfg := Config{
		Clock:         clock,
		Allow:         loopbackOnly,
		Resolver:      fakeResolver{},
		FallbackLimit: HostLimit{Anonymous: Limit{Rate: 1000, Burst: 1000}, Authenticated: Limit{Rate: 1000, Burst: 1000}},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.jitter = func(d time.Duration) time.Duration { return d } // deterministic: always the cap
	return c, clock
}

func get(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// AC: a 304 response is surfaced as NotModified and costs no body parsing.
func TestFetchNotModified(t *testing.T) {
	prev := Validators{ETag: `"v1"`, LastModified: "Sat, 31 Oct 2026 10:00:00 GMT"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != prev.ETag || r.Header.Get("If-Modified-Since") != prev.LastModified {
			t.Errorf("conditional headers missing: %v", r.Header)
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()
	c, _ := newTestClient(t, nil)

	res, err := Fetch(t.Context(), c.HTTPClient(), get(t, srv.URL), prev)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !res.NotModified || res.StatusCode != http.StatusNotModified {
		t.Fatalf("got NotModified=%v status=%d, want true/304", res.NotModified, res.StatusCode)
	}
	if res.Body != nil {
		t.Fatalf("304 must not carry a body, got %d bytes", len(res.Body))
	}
	if res.Validators != prev {
		t.Fatalf("validators: got %+v, want %+v", res.Validators, prev)
	}
}

// readCounter records whether anyone read the body.
type readCounter struct {
	reads  atomic.Int64
	closed atomic.Bool
}

func (r *readCounter) Read([]byte) (int, error) { r.reads.Add(1); return 0, io.EOF }
func (r *readCounter) Close() error             { r.closed.Store(true); return nil }

func TestFetchNotModifiedDoesNotReadBody(t *testing.T) {
	body := &readCounter{}
	c, _ := newTestClient(t, func(cfg *Config) {
		cfg.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			h := http.Header{}
			h.Set("ETag", `"v2"`)
			return &http.Response{StatusCode: http.StatusNotModified, Header: h, Body: body, Request: r}, nil
		})
	})
	res, err := Fetch(t.Context(), c.HTTPClient(), get(t, "https://192.0.2.1/pkg"), Validators{ETag: `"v1"`, LastModified: "lm"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.NotModified {
		t.Fatal("want NotModified")
	}
	if n := body.reads.Load(); n != 0 {
		t.Fatalf("body was read %d times on a 304", n)
	}
	if !body.closed.Load() {
		t.Fatal("body not closed")
	}
	// A new validator on the 304 replaces the old one; the others are kept.
	if res.Validators.ETag != `"v2"` || res.Validators.LastModified != "lm" {
		t.Fatalf("validators: %+v", res.Validators)
	}
}

func TestFetchOKStoresValidators(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			t.Errorf("unexpected If-None-Match on first fetch")
		}
		w.Header().Set("ETag", `"abc"`)
		w.Header().Set("Last-Modified", "Sun, 01 Nov 2026 09:00:00 GMT")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	c, _ := newTestClient(t, nil)
	res, err := Fetch(t.Context(), c.HTTPClient(), get(t, srv.URL), Validators{})
	if err != nil {
		t.Fatal(err)
	}
	if res.NotModified || string(res.Body) != `{"ok":true}` {
		t.Fatalf("unexpected result %+v", res)
	}
	want := Validators{ETag: `"abc"`, LastModified: "Sun, 01 Nov 2026 09:00:00 GMT"}
	if res.Validators != want {
		t.Fatalf("validators: got %+v, want %+v", res.Validators, want)
	}
}

// retryServer answers with the given handlers in order, then 200 "ok", and
// records the fake time of each request.
func retryServer(t *testing.T, clock **fakeClock, steps ...func(http.ResponseWriter)) (*httptest.Server, *[]time.Time) {
	t.Helper()
	var mu sync.Mutex
	var times []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		i := len(times)
		times = append(times, (*clock).Now())
		mu.Unlock()
		if i < len(steps) {
			steps[i](w)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)
	return srv, &times
}

// AC: Retry-After (seconds) honoured.
func TestRetryAfterSeconds(t *testing.T) {
	var clock *fakeClock
	srv, times := retryServer(t, &clock, func(w http.ResponseWriter) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	var c *Client
	c, clock = newTestClient(t, nil)
	res, err := Fetch(t.Context(), c.HTTPClient(), get(t, srv.URL), Validators{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(res.Body) != "ok" || len(*times) != 2 {
		t.Fatalf("body %q after %d requests", res.Body, len(*times))
	}
	if gap := (*times)[1].Sub((*times)[0]); gap < 7*time.Second || gap > 8*time.Second {
		t.Fatalf("retried after %v, want 7s", gap)
	}
}

// AC: Retry-After (HTTP date) honoured.
func TestRetryAfterHTTPDate(t *testing.T) {
	var clock *fakeClock
	srv, times := retryServer(t, &clock, func(w http.ResponseWriter) {
		w.Header().Set("Retry-After", clock.Now().Add(42*time.Second).Format(http.TimeFormat))
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	var c *Client
	c, clock = newTestClient(t, nil)
	if _, err := Fetch(t.Context(), c.HTTPClient(), get(t, srv.URL), Validators{}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if gap := (*times)[1].Sub((*times)[0]); gap < 42*time.Second || gap > 43*time.Second {
		t.Fatalf("retried after %v, want 42s", gap)
	}
}

func TestRetryAfterBeyondMaxWaitIsReturned(t *testing.T) {
	var clock *fakeClock
	srv, times := retryServer(t, &clock, func(w http.ResponseWriter) {
		w.Header().Set("Retry-After", "3600")
		w.Header().Set("Date", clock.Now().Format(http.TimeFormat))
		w.WriteHeader(http.StatusTooManyRequests)
	})
	var c *Client
	c, clock = newTestClient(t, nil)
	_, err := Fetch(t.Context(), c.HTTPClient(), get(t, srv.URL), Validators{})
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("got %v, want *StatusError 429", err)
	}
	if want := fakeEpoch.Add(time.Hour); !se.RetryAt.Equal(want) {
		t.Fatalf("RetryAt %v, want %v", se.RetryAt, want)
	}
	if len(*times) != 1 {
		t.Fatalf("server hit %d times, want 1 (no retry)", len(*times))
	}
	// The host stays paused for later requests.
	before := clock.Now()
	_, _ = Fetch(t.Context(), c.HTTPClient(), get(t, srv.URL), Validators{})
	if waited := clock.Now().Sub(before); waited < 59*time.Minute {
		t.Fatalf("next request waited %v, want ~1h", waited)
	}
}

func TestRetries5xxWithBackoff(t *testing.T) {
	var clock *fakeClock
	fail := func(code int) func(http.ResponseWriter) {
		return func(w http.ResponseWriter) { w.WriteHeader(code) }
	}
	srv, times := retryServer(t, &clock, fail(500), fail(502), fail(504))
	var c *Client
	c, clock = newTestClient(t, func(cfg *Config) {
		cfg.BaseBackoff = time.Second
		cfg.MaxBackoff = 3 * time.Second
	})
	res, err := Fetch(t.Context(), c.HTTPClient(), get(t, srv.URL), Validators{})
	if err != nil || string(res.Body) != "ok" {
		t.Fatalf("Fetch: %v %v", res, err)
	}
	if len(*times) != 4 {
		t.Fatalf("server hit %d times, want 4", len(*times))
	}
	// The jitter stub returns the cap: 1s, 2s, then 3s (MaxBackoff).
	want := []time.Duration{time.Second, 2 * time.Second, 3 * time.Second}
	for i, w := range want {
		if gap := (*times)[i+1].Sub((*times)[i]); gap < w || gap > w+time.Second {
			t.Errorf("gap %d: %v, want %v", i, gap, w)
		}
	}
}

func TestFullJitterStaysWithinCap(t *testing.T) {
	c, _ := newTestClient(t, nil)
	c.jitter = fullJitter
	for range 100 {
		if d := c.backoff(3); d < 0 || d > c.cfg.BaseBackoff*4 {
			t.Fatalf("backoff(3) = %v, want in [0, %v]", d, c.cfg.BaseBackoff*4)
		}
	}
	if d := c.backoff(60); d > c.cfg.MaxBackoff {
		t.Fatalf("backoff(60) = %v exceeds MaxBackoff %v", d, c.cfg.MaxBackoff)
	}
}

func TestRetriesExhausted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	var hits atomic.Int64
	c, _ := newTestClient(t, func(cfg *Config) {
		cfg.MaxAttempts = 3
		base := http.DefaultTransport
		cfg.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			hits.Add(1)
			return base.RoundTrip(r)
		})
	})
	_, err := Fetch(t.Context(), c.HTTPClient(), get(t, srv.URL), Validators{})
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("got %v, want *StatusError 503", err)
	}
	if !strings.Contains(err.Error(), "503") {
		t.Fatalf("error text %q lacks status", err)
	}
	if hits.Load() != 3 {
		t.Fatalf("attempts: %d, want 3", hits.Load())
	}
}

func TestRetriesTimeouts(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	c, _ := newTestClient(t, func(cfg *Config) { cfg.Timeout = 100 * time.Millisecond })
	res, err := Fetch(t.Context(), c.HTTPClient(), get(t, srv.URL), Validators{})
	if err != nil || string(res.Body) != "ok" {
		t.Fatalf("Fetch: %v, %v", res, err)
	}
	if hits.Load() != 2 {
		t.Fatalf("attempts: %d, want 2", hits.Load())
	}
}

func TestNonIdempotentNotRetried(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c, _ := newTestClient(t, nil)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("{}"))
	resp, err := c.HTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || hits.Load() != 1 {
		t.Fatalf("status %d after %d attempts, want 502 after 1", resp.StatusCode, hits.Load())
	}
}

func TestGitHubRateLimitHeaders(t *testing.T) {
	var clock *fakeClock
	srv, times := retryServer(t, &clock,
		// Budget exhausted on a successful response: pause until reset.
		func(w http.ResponseWriter) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(clock.Now().Add(90*time.Second).Unix(), 10))
			_, _ = io.WriteString(w, "ok")
		},
		// GitHub's primary limit answers 403 with remaining 0: retry at reset.
		func(w http.ResponseWriter) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(clock.Now().Add(30*time.Second).Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
		},
	)
	var c *Client
	c, clock = newTestClient(t, nil)
	if _, err := Fetch(t.Context(), c.HTTPClient(), get(t, srv.URL), Validators{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Fetch(t.Context(), c.HTTPClient(), get(t, srv.URL), Validators{}); err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if len(*times) != 3 {
		t.Fatalf("requests: %d, want 3", len(*times))
	}
	if gap := (*times)[1].Sub((*times)[0]); gap < 90*time.Second {
		t.Errorf("request after exhausted budget sent after %v, want >= 90s", gap)
	}
	if gap := (*times)[2].Sub((*times)[1]); gap < 30*time.Second {
		t.Errorf("retry after 403 sent after %v, want >= 30s", gap)
	}
}

func TestPlain403NotRetried(t *testing.T) {
	var clock *fakeClock
	srv, times := retryServer(t, &clock, func(w http.ResponseWriter) { w.WriteHeader(http.StatusForbidden) })
	var c *Client
	c, clock = newTestClient(t, nil)
	_, err := Fetch(t.Context(), c.HTTPClient(), get(t, srv.URL), Validators{})
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusForbidden || len(*times) != 1 {
		t.Fatalf("got %v after %d requests, want 403 after 1", err, len(*times))
	}
}

// AC: response bodies above the cap (default 10 MB) are truncated with an error.
func TestBodyCapDefault(t *testing.T) {
	const over = DefaultMaxBodyBytes + 1024
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("a"), over))
	}))
	defer srv.Close()
	c, _ := newTestClient(t, nil)
	if c.cfg.MaxBodyBytes != 10<<20 {
		t.Fatalf("default cap %d, want 10 MiB", c.cfg.MaxBodyBytes)
	}

	resp, err := c.HTTPClient().Do(get(t, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("ReadAll: got %v, want ErrBodyTooLarge", err)
	}
	if len(body) != DefaultMaxBodyBytes {
		t.Fatalf("truncated body is %d bytes, want %d", len(body), DefaultMaxBodyBytes)
	}

	if _, err := Fetch(t.Context(), c.HTTPClient(), get(t, srv.URL), Validators{}); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("Fetch: got %v, want ErrBodyTooLarge", err)
	}
}

func TestBodyCapExactSizeOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "0123456789")
	}))
	defer srv.Close()
	c, _ := newTestClient(t, func(cfg *Config) { cfg.MaxBodyBytes = 10 })
	res, err := Fetch(t.Context(), c.HTTPClient(), get(t, srv.URL), Validators{})
	if err != nil || string(res.Body) != "0123456789" {
		t.Fatalf("Fetch: %q, %v", res.Body, err)
	}
}

func TestUserAgent(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.UserAgent() }))
	defer srv.Close()
	c, _ := newTestClient(t, nil)
	if _, err := Fetch(t.Context(), c.HTTPClient(), get(t, srv.URL), Validators{}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "angelia/"+buildinfo.Version()) {
		t.Fatalf("User-Agent %q, want angelia/<version>", got)
	}
	// An explicit User-Agent on the request wins.
	req := get(t, srv.URL)
	req.Header.Set("User-Agent", "custom/1")
	if _, err := Fetch(t.Context(), c.HTTPClient(), req, Validators{}); err != nil {
		t.Fatal(err)
	}
	if got != "custom/1" {
		t.Fatalf("User-Agent %q, want custom/1", got)
	}
}

func TestClientRefusesBlockedAddresses(t *testing.T) {
	var hits atomic.Int64
	c, _ := newTestClient(t, func(cfg *Config) {
		cfg.Allow = nil
		cfg.Transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
			hits.Add(1)
			return nil, errors.New("transport must not be called")
		})
	})
	for _, u := range []string{
		"http://127.0.0.1/", "http://10.0.0.1/", "http://169.254.169.254/latest/meta-data/",
		"http://[::1]/", "http://[fc00::1]/",
	} {
		resp, err := c.HTTPClient().Do(get(t, u))
		if resp != nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, ErrBlocked) {
			t.Errorf("Do(%s): got %v, want ErrBlocked", u, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("transport called %d times", hits.Load())
	}
}

func TestClientRefusesRedirectToBlockedAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://10.0.0.1/secret", http.StatusFound)
	}))
	defer srv.Close()
	c, _ := newTestClient(t, nil)
	_, err := Fetch(t.Context(), c.HTTPClient(), get(t, srv.URL), Validators{})
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("got %v, want ErrBlocked", err)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	bad := []Config{
		{MaxAttempts: -1},
		{MaxBodyBytes: -1},
		{FallbackLimit: HostLimit{Anonymous: Limit{Rate: -1, Burst: 1}, Authenticated: Limit{Rate: 1, Burst: 1}}},
		{HostLimits: map[string]HostLimit{"x": {Anonymous: Limit{Rate: 1, Burst: 0}, Authenticated: Limit{Rate: 1, Burst: 1}}}},
	}
	for i, cfg := range bad {
		if _, err := New(cfg); err == nil {
			t.Errorf("config %d: want error", i)
		}
	}
	c, err := New(Config{})
	if err != nil {
		t.Fatalf("New(zero Config): %v", err)
	}
	if c.cfg.Timeout != DefaultTimeout || c.cfg.MaxAttempts != DefaultMaxAttempts {
		t.Fatalf("defaults not applied: %+v", c.cfg)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := fakeEpoch
	cases := map[string]time.Duration{
		"":     0,
		"5":    5 * time.Second,
		"-3":   0,
		"soon": 0,
		now.Add(10 * time.Second).Format(http.TimeFormat): 10 * time.Second,
		now.Add(-time.Minute).Format(http.TimeFormat):     0,
		// S6: longer than a day is clamped to the maximum pause, not dropped.
		"999999999": defaultMaxPause,
		"3601":      defaultMaxPause,
		now.Add(48 * time.Hour).Format(http.TimeFormat): defaultMaxPause,
	}
	for in, want := range cases {
		if got := ParseRetryAfter(in, now); got != want {
			t.Errorf("ParseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}
