package netguard

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
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
	_, err := Fetch(t.Context(), c.HTTPClient(), get(t, "https://192.0.2.1/"), Validators{})
	if err == nil || !strings.Contains(err.Error(), "stopped after 5 redirects") {
		t.Fatalf("got %v, want an error naming the cap of 5", err)
	}
	if MaxRedirects != 5 || hops.Load() != 5 {
		t.Fatalf("hops: %d, MaxRedirects %d, want 5", hops.Load(), MaxRedirects)
	}
}

// D109: a redirect from https to http is refused with ErrInvalidURL (not
// retried), and the http hop is never sent. An http base URL may still
// redirect to http.
func TestHTTPClientRefusesHTTPSDowngrade(t *testing.T) {
	var sent []string
	c, _ := newTestClient(t, func(cfg *Config) {
		cfg.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			sent = append(sent, r.URL.String())
			switch r.URL.Path {
			case "/down":
				return stubResponse(r, http.StatusFound, http.Header{"Location": {"http://192.0.2.1/plain"}}, nil), nil
			case "/up-down":
				return stubResponse(r, http.StatusFound, http.Header{"Location": {"https://192.0.2.1/down"}}, nil), nil
			case "/hop":
				return stubResponse(r, http.StatusFound, http.Header{"Location": {"http://192.0.2.2/plain"}}, nil), nil
			}
			return stubResponse(r, http.StatusOK, nil, nil), nil
		})
	})
	for _, start := range []string{"https://192.0.2.1/down", "http://192.0.2.1/up-down"} {
		sent = nil
		_, err := Fetch(t.Context(), c.HTTPClient(), get(t, start), Validators{})
		if !errors.Is(err, ErrInvalidURL) || !strings.Contains(err.Error(), "from https to http") {
			t.Errorf("%s: got %v, want ErrInvalidURL for the downgrade", start, err)
		}
		if slices.Contains(sent, "http://192.0.2.1/plain") {
			t.Errorf("%s: the http hop was sent: %v", start, sent)
		}
	}
	sent = nil
	res, err := Fetch(t.Context(), c.HTTPClient(), get(t, "http://192.0.2.1/hop"), Validators{})
	if err != nil || res.StatusCode != http.StatusOK || len(sent) != 2 {
		t.Fatalf("http to http: %v, sent %v", err, sent)
	}
}

// D105: the exemption is per hop. A listed host that redirects to an
// unlisted name resolving to a private address is refused at the second
// hop, which is never sent.
func TestAllowHostsPerHop(t *testing.T) {
	var sent []string
	c, _ := newTestClient(t, func(cfg *Config) {
		cfg.Allow = nil
		cfg.AllowHosts = []string{"registry.lan"}
		cfg.Resolver = fakeResolver{"registry.lan": addrs("10.0.0.5"), "other.lan": addrs("10.0.0.6")}
		cfg.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			sent = append(sent, r.URL.Host)
			if r.URL.Host == "registry.lan" {
				return stubResponse(r, http.StatusFound, http.Header{"Location": {"http://other.lan/x"}}, nil), nil
			}
			return stubResponse(r, http.StatusOK, nil, nil), nil
		})
	})
	_, err := Fetch(t.Context(), c.HTTPClient(), get(t, "http://registry.lan/simple/"), Validators{})
	var be *BlockedError
	if !errors.As(err, &be) || be.Host != "other.lan" {
		t.Fatalf("got %v, want other.lan refused", err)
	}
	if len(sent) != 1 || sent[0] != "registry.lan" {
		t.Fatalf("sent %v, want only registry.lan", sent)
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
