package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

func TestServeRolesFlagAndGracefulExit(t *testing.T) {
	t.Setenv("ANGELIA_CONFIG", "")
	addr := freeAddr(t)
	cfg := writeCfg(t, fmt.Sprintf("listen: %q\ndatabase:\n  dsn: sqlite://%s\n", addr, filepath.Join(t.TempDir(), "a.db")))
	var errb syncBuf
	root := newRootCmd()
	root.SetOut(&errb)
	root.SetErr(&errb)
	root.SetArgs([]string{"serve", "--config", cfg, "--roles=worker,dispatcher", "--drain-timeout=2s"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()

	ok := false
	for range 100 {
		if resp, err := http.Get("http://" + addr + "/readyz"); err == nil {
			_ = resp.Body.Close()
			ok = resp.StatusCode == http.StatusOK
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ok {
		t.Fatal("server did not become ready")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("want clean exit, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not exit")
	}
	out := errb.String()
	for _, r := range []string{"worker", "dispatcher"} {
		if !strings.Contains(out, `"role":"`+r+`"`) {
			t.Errorf("missing log line for %s: %s", r, out)
		}
	}
	for _, r := range []string{"api", "scheduler"} {
		if strings.Contains(out, `"role":"`+r+`"`) {
			t.Errorf("unexpected role %s started", r)
		}
	}
}

func TestServeRejectsUnknownRole(t *testing.T) {
	t.Setenv("ANGELIA_CONFIG", "")
	root := newRootCmd()
	root.SetOut(&syncBuf{})
	root.SetErr(&syncBuf{})
	root.SetArgs([]string{"serve", "--roles=bogus"})
	if err := root.ExecuteContext(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestServeInvalidConfig(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&syncBuf{})
	root.SetErr(&syncBuf{})
	root.SetArgs([]string{"serve", "--config", writeCfg(t, "bogus: 1\n")})
	if err := root.ExecuteContext(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}
