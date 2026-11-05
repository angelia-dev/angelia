package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAllStopsOnRepeatedCursor(t *testing.T) {
	isolate(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"items":[],"next_cursor":"same"}`)
	}))
	defer srv.Close()
	_, _, err := runCLI(t, "sink", "list", "--all", "--server", srv.URL)
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("err = %v", err)
	}
	if n := calls.Load(); n > 3 {
		t.Errorf("%d requests", n)
	}
}

func TestEventListAllStopsWhenNextAfterStalls(t *testing.T) {
	isolate(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"items":[],"next_after":5,"has_more":true}`)
	}))
	defer srv.Close()
	_, _, err := runCLI(t, "event", "list", "--all", "--after", "5", "--server", srv.URL)
	if err == nil || !strings.Contains(err.Error(), "next_after") {
		t.Fatalf("err = %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("%d requests", n)
	}
}

func TestEventStreamReportsReconnect(t *testing.T) {
	isolate(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "retry: 100\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"type":"https://example.test/api.md#problem-unauthorized","title":"Unauthorized","status":401}`)
	}))
	defer srv.Close()
	_, errOut, err := runCLI(t, "event", "stream", "--server", srv.URL)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
	if strings.Count(errOut, "reconnecting in 100ms") != 1 {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestEventStreamTableTruncatesLongNames(t *testing.T) {
	isolate(t)
	long := strings.Repeat("p", 80)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "id: 1\ndata: {\"seq\":1,\"ref_event_id\":null,\"event\":{\"type\":\"dev.angelia.release.published.v1\",\"id\":\"E\",\"time\":\"T\",\"data\":{\"package\":%q,\"version\":\"1\",\"watch\":{\"name\":%q}}}}\n\n", long, long)
	}))
	defer srv.Close()
	out, _, err := runCLI(t, "event", "stream", "--no-reconnect", "--server", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, long) || !strings.Contains(out, "…") {
		t.Errorf("not truncated:\n%s", out)
	}
	if row := strings.Split(strings.TrimSpace(out), "\n")[1]; !strings.HasSuffix(row, "1                 T") {
		t.Errorf("columns lost: %q", row)
	}
}
