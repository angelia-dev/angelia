package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const sseBody = "retry: 5000\n\n" +
	"id: 4182\ndata: {\"seq\":4182,\"ref_event_id\":null,\"event\":{\"type\":\"dev.angelia.release.published.v1\",\"id\":\"01JBX2A7C8D9E0F1G2H3J4K5M6\",\"time\":\"2026-10-30T17:00:02.512Z\",\"data\":{\"package\":\"requests\",\"version\":\"2.33.0\",\"watch\":{\"name\":\"requests\"}}}}\n\n" +
	": keep-alive\n\n" +
	"id: 4190\ndata: {\"seq\":4190,\"ref_event_id\":\"01JBX2A7C8D9E0F1G2H3J4K5M6\",\"event\":{\"type\":\"dev.angelia.release.yanked.v1\",\"id\":\"01JBXC4D5E6F7G8H9J0K1M2N3P\",\"time\":\"2026-11-02T09:30:01.207Z\",\"data\":{\"package\":\"requests\",\"version\":\"2.33.0\",\"watch\":{\"name\":\"requests\"}}}}\n\n"

func newSSEServer(t *testing.T) (*httptest.Server, *atomic.Value) {
	t.Helper()
	var lastURI atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastURI.Store(r.URL.RequestURI() + " auth=" + r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, sseBody)
	}))
	t.Cleanup(srv.Close)
	return srv, &lastURI
}

func TestEventStreamTable(t *testing.T) {
	isolate(t)
	srv, uri := newSSEServer(t)
	out, _, err := runCLI(t, "event", "stream", "--no-reconnect", "--server", srv.URL, "--token", testToken,
		"--after", "10", "--watch", "requests", "--type", "release.published", "--type", "release.yanked", "--source", "pypi")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "event-stream.table", out)
	got := uri.Load().(string)
	for _, want := range []string{"after=10", "watch=requests", "type=release.published", "type=release.yanked", "source=pypi", "auth=Bearer " + testToken} {
		if !strings.Contains(got, want) {
			t.Errorf("request %q lacks %q", got, want)
		}
	}
}

func TestEventStreamJSON(t *testing.T) {
	isolate(t)
	srv, _ := newSSEServer(t)
	out, _, err := runCLI(t, "event", "stream", "--no-reconnect", "-o", "json", "--server", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "event-stream.json", out)
	if lines := strings.Count(strings.TrimSpace(out), "\n") + 1; lines != 2 {
		t.Errorf("want one JSON line per event, got %d lines", lines)
	}
}

func TestEventStreamRefused(t *testing.T) {
	isolate(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusGone)
		_, _ = fmt.Fprint(w, `{"type":"https://github.com/angelia-dev/spec/blob/main/docs/api.md#problem-cursor-expired","title":"Cursor expired","status":410,"oldest_seq":4000}`)
	}))
	defer srv.Close()
	_, _, err := runCLI(t, "event", "stream", "--server", srv.URL, "--after", "5")
	if err == nil || !strings.Contains(err.Error(), "410") || !strings.Contains(err.Error(), "cursor-expired") {
		t.Fatalf("err = %v", err)
	}
}

func TestCompletion(t *testing.T) {
	isolate(t)
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		out, _, err := runCLI(t, "completion", shell)
		if err != nil {
			t.Fatalf("completion %s: %v", shell, err)
		}
		if !strings.Contains(out, "angelia") || len(out) < 500 {
			t.Errorf("completion %s: implausible script (%d bytes)", shell, len(out))
		}
	}
	// The dynamic completion the scripts call.
	out, _, err := runCLI(t, "__complete", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"watch", "sink", "sub", "event", "delivery", "completion"} {
		if !strings.Contains(out, c+"\t") && !strings.Contains(out, c+"\n") {
			t.Errorf("top-level completion lacks %q:\n%s", c, out)
		}
	}
	out, _, _ = runCLI(t, "__complete", "event", "")
	for _, c := range []string{"list", "get", "stream"} {
		if !strings.Contains(out, c+"\t") {
			t.Errorf("event completion lacks %q:\n%s", c, out)
		}
	}
	out, _, _ = runCLI(t, "__complete", "watch", "list", "-o", "")
	if !strings.Contains(out, "json") || !strings.Contains(out, "table") {
		t.Errorf("-o completion:\n%s", out)
	}
	out, _, _ = runCLI(t, "__complete", "delivery", "list", "--status", "")
	if !strings.Contains(out, "dead") {
		t.Errorf("--status completion:\n%s", out)
	}
}
