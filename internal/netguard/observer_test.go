package netguard

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type waitObs struct {
	mu    sync.Mutex
	hosts []string
	waits []time.Duration
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
	if obs.hosts[0] != "127.0.0.1" {
		t.Errorf("host = %q, want 127.0.0.1", obs.hosts[0])
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
