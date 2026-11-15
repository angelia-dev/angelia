package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/apitoken"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
)

// TestServeAuthenticatesAPITokens: serve's api role accepts a stored API
// token (no more DenyAll), logs its name in the access log, refuses an
// unknown one with the failure reason, and writes the token's last use on
// shutdown (the final flush of ADR 0030 T6). Until bootstrap (72-E) lands,
// the first admin token goes straight into the store.
func TestServeAuthenticatesAPITokens(t *testing.T) {
	t.Setenv("ANGELIA_CONFIG", "")
	dbPath := filepath.Join(t.TempDir(), "a.db")
	st, err := sqlite.Open(t.Context(), dbPath, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	id, tok := apitoken.Generate(created)
	if err := st.CreateAPIToken(t.Context(), store.APIToken{
		ID: id, Name: "admin", Scope: store.TokenScopeAdmin, Hash: apitoken.Hash(tok), CreatedAt: created, CreatedBy: "local",
	}); err != nil {
		t.Fatal(err)
	}
	_, unknown := apitoken.Generate(created)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	addr := freeAddr(t)
	cfg := writeCfg(t, fmt.Sprintf("listen: %q\ndatabase:\n  dsn: sqlite://%s\n", addr, dbPath))
	var logs syncBuf
	root := newRootCmd()
	root.SetOut(&logs)
	root.SetErr(&logs)
	root.SetArgs([]string{"serve", "--config", cfg, "--roles=api", "--drain-timeout=2s"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()

	get := func(token string) int {
		req, _ := http.NewRequestWithContext(t.Context(), "GET", "http://"+addr+"/v1/watches", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	status := 0
	for range 100 {
		if status = get(tok); status != 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status != http.StatusOK {
		t.Fatalf("GET /v1/watches with the admin token: %d\n%s", status, logs.String())
	}
	if s := get(unknown); s != http.StatusUnauthorized {
		t.Fatalf("unknown token: %d", s)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not exit")
	}
	out := logs.String()
	for _, want := range []string{`"token_name":"admin"`, `"principal":"` + id + `"`, `"auth_failure":"unknown"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, tok) || strings.Contains(out, unknown) {
		t.Error("a token reached the log")
	}

	st, err = sqlite.Open(context.Background(), dbPath, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	row, err := st.APITokenByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.LastUsedAt == nil || row.LastUsedAt.Before(created) {
		t.Fatalf("last_used_at = %v: the final flush did not run", row.LastUsedAt)
	}
}
