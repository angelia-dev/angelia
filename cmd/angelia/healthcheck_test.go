package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// runHealthcheck runs `angelia healthcheck` with the given arguments and
// returns the error of the command (nil means exit status 0).
func runHealthcheck(t *testing.T, args ...string) error {
	t.Helper()
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(append([]string{"healthcheck"}, args...))
	return root.Execute()
}

// AC: the image healthcheck runs `angelia healthcheck` against /healthz.
func TestHealthcheckDefaultURLIsTheListenPortOnLoopback(t *testing.T) {
	cmd := newHealthcheckCmd()
	got, err := cmd.Flags().GetString("url")
	if err != nil {
		t.Fatal(err)
	}
	if want := "http://127.0.0.1:8080/healthz"; got != want {
		t.Fatalf("default --url = %q, want %q", got, want)
	}
}

// AC: exits 0 on a 2xx answer.
func TestHealthcheckExitsZeroOn2xx(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusNoContent} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		err := runHealthcheck(t, "--url", srv.URL+"/healthz")
		srv.Close()
		if err != nil {
			t.Errorf("status %d: want exit 0, got %v", code, err)
		}
	}
}

// AC: exits 1 on any other answer, such as 503 while the instance is not ready.
func TestHealthcheckExitsOneOnNon2xx(t *testing.T) {
	for _, code := range []int{http.StatusServiceUnavailable, http.StatusInternalServerError, http.StatusNotFound} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		err := runHealthcheck(t, "--url", srv.URL+"/healthz")
		srv.Close()
		if err == nil {
			t.Errorf("status %d: want exit 1, got 0", code)
		}
	}
}

// AC: exits 1 when nothing listens (connection refused).
func TestHealthcheckExitsOneOnConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runHealthcheck(t, "--url", "http://"+addr+"/healthz"); err == nil {
		t.Fatal("want exit 1 for a refused connection, got 0")
	}
}

// AC: exits 1 when the answer takes longer than the timeout.
func TestHealthcheckExitsOneOnTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(release)

	start := time.Now()
	err := checkHealthz(context.Background(), srv.URL+"/healthz", 100*time.Millisecond)
	if err == nil {
		t.Fatal("want an error for a slow answer, got nil")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the check ran %v, the timeout is not applied", elapsed)
	}
}

// The probe is for the local instance only: a URL that is not http(s) is a usage error.
func TestHealthcheckRejectsNonHTTPURL(t *testing.T) {
	for _, u := range []string{"ftp://127.0.0.1:8080/healthz", "127.0.0.1:8080/healthz", "http:///healthz"} {
		if err := runHealthcheck(t, "--url", u); err == nil {
			t.Errorf("--url %q: want an error", u)
		}
	}
}

func TestHealthcheckRejectsArgs(t *testing.T) {
	err := runHealthcheck(t, "extra")
	if err == nil || (!strings.Contains(err.Error(), "unknown command") && !strings.Contains(err.Error(), "accepts")) {
		t.Fatalf("want an argument error, got %v", err)
	}
}
