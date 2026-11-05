package apiclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStreamRetriesServerErrors(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(status)
					return
				}
				_, _ = fmt.Fprint(w, record(9))
			}))
			defer srv.Close()
			a, _ := New(Options{Server: srv.URL})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var waits []time.Duration
			var causes []error
			start := time.Now()
			err := a.Stream(ctx, StreamEventsParams{}, StreamOptions{Reconnect: true, OnReconnect: func(err error, wait time.Duration) {
				causes, waits = append(causes, err), append(waits, wait)
			}}, func(EventRecord) error { cancel(); return nil })
			if err != nil {
				t.Fatal(err)
			}
			var ae *Error
			if len(waits) < 1 || waits[0] != time.Second || !errors.As(causes[0], &ae) || ae.Status != status {
				t.Errorf("OnReconnect: waits %v causes %v", waits, causes)
			}
			if time.Since(start) < 900*time.Millisecond {
				t.Error("Retry-After was not honoured")
			}
		})
	}
}

func TestStreamServerErrorWithoutReconnectReturns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	a, _ := New(Options{Server: srv.URL})
	err := a.Stream(t.Context(), StreamEventsParams{}, StreamOptions{}, func(EventRecord) error { return nil })
	var ae *Error
	if !errors.As(err, &ae) || ae.Status != 503 {
		t.Errorf("err = %v", err)
	}
}

func TestStreamReconnectsAfterConnectionError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, record(1)) }))
	addr := srv.URL
	srv.Close() // nothing listens: connection refused
	a, _ := New(Options{Server: addr})
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	var n atomic.Int32
	err := a.Stream(ctx, StreamEventsParams{}, StreamOptions{
		Reconnect: true, RetryDelay: 20 * time.Millisecond,
		OnReconnect: func(err error, _ time.Duration) {
			if err == nil {
				t.Error("nil cause for a connection error")
			}
			n.Add(1)
		},
	}, func(EventRecord) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if n.Load() < 2 {
		t.Errorf("%d reconnects", n.Load())
	}
}

func TestStreamOversizedLineStops(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = fmt.Fprint(w, "id: 1\ndata: "+strings.Repeat("x", maxStreamLine+10)+"\n\n")
	}))
	defer srv.Close()
	a, _ := New(Options{Server: srv.URL})
	err := a.Stream(t.Context(), StreamEventsParams{}, StreamOptions{Reconnect: true, RetryDelay: time.Millisecond}, func(EventRecord) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("%d requests", calls.Load())
	}
}

func TestStreamIdleTimeoutReconnects(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			_, _ = fmt.Fprint(w, record(1))
			w.(http.Flusher).Flush()
			<-r.Context().Done() // a dead connection: silence
			return
		}
		_, _ = fmt.Fprint(w, record(2))
	}))
	defer srv.Close()
	a, _ := New(Options{Server: srv.URL})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var seqs []int64
	var cause atomic.Value
	err := a.Stream(ctx, StreamEventsParams{}, StreamOptions{
		Reconnect: true, RetryDelay: time.Millisecond, IdleTimeout: 100 * time.Millisecond,
		OnReconnect: func(err error, _ time.Duration) {
			if err != nil {
				cause.CompareAndSwap(nil, err.Error())
			}
		},
	}, func(r EventRecord) error {
		seqs = append(seqs, r.Seq)
		if r.Seq == 2 {
			cancel()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(seqs) != "[1 2]" {
		t.Errorf("seqs = %v", seqs)
	}
	if c, _ := cause.Load().(string); !strings.Contains(c, "no data for") {
		t.Errorf("cause = %q", c)
	}
}

func TestKeepAliveResetsIdleTimer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for range 6 {
			_, _ = fmt.Fprint(w, ": keep-alive\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(20 * time.Millisecond)
		}
		_, _ = fmt.Fprint(w, record(1))
	}))
	defer srv.Close()
	a, _ := New(Options{Server: srv.URL})
	var got int
	err := a.Stream(t.Context(), StreamEventsParams{}, StreamOptions{IdleTimeout: 300 * time.Millisecond}, func(EventRecord) error { got++; return nil })
	if err != nil || got != 1 {
		t.Errorf("err %v, %d events", err, got)
	}
}

func TestRetryClamp(t *testing.T) {
	for in, want := range map[time.Duration]time.Duration{
		time.Millisecond: minRetry, time.Second: time.Second, 11 * 24 * time.Hour: maxRetry,
	} {
		if got := clampRetry(in); got != want {
			t.Errorf("clampRetry(%v) = %v, want %v", in, got, want)
		}
	}
	if got := parseRetryAfter("999999999"); got != maxRetry {
		t.Errorf("Retry-After clamp = %v", got)
	}
	if parseRetryAfter("soon") != 0 || parseRetryAfter("-3") != 0 {
		t.Error("bad Retry-After accepted")
	}
}

func TestStreamHugeRetryHintIsClamped(t *testing.T) {
	var mu sync.Mutex
	var waits []time.Duration
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = fmt.Fprint(w, "retry: 999999999999999\n\n")
		}
	}))
	defer srv.Close()
	a, _ := New(Options{Server: srv.URL})
	ctx, cancel := context.WithCancel(t.Context())
	_ = a.Stream(ctx, StreamEventsParams{}, StreamOptions{Reconnect: true, OnReconnect: func(_ error, w time.Duration) {
		mu.Lock()
		waits = append(waits, w)
		mu.Unlock()
		cancel()
	}}, func(EventRecord) error { return nil })
	mu.Lock()
	defer mu.Unlock()
	if len(waits) != 1 || waits[0] != maxRetry {
		t.Errorf("waits = %v", waits)
	}
}

func TestStreamLineEndings(t *testing.T) {
	body := "id: 1\r\ndata: {\"seq\":1,\"ref_event_id\":null,\"event\":{}}\r\n\r\n" +
		"id: 2\rdata: {\"seq\":2,\"ref_event_id\":null,\"event\":{}}\r\r" +
		"id: 3\ndata: {\"seq\":3,\"ref_event_id\":null,\"event\":{}}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, body) }))
	defer srv.Close()
	a, _ := New(Options{Server: srv.URL})
	var seqs []int64
	if err := a.Stream(t.Context(), StreamEventsParams{}, StreamOptions{}, func(r EventRecord) error { seqs = append(seqs, r.Seq); return nil }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(seqs) != "[1 2 3]" {
		t.Errorf("seqs = %v", seqs)
	}
}

// A caller's client with a Timeout must not cut streams.
func TestCallerTimeoutDoesNotCutStreams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, record(1))
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond)
		_, _ = fmt.Fprint(w, record(2))
	}))
	defer srv.Close()
	a, _ := New(Options{Server: srv.URL, HTTPClient: &http.Client{Timeout: 100 * time.Millisecond}})
	var got int
	if err := a.Stream(t.Context(), StreamEventsParams{}, StreamOptions{}, func(EventRecord) error { got++; return nil }); err != nil || got != 2 {
		t.Errorf("err %v, %d events", err, got)
	}
}
