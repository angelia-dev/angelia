package netguard

import (
	"net/http"
	"testing"
	"time"
)

func TestIsCredentialHeader(t *testing.T) {
	for _, n := range []string{"Authorization", "authorization", "PRIVATE-TOKEN", "Private-Token"} {
		if !IsCredentialHeader(n) {
			t.Errorf("IsCredentialHeader(%q) = false", n)
		}
	}
	for _, n := range []string{"X-Api-Key", "Accept"} {
		if IsCredentialHeader(n) {
			t.Errorf("IsCredentialHeader(%q) = true", n)
		}
	}
}

func TestRetryAtRemainingForms(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	for _, tc := range []struct {
		remaining string
		want      bool
	}{{"0", true}, {"-1", true}, {"0;w=60", true}, {" 0 ", true}, {"5", false}, {"abc", false}, {"", false}} {
		h := http.Header{}
		h.Set("RateLimit-Remaining", tc.remaining)
		h.Set("RateLimit-Reset", "1000050")
		if got := !retryAt(h, now).IsZero(); got != tc.want {
			t.Errorf("Remaining %q: paused = %v, want %v", tc.remaining, got, tc.want)
		}
	}
	// A delta-seconds Reset is in the past as a timestamp: ignored.
	h := http.Header{}
	h.Set("RateLimit-Remaining", "0")
	h.Set("RateLimit-Reset", "30")
	if got := retryAt(h, now); !got.IsZero() {
		t.Errorf("delta Reset gave %v, want zero", got)
	}
}

func TestExtraAuthHeadersCloned(t *testing.T) {
	extra := []string{"X-Custom-Key"}
	c, _ := newTestClient(t, func(cfg *Config) { cfg.ExtraAuthHeaders = extra })
	extra[0] = "X-Other"
	h := http.Header{}
	h.Set("X-Custom-Key", "k")
	if !c.isAuthenticated(h) {
		t.Error("caller mutation of ExtraAuthHeaders leaked into the client")
	}
}

func TestClientIsCredentialHeader(t *testing.T) {
	c, _ := newTestClient(t, func(cfg *Config) { cfg.ExtraAuthHeaders = []string{"X-Api-Key"} })
	for _, n := range []string{"Authorization", "private-token", "X-Api-Key", "x-api-key"} {
		if !c.IsCredentialHeader(n) {
			t.Errorf("IsCredentialHeader(%q) = false", n)
		}
	}
	if c.IsCredentialHeader("Accept") {
		t.Error("IsCredentialHeader(Accept) = true")
	}
}
