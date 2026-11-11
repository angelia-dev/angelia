package sqlite

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

func testSecret(id, name, keyID string) store.Secret {
	at := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	return store.Secret{ID: id, Name: name, KeyID: keyID, Ciphertext: []byte{1, 2, 3}, CreatedAt: at, UpdatedAt: at}
}

func TestSecretHostsMustBeAnArray(t *testing.T) {
	s, _ := openTest(t)
	_, err := s.db.ExecContext(t.Context(), `INSERT INTO secret (id, name, hosts_json, key_id, ciphertext, created_at, updated_at)
		VALUES ('i', 'n', '{"a":1}', 'k', x'00', 1, 1)`)
	if err == nil {
		t.Fatal("hosts_json that is not an array was stored")
	}
}

func TestSecretKeyCounts(t *testing.T) {
	ctx := t.Context()
	if _, err := SecretKeyCounts(ctx, filepath.Join(t.TempDir(), "missing.db")); !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("missing file: %v", err)
	}

	// A database before the secret migration has no secret.
	old := filepath.Join(t.TempDir(), "old.db")
	db, err := openDB(old)
	must(t, err)
	_, err = migrate(ctx, db, discard(), 14)
	must(t, err)
	must(t, db.Close())
	counts, err := SecretKeyCounts(ctx, old)
	if err != nil || len(counts) != 0 {
		t.Fatalf("old schema: %v %v", counts, err)
	}

	s, path := openTest(t)
	must(t, s.InSecretTx(ctx, func(tx store.SecretTx) error {
		return tx.CreateSecret(ctx, testSecret("01SECRETA", "a", "aaaaaaaaaaaaaaaa"))
	}))
	counts, err = SecretKeyCounts(ctx, path)
	if err != nil || counts["aaaaaaaaaaaaaaaa"] != 1 {
		t.Fatalf("counts: %v %v", counts, err)
	}

	// Read-only: no file is created next to a missing database, and the
	// check never migrates.
	info, err := os.Stat(old)
	must(t, err)
	if _, err := SecretKeyCounts(ctx, old); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(old)
	must(t, err)
	if !after.ModTime().Equal(info.ModTime()) || after.Size() != info.Size() {
		t.Fatal("SecretKeyCounts changed the database file")
	}
}
