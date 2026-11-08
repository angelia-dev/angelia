package netguard

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type waitObs struct {
	mu        sync.Mutex
	hosts     []string
	waits     []time.Duration
	pauses    []string // host/reason
	responses []string // host/class
	refusals  []string
}

func (o *waitObs) ServerPause(host, reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pauses = append(o.pauses, host+"/"+reason)
}

func (o *waitObs) Response(host, class string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.responses = append(o.responses, host+"/"+class)
}

func (o *waitObs) DeadlineRefusal(host string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.refusals = append(o.refusals, host)
}

func (o *waitObs) RateLimitWait(host string, d time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.hosts = append(o.hosts, host)
	o.waits = append(o.waits, d)
}

// The observer sees one wait per attempt: zero inside the burst, the real
// wait once the bucket is empty.
func TestObserverRateLimitWait(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	obs := &waitObs{}
	one := Limit{Rate: 1, Burst: 1}
	c, _ := newTestClient(t, func(cfg *Config) {
		cfg.FallbackLimit = HostLimit{Anonymous: one, Authenticated: one}
		cfg.Observer = obs
	})
	for range 2 {
		if _, err := Fetch(t.Context(), c.HTTPClient(), get(t, srv.URL), Validators{}); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
	}
	if len(obs.waits) != 2 {
		t.Fatalf("observed %d waits, want 2: %v", len(obs.waits), obs.waits)
	}
	if obs.hosts[0] != OtherHost {
		t.Errorf("host = %q, want %q: an unlisted host must not reach the observer", obs.hosts[0], OtherHost)
	}
	if obs.waits[0] != 0 {
		t.Errorf("first wait = %v, want 0 (inside the burst)", obs.waits[0])
	}
	if obs.waits[1] != time.Second {
		t.Errorf("second wait = %v, want 1s", obs.waits[1])
	}
}

// A request refused by the SSRF guard never reaches the limiter, so nothing
// is observed.
func TestObserverNotCalledWhenGuardRefuses(t *testing.T) {
	obs := &waitObs{}
	c, _ := newTestClient(t, func(cfg *Config) {
		cfg.Allow = nil
		cfg.Observer = obs
	})
	if _, err := Fetch(t.Context(), c.HTTPClient(), get(t, "http://127.0.0.1:1/"), Validators{}); err == nil {
		t.Fatal("request to loopback was allowed")
	}
	if len(obs.waits) != 0 {
		t.Fatalf("observed %v for a refused request", obs.waits)
	}
}

// Hosts of the limits table (defaults and Config.HostLimits) are reported by
// name, in lower case; every other host is "other".
func TestObserverHostsFromLimitsTableOnly(t *testing.T) {
	l := newLimiter(newFakeClock(true), DefaultFallbackLimit(), map[string]HostLimit{
		"api.github.com": DefaultHostLimits()["api.github.com"],
		"mirror.corp":    DefaultFallbackLimit(),
	})
	for host, want := range map[string]string{
		"API.GitHub.com":         "api.github.com",
		"mirror.corp.":           "mirror.corp",
		"abc123.m.pipedream.net": OtherHost,
		"hooks.example":          OtherHost,
	} {
		if got := l.observedHost(host); got != want {
			t.Errorf("observedHost(%q) = %q, want %q", host, got, want)
		}
	}
}
