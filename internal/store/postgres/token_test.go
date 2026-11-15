package postgres

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// TestTokenWritesTakeNoConfigLock: a token write is one statement outside the
// config lock (ADR 0030 T3, T6), so a touch, a create and a revoke all finish
// while another session holds the config lock in an open transaction.
func TestTokenWritesTakeNoConfigLock(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	tok := store.APIToken{ID: "01TOKENA", Name: "a", Scope: "admin", Hash: bytes.Repeat([]byte{1}, 32), CreatedAt: t0, CreatedBy: "local"}
	must(t, s.CreateAPIToken(ctx, tok))

	locked, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.InConfigTx(ctx, func(store.ConfigTx) error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	defer func() {
		close(release)
		must(t, <-done)
	}()

	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	must(t, s.TouchAPITokens(wctx, map[string]time.Time{tok.ID: t0.Add(time.Hour)}))
	b := tok
	b.ID, b.Name = "01TOKENB", "b"
	must(t, s.CreateAPIToken(wctx, b))
	ok, err := s.RevokeAPIToken(wctx, b.ID, t0, "local")
	must(t, err)
	if !ok {
		t.Fatal("revoke reported false")
	}
	got, err := s.APITokenByID(wctx, tok.ID)
	must(t, err)
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("last use = %v", got.LastUsedAt)
	}
}
