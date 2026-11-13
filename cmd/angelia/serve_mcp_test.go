package main

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestServeMountsMCPWithAPIRole: /mcp is served by the api role only, and
// with the api role it takes the same authentication as /v1/ (DenyAll until
// API tokens exist, so 401 with the challenge).
func TestServeMountsMCPWithAPIRole(t *testing.T) {
	for _, tc := range []struct {
		roles string
		want  int
	}{{"api", http.StatusUnauthorized}, {"worker", http.StatusNotFound}} {
		t.Run(tc.roles, func(t *testing.T) {
			t.Setenv("ANGELIA_CONFIG", "")
			addr := freeAddr(t)
			cfg := writeCfg(t, fmt.Sprintf("listen: %q\ndatabase:\n  dsn: sqlite://%s\n", addr, filepath.Join(t.TempDir(), "a.db")))
			root := newRootCmd()
			root.SetOut(&syncBuf{})
			root.SetErr(&syncBuf{})
			root.SetArgs([]string{"serve", "--config", cfg, "--roles=" + tc.roles, "--drain-timeout=2s"})
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- root.ExecuteContext(ctx) }()
			defer func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("serve: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Error("serve did not exit")
				}
			}()

			var resp *http.Response
			for range 100 {
				req, _ := http.NewRequestWithContext(t.Context(), "POST", "http://"+addr+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
				req.Header.Set("Content-Type", "application/json")
				r, err := http.DefaultClient.Do(req)
				if err == nil {
					resp = r
					_ = r.Body.Close()
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if resp == nil {
				t.Fatal("server did not start")
			}
			if resp.StatusCode != tc.want {
				t.Fatalf("POST /mcp: %d, want %d", resp.StatusCode, tc.want)
			}
			if tc.want == http.StatusUnauthorized && resp.Header.Get("WWW-Authenticate") != `Bearer realm="angelia"` {
				t.Errorf("challenge %q", resp.Header.Get("WWW-Authenticate"))
			}
		})
	}
}
