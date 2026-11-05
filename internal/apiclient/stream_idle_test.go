package apiclient

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A CR and its LF may arrive in different reads.
func TestStreamSplitCRLF(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "id: 1\r")
		f.Flush()
		time.Sleep(50 * time.Millisecond)
		_, _ = fmt.Fprint(w, "\ndata: {\"seq\":1,\"ref_event_id\":null,\"event\":{}}\r")
		f.Flush()
		time.Sleep(50 * time.Millisecond)
		_, _ = fmt.Fprint(w, "\n\r")
		f.Flush()
		time.Sleep(50 * time.Millisecond)
		_, _ = fmt.Fprint(w, "\n")
	}))
	defer srv.Close()
	a, _ := New(Options{Server: srv.URL})
	var seqs []int64
	if err := a.Stream(t.Context(), StreamEventsParams{}, StreamOptions{}, func(r EventRecord) error { seqs = append(seqs, r.Seq); return nil }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(seqs) != "[1]" {
		t.Errorf("seqs = %v", seqs)
	}
}

// A server that accepts the connection but never answers is an idle one.
func TestStreamIdleBeforeHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()
	a, _ := New(Options{Server: srv.URL})
	start := time.Now()
	err := a.Stream(t.Context(), StreamEventsParams{}, StreamOptions{IdleTimeout: 100 * time.Millisecond}, func(EventRecord) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "no data for") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Error("took too long")
	}
}

// Time spent in the callback is not idle time.
func TestStreamSlowCallbackIsNotIdle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, record(1)+record(2))
	}))
	defer srv.Close()
	a, _ := New(Options{Server: srv.URL})
	var seqs []int64
	err := a.Stream(t.Context(), StreamEventsParams{}, StreamOptions{IdleTimeout: 100 * time.Millisecond}, func(r EventRecord) error {
		seqs = append(seqs, r.Seq)
		time.Sleep(250 * time.Millisecond)
		return nil
	})
	if err != nil || fmt.Sprint(seqs) != "[1 2]" {
		t.Errorf("err %v, seqs %v", err, seqs)
	}
}

func TestRedirectsAreRefused(t *testing.T) {
	var followed atomic.Int32
	dst := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed.Add(1) }))
	defer dst.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dst.URL+r.URL.Path, http.StatusTemporaryRedirect) //nolint:gosec // test server
	}))
	defer srv.Close()
	a, _ := New(Options{Server: srv.URL, Token: "tok-secret"})

	resp, err := a.ListWatches(t.Context(), &ListWatchesParams{}) //nolint:bodyclose // Decode closes it
	_, err = Decode(resp, err, new(WatchPage))
	if err == nil || !strings.Contains(err.Error(), "redirect") || strings.Contains(err.Error(), "tok-secret") {
		t.Errorf("request: err = %v", err)
	}
	err = a.Stream(t.Context(), StreamEventsParams{}, StreamOptions{}, func(EventRecord) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("stream: err = %v", err)
	}
	if followed.Load() != 0 {
		t.Error("the redirect was followed")
	}
}
