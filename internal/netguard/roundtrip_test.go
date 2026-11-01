package netguard

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func stubResponse(r *http.Request, code int, h http.Header, body io.ReadCloser) *http.Response {
	if h == nil {
		h = http.Header{}
	}
	if body == nil {
		body = io.NopCloser(strings.NewReader(""))
	}
	return &http.Response{StatusCode: code, Status: http.StatusText(code), Header: h, Body: body, Request: r}
}

// S3/S4: Retry-After on a redirect target pauses that hop's host only.
func TestRedirectPausesHopHostOnly(t *testing.T) {
	c, clock := newTestClient(t, func(cfg *Config) {
		cfg.Resolver = fakeResolver{"a.example": addrs("192.0.2.1"), "b.example": addrs("192.0.2.2")}
		cfg.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			switch r.URL.Hostname() {
			case "a.example":
				return stubResponse(r, http.StatusFound, http.Header{"Location": {"https://b.example/x"}}, nil), nil
			default:
				return stubResponse(r, http.StatusTooManyRequests, http.Header{"Retry-After": {"3600"}}, nil), nil
			}
		})
	})
	_, err := Fetch(t.Context(), c.HTTPClient(), get(t, "https://a.example/"), Validators{})
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("got %v, want *StatusError 429", err)
	}

	start := clock.Now()
	if err := c.limiter.wait(t.Context(), "a.example", false); err != nil {
		t.Fatal(err)
	}
	if got := clock.Now().Sub(start); got != 0 {
		t.Fatalf("a.example waited %v, want no pause", got)
	}
	if err := c.limiter.wait(t.Context(), "b.example", false); err != nil {
		t.Fatal(err)
	}
	if got := clock.Now().Sub(start); got < 59*time.Minute {
		t.Fatalf("b.example waited %v, want ~1h", got)
	}
}

func TestRoundTripDoesNotMutateRequest(t *testing.T) {
	var ua string
	c, _ := newTestClient(t, func(cfg *Config) {
		cfg.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			ua = r.UserAgent()
			return stubResponse(r, http.StatusOK, nil, nil), nil
		})
	})
	req := get(t, "https://192.0.2.1/")
	resp, err := c.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !strings.HasPrefix(ua, "angelia/") {
		t.Fatalf("sent User-Agent %q", ua)
	}
	if got := req.Header.Get("User-Agent"); got != "" {
		t.Fatalf("caller's request was mutated: User-Agent %q", got)
	}
}

func TestDefaultTransportUsesOwnDialer(t *testing.T) {
	c, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := c.transport.(*http.Transport)
	if !ok {
		t.Fatalf("default transport is %T, want *http.Transport", c.transport)
	}
	if tr == http.DefaultTransport || tr.DialContext == nil {
		t.Fatal("default transport must be netguard's own, with its own dialer")
	}
	if hc := c.HTTPClient(); hc.Transport != c || hc.Timeout != 0 {
		t.Fatalf("HTTPClient: transport %T, timeout %v", hc.Transport, hc.Timeout)
	}
}

func TestHTTPClientStopsAfterMaxRedirects(t *testing.T) {
	var hops atomic.Int64
	c, _ := newTestClient(t, func(cfg *Config) {
		cfg.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			hops.Add(1)
			return stubResponse(r, http.StatusFound, http.Header{"Location": {"/again"}}, nil), nil
		})
	})
	if _, err := Fetch(t.Context(), c.HTTPClient(), get(t, "https://192.0.2.1/"), Validators{}); err == nil {
		t.Fatal("want redirect limit error")
	}
	if hops.Load() != maxRedirects {
		t.Fatalf("hops: %d, want %d", hops.Load(), maxRedirects)
	}
}

// S5: when the context deadline comes before the server's Retry-After, the
// response is returned instead of sleeping into a deadline error.
func TestDeadlineBeforeRetryAfterReturnsResponse(t *testing.T) {
	var clock *fakeClock
	srv, times := retryServer(t, &clock, func(w http.ResponseWriter) {
		w.Header().Set("Retry-After", "60")
		w.Header().Set("Date", clock.Now().Format(http.TimeFormat))
		w.WriteHeader(http.StatusTooManyRequests)
	})
	var c *Client
	c, clock = newTestClient(t, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := Fetch(ctx, c.HTTPClient(), get(t, srv.URL), Validators{})
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("got %v, want *StatusError 429", err)
	}
	if want := fakeEpoch.Add(time.Minute); !se.RetryAt.Equal(want) {
		t.Fatalf("RetryAt %v, want %v", se.RetryAt, want)
	}
	if len(*times) != 1 {
		t.Fatalf("server hit %d times, want 1", len(*times))
	}
}

func TestFetchDrainsErrorBody(t *testing.T) {
	body := &readCounter{}
	c, _ := newTestClient(t, func(cfg *Config) {
		cfg.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			return stubResponse(r, http.StatusNotFound, nil, body), nil
		})
	})
	_, err := Fetch(t.Context(), c.HTTPClient(), get(t, "https://192.0.2.1/"), Validators{})
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusNotFound {
		t.Fatalf("got %v, want 404", err)
	}
	if body.reads.Load() == 0 || !body.closed.Load() {
		t.Fatalf("error body not drained and closed: reads=%d closed=%v", body.reads.Load(), body.closed.Load())
	}
	if strings.Count(err.Error(), "netguard:") != 1 {
		t.Fatalf("error %q repeats the package prefix", err)
	}
}

func TestRequestWithBodyNotRetried(t *testing.T) {
	var hits atomic.Int64
	c, _ := newTestClient(t, func(cfg *Config) {
		cfg.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			hits.Add(1)
			return stubResponse(r, http.StatusServiceUnavailable, nil, nil), nil
		})
	})
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://192.0.2.1/", strings.NewReader("q"))
	resp, err := c.HTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if hits.Load() != 1 {
		t.Fatalf("attempts: %d, want 1", hits.Load())
	}
}

func TestBlockedErrorHasSinglePrefix(t *testing.T) {
	c, _ := newTestClient(t, func(cfg *Config) { cfg.Allow = nil })
	_, err := Fetch(t.Context(), c.HTTPClient(), get(t, "http://10.0.0.1/"), Validators{})
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("got %v", err)
	}
	if strings.Count(err.Error(), "netguard:") != 1 {
		t.Fatalf("error %q repeats the package prefix", err)
	}
}
