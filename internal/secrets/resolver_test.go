package secrets

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/angelia-dev/angelia/internal/store"
)

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestResolver(t *testing.T) {
	ctx := t.Context()
	st, path := openStorePath(t)
	keys := ring(t, 1)
	put(t, st, keys, "01A", "ci-webhook", canary)
	put(t, st, keys, "01B", "registry", canary, "registry.example.com")
	r := &Resolver{Store: st, Keys: keys}

	b, err := r.Resolve(ctx, "ci-webhook", mustURL(t, "https://anywhere.example.com/hook"))
	if err != nil || string(b.Value.Reveal()) != canary || b.Name != "ci-webhook" || len(b.Hosts) != 0 {
		t.Fatalf("unbound: %v", err)
	}
	b, err = r.Resolve(ctx, "registry", mustURL(t, "https://registry.example.com/simple/"))
	if err != nil || string(b.Value.Reveal()) != canary || len(b.Hosts) != 1 {
		t.Fatalf("bound, right host: %v", err)
	}
	if err := b.Allows(mustURL(t, "https://other.example.com/")); !errors.Is(err, ErrHostMismatch) {
		t.Fatalf("redirect check: %v", err)
	}

	for name, tc := range map[string]struct {
		r    *Resolver
		ref  string
		dest string
		want error
	}{
		"other host":     {r, "registry", "https://evil.example.com/", ErrHostMismatch},
		"other port":     {r, "registry", "https://registry.example.com:8443/", ErrHostMismatch},
		"plain http":     {r, "registry", "http://registry.example.com/", ErrHostMismatch},
		"no destination": {r, "registry", "", ErrHostMismatch},
		"missing":        {r, "nope", "https://x.example.com/", ErrNotFound},
		"invalid name":   {r, "Bad Name", "https://x.example.com/", ErrInvalidName},
		"no key":         {&Resolver{Store: st}, "ci-webhook", "https://x.example.com/", ErrKeyMissing},
		"wrong key":      {&Resolver{Store: st, Keys: ring(t, 2)}, "ci-webhook", "https://x.example.com/", ErrUnknownKey},
	} {
		var dest *url.URL
		if tc.dest != "" {
			dest = mustURL(t, tc.dest)
		}
		_, err := tc.r.Resolve(ctx, tc.ref, dest)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
			continue
		}
		noLeak(t, name, err.Error())
	}

	// The host check comes before decryption: a bound secret is refused
	// for another host even when no key is configured.
	_, err = (&Resolver{Store: st}).Resolve(ctx, "registry", mustURL(t, "https://evil.example.com/"))
	if !errors.Is(err, ErrHostMismatch) {
		t.Fatalf("no key, other host: %v", err)
	}

	// Swapping the ciphertexts of two rows, or renaming a row in SQL,
	// makes them fail.
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	a, err := st.SecretByName(ctx, "ci-webhook")
	must(err)
	bb, err := st.SecretByName(ctx, "registry")
	must(err)
	must(st.InSecretTx(ctx, func(tx store.SecretTx) error {
		a.Ciphertext, bb.Ciphertext = bb.Ciphertext, a.Ciphertext
		if _, err := tx.UpdateSecret(ctx, a); err != nil {
			return err
		}
		_, err := tx.UpdateSecret(ctx, bb)
		return err
	}))
	if _, err := r.Resolve(ctx, "ci-webhook", mustURL(t, "https://x.example.com/")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("swapped ciphertext: %v", err)
	}
	put(t, st, keys, "01C", "before", canary)
	must(renameSecret(ctx, path, "before", "after"))
	if _, err := r.Resolve(ctx, "after", mustURL(t, "https://x.example.com/")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("renamed row: %v", err)
	}
}

type failingLookup struct{}

func (failingLookup) SecretByName(context.Context, string) (store.Secret, error) {
	return store.Secret{}, errors.New("disk on fire")
}

func TestResolverStoreError(t *testing.T) {
	_, err := (&Resolver{Store: failingLookup{}, Keys: ring(t, 1)}).Resolve(t.Context(), "a", nil)
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("store error: %v", err)
	}
}
