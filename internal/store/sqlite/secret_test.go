package sqlite

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

func testSecret(id, name, keyID string) store.Secret {
	at := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	return store.Secret{ID: id, Name: name, KeyID: keyID, Ciphertext: []byte{1, 2, 3}, CreatedAt: at, UpdatedAt: at}
}

func TestSecretCRUD(t *testing.T) {
	ctx := t.Context()
	s, _ := openTest(t)

	a := testSecret("01SECRETA", "ci-webhook", "aaaaaaaaaaaaaaaa")
	a.Hosts = []string{"ci.example.com", "ci.example.com:8443"}
	must(t, s.InSecretTx(ctx, func(tx store.SecretTx) error { return tx.CreateSecret(ctx, a) }))
	must(t, s.InSecretTx(ctx, func(tx store.SecretTx) error {
		return tx.CreateSecret(ctx, testSecret("01SECRETB", "b-token", "bbbbbbbbbbbbbbbb"))
	}))

	got, err := s.SecretByName(ctx, "ci-webhook")
	must(t, err)
	a.Revision = 1
	if !reflect.DeepEqual(got, a) {
		t.Fatalf("SecretByName = %+v, want %+v", got, a)
	}
	list, err := s.ListSecrets(ctx)
	must(t, err)
	if len(list) != 2 || list[0].Name != "b-token" || list[1].Name != "ci-webhook" || list[0].Hosts == nil {
		t.Fatalf("ListSecrets = %+v", list)
	}
	counts, err := s.CountSecretsByKeyID(ctx)
	must(t, err)
	if !reflect.DeepEqual(counts, map[string]int64{"aaaaaaaaaaaaaaaa": 1, "bbbbbbbbbbbbbbbb": 1}) {
		t.Fatalf("counts = %v", counts)
	}

	// Name and id are unique.
	err = s.InSecretTx(ctx, func(tx store.SecretTx) error {
		return tx.CreateSecret(ctx, testSecret("01SECRETC", "ci-webhook", "aaaaaaaaaaaaaaaa"))
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate name: %v", err)
	}
	err = s.InSecretTx(ctx, func(tx store.SecretTx) error {
		return tx.CreateSecret(ctx, store.Secret{ID: "x", Name: "y"})
	})
	if !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("empty fields: %v", err)
	}

	// Updates are fenced by the revision.
	upd := got
	upd.KeyID, upd.Ciphertext, upd.Hosts = "cccccccccccccccc", []byte{9}, nil
	upd.UpdatedAt = got.UpdatedAt.Add(time.Minute)
	var ok bool
	must(t, s.InSecretTx(ctx, func(tx store.SecretTx) (err error) { ok, err = tx.UpdateSecret(ctx, upd); return err }))
	if !ok {
		t.Fatal("update at the current revision was refused")
	}
	must(t, s.InSecretTx(ctx, func(tx store.SecretTx) (err error) { ok, err = tx.UpdateSecret(ctx, upd); return err }))
	if ok {
		t.Fatal("update at a stale revision was applied")
	}
	var byID store.Secret
	must(t, s.InSecretTx(ctx, func(tx store.SecretTx) (err error) { byID, err = tx.GetSecret(ctx, a.ID); return err }))
	if byID.Revision != 2 || byID.KeyID != "cccccccccccccccc" || len(byID.Hosts) != 0 || !byID.CreatedAt.Equal(a.CreatedAt) ||
		!byID.UpdatedAt.Equal(upd.UpdatedAt) || byID.Name != "ci-webhook" {
		t.Fatalf("after update: %+v", byID)
	}

	must(t, s.InSecretTx(ctx, func(tx store.SecretTx) error { return tx.DeleteSecret(ctx, a.ID) }))
	err = s.InSecretTx(ctx, func(tx store.SecretTx) error { return tx.DeleteSecret(ctx, a.ID) })
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if _, err := s.SecretByName(ctx, "ci-webhook"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted secret: %v", err)
	}
}

func TestSecretTxRollsBack(t *testing.T) {
	ctx := t.Context()
	s, _ := openTest(t)
	boom := errors.New("boom")
	err := s.InSecretTx(ctx, func(tx store.SecretTx) error {
		if err := tx.CreateSecret(ctx, testSecret("01SECRETA", "a", "aaaaaaaaaaaaaaaa")); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if n, err := s.CountSecretsByKeyID(ctx); err != nil || len(n) != 0 {
		t.Fatalf("rolled back write kept: %v %v", n, err)
	}
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
