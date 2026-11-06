package store

import (
	"context"
	"time"
)

// Secret is a stored secret (story #75, ADR 0024 §6). The store keeps the
// sealed envelope only: sealing and opening are internal/secrets' job, and
// nothing here ever holds a plaintext value.
type Secret struct {
	// ID is a ULID, immutable; it is part of the envelope's associated data.
	ID string
	// Name is unique and referenced by *_ref configuration keys; it is part
	// of the associated data too.
	Name string
	// Hosts is the host binding (ADR 0024 §2): "host" or "host:port"
	// entries. Empty means unbound. Not secret.
	Hosts []string
	// KeyID is the hex id of the key that sealed Ciphertext (a copy of
	// envelope bytes 1-8).
	KeyID      string
	Ciphertext []byte
	// Revision counts the writes of the row, starting at 1; UpdateSecret is
	// fenced by it.
	Revision  int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SecretReader holds the reads of stored secrets. On the store they each run
// outside a transaction.
type SecretReader interface {
	// ListSecrets returns every stored secret, ordered by name.
	ListSecrets(ctx context.Context) ([]Secret, error)
	// SecretByName returns the secret with this name, or ErrNotFound.
	SecretByName(ctx context.Context, name string) (Secret, error)
	// CountSecretsByKeyID returns how many secrets each key id sealed.
	// Key ids with no row are absent; an empty table gives an empty map.
	CountSecretsByKeyID(ctx context.Context) (map[string]int64, error)
}

// SecretTx is the unit of work of a secret write or of a rotation batch.
type SecretTx interface {
	SecretReader
	// GetSecret returns the secret with this id, or ErrNotFound.
	GetSecret(ctx context.Context, id string) (Secret, error)
	// CreateSecret stores a new secret at revision 1. ErrConflict when the
	// id or the name is taken; ErrInvalid for an empty field.
	CreateSecret(ctx context.Context, s Secret) error
	// UpdateSecret stores Hosts, KeyID, Ciphertext and UpdatedAt of the row
	// s.ID, fenced by s.Revision: it writes only while the row is still at
	// that revision, increments it, and reports false, writing nothing,
	// otherwise (a concurrent write won, or the row is gone). ID, Name and
	// CreatedAt are immutable.
	UpdateSecret(ctx context.Context, s Secret) (bool, error)
	// DeleteSecret deletes a secret. ErrNotFound when it does not exist.
	DeleteSecret(ctx context.Context, id string) error
}

// SecretStore is what secret management needs from an engine: the reads,
// and transactions for writes and for rotation (ADR 0024 §5). On SQLite a
// transaction takes the write lock at BEGIN (BEGIN IMMEDIATE), so no other
// write interleaves with a rotation batch.
type SecretStore interface {
	SecretReader
	InSecretTx(ctx context.Context, fn func(tx SecretTx) error) error
}
