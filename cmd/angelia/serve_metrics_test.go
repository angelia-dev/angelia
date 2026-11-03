package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// serveAndGet runs `serve` with args and returns the status and body of GET path.
func serveAndGet(t *testing.T, path string, args ...string) (int, string) {
	t.Helper()
	t.Setenv("ANGELIA_CONFIG", "")
	addr := freeAddr(t)
	cfg := writeCfg(t, fmt.Sprintf("listen: %q\n", addr))
	root := newRootCmd()
	root.SetOut(&syncBuf{})
	root.SetErr(&syncBuf{})
	root.SetArgs(append([]string{"serve", "--config", cfg, "--roles=worker", "--drain-timeout=2s"}, args...))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			_ = err
		case <-time.After(5 * time.Second):
			t.Error("serve did not exit")
		}
	}()
	for range 100 {
		resp, err := http.Get("http://" + addr + path)
		if err == nil {
			defer func() { _ = resp.Body.Close() }()
			b, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, string(b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server did not start")
	return 0, ""
}

// /metrics is served on the listen address by default.
func TestServeMetrics(t *testing.T) {
	code, body := serveAndGet(t, "/metrics")
	if code != http.StatusOK || !strings.Contains(body, "angelia_build_info{") {
		t.Fatalf("GET /metrics = %d\n%s", code, body)
	}
}

func TestServeMetricsDisabled(t *testing.T) {
	if code, _ := serveAndGet(t, "/metrics", "--metrics=false"); code != http.StatusNotFound {
		t.Fatalf("GET /metrics with --metrics=false = %d, want 404", code)
	}
}
