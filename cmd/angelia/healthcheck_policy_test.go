package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// AC (review S1): a redirect is not followed. A 3xx answer is not healthy,
// even when the place it points to answers 200.
func TestHealthcheckDoesNotFollowRedirects(t *testing.T) {
	var target atomic.Int32
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		target.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer dst.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dst.URL+"/healthz", http.StatusFound)
	}))
	defer src.Close()

	err := checkHealthz(context.Background(), src.URL+"/healthz", time.Second)
	if err == nil {
		t.Fatal("want an error for a 302 answer, got nil")
	}
	if n := target.Load(); n != 0 {
		t.Fatalf("the redirect target was requested %d times", n)
	}
}

// AC (review S1): the probe does not go through an HTTP proxy from the
// environment. The URL uses a name that never resolves, so only a proxy
// could answer it.
func TestHealthcheckIgnoresHTTPProxyEnvironment(t *testing.T) {
	var hits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)

	if err := checkHealthz(context.Background(), "http://angelia-probe.invalid:8080/healthz", time.Second); err == nil {
		t.Fatal("want an error: the host does not resolve and no proxy may answer")
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the proxy received %d requests", n)
	}
}

// AC (review S2): a URL with credentials is refused, and the secret is never printed.
func TestHealthcheckRejectsCredentialsInURLWithoutEchoingThem(t *testing.T) {
	const secret = "s3cret-pass"
	err := runHealthcheck(t, "--url", "http://probe:"+secret+"@127.0.0.1:1/healthz")
	if err == nil {
		t.Fatal("want an error for a URL with credentials")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "probe:") {
		t.Fatalf("error echoes the credentials: %v", err)
	}
}

// AC (review S3): the default probe follows ANGELIA_LISTEN.
func TestHealthcheckURLFollowsTheListenAddress(t *testing.T) {
	cases := map[string]string{
		"":                "http://127.0.0.1:8080/healthz",
		":9000":           "http://127.0.0.1:9000/healthz",
		"0.0.0.0:9000":    "http://127.0.0.1:9000/healthz",
		"[::]:9000":       "http://127.0.0.1:9000/healthz",
		"10.1.2.3:9000":   "http://10.1.2.3:9000/healthz",
		"[::1]:9000":      "http://[::1]:9000/healthz",
		"not-an-address":  "http://127.0.0.1:8080/healthz",
		"localhost:18080": "http://localhost:18080/healthz",
	}
	for listen, want := range cases {
		if got := healthcheckURL(listen); got != want {
			t.Errorf("healthcheckURL(%q) = %q, want %q", listen, got, want)
		}
	}
}

func TestHealthcheckFlagDefaultReadsANGELIA_LISTEN(t *testing.T) {
	t.Setenv("ANGELIA_LISTEN", ":9123")
	got, err := newHealthcheckCmd().Flags().GetString("url")
	if err != nil {
		t.Fatal(err)
	}
	if want := "http://127.0.0.1:9123/healthz"; got != want {
		t.Fatalf("default --url with ANGELIA_LISTEN=:9123 is %q, want %q", got, want)
	}
}
