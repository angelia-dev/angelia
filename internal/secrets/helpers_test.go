package secrets

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
)

// canary is a synthetic secret value: tests assert it never reaches a log,
// an error or an encoding.
const canary = "whsec_CANARYcanaryCANARYcanary0123456789"

func master(b byte) []byte { return bytes.Repeat([]byte{b}, KeySize) }

func ring(t *testing.T, primary byte, previous ...byte) *Keyring {
	t.Helper()
	var prev [][]byte
	for _, p := range previous {
		prev = append(prev, master(p))
	}
	k, err := NewKeyring(master(primary), prev...)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func openStore(t *testing.T) *sqlite.Store {
	t.Helper()
	st, _ := openStorePath(t)
	return st
}

func openStorePath(t *testing.T) (*sqlite.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.db")
	st, err := sqlite.Open(t.Context(), path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, path
}

// put stores a secret sealed with keys.
func put(t *testing.T, st store.SecretStore, keys *Keyring, id, name, value string, hosts ...string) store.Secret {
	t.Helper()
	env, kid, err := keys.Seal(id, name, []byte(value))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	s := store.Secret{ID: id, Name: name, Hosts: hosts, KeyID: kid, Ciphertext: env, CreatedAt: at, UpdatedAt: at}
	if err := st.InSecretTx(t.Context(), func(tx store.SecretTx) error { return tx.CreateSecret(t.Context(), s) }); err != nil {
		t.Fatal(err)
	}
	return s
}

// noLeak fails when out holds the canary, its base64, or key material.
func noLeak(t *testing.T, what, out string, keys ...[]byte) {
	t.Helper()
	for _, bad := range []string{canary, base64.StdEncoding.EncodeToString([]byte(canary)), "CANARY"} {
		if strings.Contains(out, bad) {
			t.Fatalf("%s leaks the secret value: %s", what, out)
		}
	}
	for _, k := range keys {
		if strings.Contains(out, base64.StdEncoding.EncodeToString(k)) {
			t.Fatalf("%s leaks a key: %s", what, out)
		}
	}
}
