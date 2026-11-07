package secrets

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/angelia-dev/angelia/internal/store"
)

var (
	// ErrNotFound is wrapped when a reference names a secret that does
	// not exist.
	ErrNotFound = errors.New("secrets: no such secret")
	// ErrKeyMissing is wrapped when a secret is read or written while no
	// key is configured.
	ErrKeyMissing = errors.New("secrets: no key configured")
	// ErrInvalidName is wrapped when a reference is not a valid secret
	// name.
	ErrInvalidName = errors.New("secrets: invalid secret name")
)

// Lookup is the store read a Resolver needs.
type Lookup interface {
	SecretByName(ctx context.Context, name string) (store.Secret, error)
}

// Resolver turns a *_ref reference into the secret's value, for one
// destination. It reads the store on every call: values are not cached
// across attempts (ADR 0024 §6).
type Resolver struct {
	Store Lookup
	// Keys opens the envelopes; nil means no key is configured.
	Keys *Keyring
}

// Resolve returns the secret named name for a request to dest. A secret
// bound to hosts is resolved only when dest's canonical host and port equal
// one of them, before anything is decrypted; with a nil dest a bound secret
// is refused. The caller re-checks every further request that carries the
// value (each redirect hop) with Bound.Allows or Bound.CheckRedirect.
// Errors name the secret and the host, never the value: ErrInvalidName,
// ErrNotFound, ErrKeyMissing, ErrHostMismatch, ErrUnknownKey or ErrDecrypt.
func (r *Resolver) Resolve(ctx context.Context, name string, dest *url.URL) (Bound, error) {
	return r.resolve(ctx, name, func(row store.Secret) error { return allows(name, row.Hosts, dest) })
}

// ResolveSelfAddressed returns the secret named name with its hosts, for a
// reference whose resolved value is itself the destination (a reference the
// kind's config_schema marks x-angelia-secret-destination, ADR 0026 §2). The
// destination is known only after decrypting, so nothing is checked against
// the hosts here: the caller derives the origins from the value and checks
// each with Bound.Allows before any byte of the value leaves the process.
// Errors are those of Resolve, without ErrHostMismatch.
func (r *Resolver) ResolveSelfAddressed(ctx context.Context, name string) (Bound, error) {
	return r.resolve(ctx, name, func(store.Secret) error { return nil })
}

// resolve reads the secret named name, runs check on its row before
// anything is decrypted, then opens it.
func (r *Resolver) resolve(ctx context.Context, name string, check func(store.Secret) error) (Bound, error) {
	if !ValidName(name) {
		return Bound{}, fmt.Errorf("%w: a secret name matches ^[a-z0-9][a-z0-9._-]{0,62}$", ErrInvalidName)
	}
	row, err := r.Store.SecretByName(ctx, name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return Bound{}, fmt.Errorf("%w: secret %s does not exist", ErrNotFound, name)
	case err != nil:
		return Bound{}, fmt.Errorf("secrets: read secret %s: %w", name, err)
	}
	if err := check(row); err != nil {
		return Bound{}, err
	}
	if r.Keys == nil {
		return Bound{}, fmt.Errorf("%w: cannot read secret %s; set secrets.key_file", ErrKeyMissing, name)
	}
	v, err := r.Keys.Open(row.ID, row.Name, row.Ciphertext)
	if err != nil {
		return Bound{}, err
	}
	return Bound{Name: name, Value: v, Hosts: append([]string(nil), row.Hosts...)}, nil
}
