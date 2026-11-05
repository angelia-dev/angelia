package secrets

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	k := ring(t, 1)
	a, kid, err := k.Seal("01ID", "ci-webhook", []byte(canary))
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := k.Seal("01ID", "ci-webhook", []byte(canary))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two seals of one plaintext are equal")
	}
	if kid != k.PrimaryID() || EnvelopeKeyID(a) != kid || len(kid) != 16 || a[0] != 1 {
		t.Fatalf("key id %q, envelope header %x", kid, a[:9])
	}
	if bytes.Contains(a, []byte(canary)) {
		t.Fatal("envelope holds the plaintext")
	}
	if len(a) != 1+8+12+len(canary)+16 {
		t.Fatalf("envelope length %d", len(a))
	}
	for _, env := range [][]byte{a, b} {
		v, err := k.Open("01ID", "ci-webhook", env)
		if err != nil || string(v.Reveal()) != canary {
			t.Fatalf("Open: %v", err)
		}
	}
	// An empty value round trips too.
	e, _, err := k.Seal("01ID", "empty", nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := k.Open("01ID", "empty", e); err != nil || !v.IsZero() {
		t.Fatalf("empty: %v", err)
	}
	if _, _, err := k.Seal("01ID", "big", make([]byte, MaxValueBytes+1)); err == nil {
		t.Fatal("a value over 64 KiB was sealed")
	}
}

func TestKeyIDsAreStableAndDistinct(t *testing.T) {
	a, b := ring(t, 1), ring(t, 1)
	if a.PrimaryID() != b.PrimaryID() {
		t.Fatal("key id is not deterministic")
	}
	if ring(t, 1).PrimaryID() == ring(t, 2).PrimaryID() {
		t.Fatal("two keys share an id")
	}
	k := ring(t, 1, 2, 1, 2)
	if ids := k.IDs(); len(ids) != 2 || ids[0] != k.PrimaryID() || !k.Has(ids[1]) || k.Has("0000000000000000") {
		t.Fatalf("IDs = %v", ids)
	}
	if _, err := NewKeyring(make([]byte, 31)); err == nil {
		t.Fatal("a 31-byte key was accepted")
	}
	if _, err := NewKeyring(master(1), make([]byte, 33)); err == nil {
		t.Fatal("a 33-byte previous key was accepted")
	}
}

// Acceptance criterion "tampered ciphertext fails" (ADR 0024 §7).
func TestTamperedEnvelopeFails(t *testing.T) {
	k := ring(t, 1)
	env, kid, err := k.Seal("01ID", "ci-webhook", []byte(canary))
	if err != nil {
		t.Fatal(err)
	}
	check := func(what string, err error, want error) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Fatalf("%s: error %v, want %v", what, err, want)
		}
		noLeak(t, what, err.Error())
		if !strings.Contains(err.Error(), "ci-webhook") {
			t.Fatalf("%s: error does not name the secret: %v", what, err)
		}
	}

	// Every byte flipped: version, key id, nonce, ciphertext, tag.
	for i := range env {
		bad := bytes.Clone(env)
		bad[i] ^= 0x01
		_, err := k.Open("01ID", "ci-webhook", bad)
		switch {
		case i == 0:
			check("version byte", err, ErrUnknownKey)
		case i < 9:
			check("key id byte", err, ErrUnknownKey)
		default:
			check("byte", err, ErrDecrypt)
		}
	}
	// Truncated, at every length.
	for n := range len(env) {
		_, err := k.Open("01ID", "ci-webhook", env[:n])
		if err == nil {
			t.Fatalf("envelope truncated to %d bytes opened", n)
		}
		noLeak(t, "truncated", err.Error())
	}
	// Moved to another row, or the row renamed.
	_, err = k.Open("01OTHER", "ci-webhook", env)
	check("other id", err, ErrDecrypt)
	_, err = k.Open("01ID", "renamed", env)
	if !errors.Is(err, ErrDecrypt) {
		t.Fatalf("renamed: %v", err)
	}
	// A wrong key with the right id cannot exist; a key that is not
	// configured is named.
	_, err = ring(t, 2).Open("01ID", "ci-webhook", env)
	check("unknown key", err, ErrUnknownKey)
	if !strings.Contains(err.Error(), kid) {
		t.Fatalf("unknown key error does not name the key id: %v", err)
	}
	// The key id byte rewritten to another configured key: the GCM tag
	// fails under that key.
	two := ring(t, 2, 1)
	bad := bytes.Clone(env)
	other, _, _ := two.Seal("x", "x", nil)
	copy(bad[1:9], other[1:9])
	_, err = two.Open("01ID", "ci-webhook", bad)
	check("key id swapped", err, ErrDecrypt)
}

func TestPreviousKeyOpens(t *testing.T) {
	old := ring(t, 1)
	env, _, err := old.Seal("01ID", "a", []byte("v"))
	if err != nil {
		t.Fatal(err)
	}
	both := ring(t, 2, 1)
	if v, err := both.Open("01ID", "a", env); err != nil || string(v.Reveal()) != "v" {
		t.Fatalf("previous key: %v", err)
	}
	if _, kid, _ := both.Seal("01ID", "a", []byte("v")); kid != ring(t, 2).PrimaryID() {
		t.Fatal("a previous key sealed")
	}
}

// NIT1 (#75 review): the AAD binds the header, not only the row. The key id
// lookup already refuses a changed id, so this test aliases a forged id to
// the real key, which leaves the AAD as the only thing that rejects it; a
// changed version byte is checked by aad() directly.
func TestAADBindsHeader(t *testing.T) {
	k := ring(t, 1)
	env, kid, err := k.Seal("01ID", "ci-webhook", []byte(canary))
	if err != nil {
		t.Fatal(err)
	}
	forged := bytes.Clone(env)
	forged[1] ^= 0xff
	k.byID[EnvelopeKeyID(forged)] = k.byID[kid]
	if _, err := k.Open("01ID", "ci-webhook", forged); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("a forged key id opened: %v", err)
	}
	if v, err := k.Open("01ID", "ci-webhook", env); err != nil || string(v.Reveal()) != canary {
		t.Fatalf("the genuine envelope: %v", err)
	}
	h := env[:headerLen]
	v2 := append([]byte{2}, h[1:]...)
	if bytes.Equal(aad(h, "01ID", "ci-webhook"), aad(v2, "01ID", "ci-webhook")) {
		t.Fatal("the AAD ignores the version byte")
	}
}
