package netguard

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

// twoBuckets gives the loopback test host a tight anonymous bucket and a
// roomy authenticated one, so the bucket a request used shows in its delay.
func twoBuckets(cfg *Config) {
	cfg.HostLimits = map[string]HostLimit{"127.0.0.1": {
		Anonymous:     Limit{Rate: 0.01, Burst: 1}, // second request waits ~100s
		Authenticated: Limit{Rate: 1000, Burst: 1000},
	}}
}

// sendTwo sends two requests with the given header and returns the gap
// between their arrivals on the server.
func sendTwo(t *testing.T, header, value string, mutate func(*Config)) time.Duration {
	t.Helper()
	var clock *fakeClock
	srv, times := retryServer(t, &clock)
	var c *Client
	c, clock = newTestClient(t, func(cfg *Config) {
		twoBuckets(cfg)
		if mutate != nil {
			mutate(cfg)
		}
	})
	for range 2 {
		req := get(t, srv.URL)
		if header != "" {
			req.Header.Set(header, value)
		}
		resp, err := c.HTTPClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	return (*times)[1].Sub((*times)[0])
}

// AC: a request carrying PRIVATE-TOKEN counts as authenticated.
func TestPrivateTokenUsesAuthenticatedBucket(t *testing.T) {
	if gap := sendTwo(t, "PRIVATE-TOKEN", "glpat-synthetic", nil); gap > time.Second {
		t.Errorf("PRIVATE-TOKEN requests waited %v, want the authenticated bucket", gap)
	}
	if gap := sendTwo(t, "", "", nil); gap < 90*time.Second {
		t.Errorf("anonymous requests waited %v, want >= 90s (control)", gap)
	}
	if gap := sendTwo(t, "Authorization", "Bearer x", nil); gap > time.Second {
		t.Errorf("Authorization requests waited %v, want the authenticated bucket", gap)
	}
}

// AC: the list of auth headers is configurable.
func TestExtraAuthHeaders(t *testing.T) {
	if gap := sendTwo(t, "X-Custom-Key", "k", nil); gap < 90*time.Second {
		t.Errorf("unlisted header waited %v, want anonymous bucket", gap)
	}
	gap := sendTwo(t, "x-custom-key", "k", func(c *Config) { c.ExtraAuthHeaders = []string{"X-Custom-Key"} })
	if gap > time.Second {
		t.Errorf("configured header waited %v, want authenticated bucket", gap)
	}
	// An empty header value is not a credential.
	if gap := sendTwo(t, "PRIVATE-TOKEN", "", nil); gap < 90*time.Second {
		t.Errorf("empty PRIVATE-TOKEN waited %v, want anonymous bucket", gap)
	}
}

// AC: unprefixed RateLimit-* headers are honoured like the X- ones.
func TestUnprefixedRateLimitHeaders(t *testing.T) {
	var clock *fakeClock
	srv, times := retryServer(t, &clock,
		func(w http.ResponseWriter) {
			w.Header().Set("RateLimit-Limit", "500")
			w.Header().Set("RateLimit-Remaining", "0")
			w.Header().Set("RateLimit-Reset", strconv.FormatInt(clock.Now().Add(90*time.Second).Unix(), 10))
			w.WriteHeader(http.StatusOK)
		},
		func(w http.ResponseWriter) {
			w.Header().Set("RateLimit-Remaining", "0")
			w.Header().Set("RateLimit-Reset", strconv.FormatInt(clock.Now().Add(30*time.Second).Unix(), 10))
			w.WriteHeader(http.StatusTooManyRequests)
		},
	)
	var c *Client
	c, clock = newTestClient(t, nil)
	for i := range 2 {
		if _, err := Fetch(t.Context(), c.HTTPClient(), get(t, srv.URL), Validators{}); err != nil {
			t.Fatalf("Fetch %d: %v", i, err)
		}
	}
	if len(*times) != 3 {
		t.Fatalf("requests: %d, want 3", len(*times))
	}
	if gap := (*times)[1].Sub((*times)[0]); gap < 90*time.Second {
		t.Errorf("request after exhausted budget sent after %v, want >= 90s", gap)
	}
	if gap := (*times)[2].Sub((*times)[1]); gap < 30*time.Second {
		t.Errorf("retry after 429 sent after %v, want >= 30s", gap)
	}
}

func TestRetryAtHeaderPrecedence(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	h := http.Header{}
	h.Set("X-RateLimit-Remaining", "0")
	h.Set("X-RateLimit-Reset", "1000050")
	h.Set("RateLimit-Remaining", "0")
	h.Set("RateLimit-Reset", "1000090")
	if got := retryAt(h, now); !got.Equal(time.Unix(1000050, 0)) {
		t.Errorf("retryAt = %v, want the X- header to win", got)
	}
	h = http.Header{}
	h.Set("RateLimit-Remaining", "3")
	h.Set("RateLimit-Reset", "1000090")
	if got := retryAt(h, now); !got.IsZero() {
		t.Errorf("retryAt = %v with budget left, want zero", got)
	}
}

// AC: gitlab.com has default limits, anonymous and authenticated.
func TestDefaultHostLimitsGitLab(t *testing.T) {
	gl, ok := DefaultHostLimits()["gitlab.com"]
	if !ok {
		t.Fatal("gitlab.com has no default limit")
	}
	if got := float64(gl.Anonymous.Burst) + gl.Anonymous.Rate*60; got > 500+1e-6 || got < 400 {
		t.Errorf("anonymous budget %.1f/min, want just under 500", got)
	}
	if got := float64(gl.Authenticated.Burst) + gl.Authenticated.Rate*60; got > 2000+1e-6 || got < 1600 {
		t.Errorf("authenticated budget %.1f/min, want just under 2000", got)
	}
	if gl.Authenticated.Rate <= gl.Anonymous.Rate {
		t.Error("authenticated limit should exceed anonymous")
	}
}
