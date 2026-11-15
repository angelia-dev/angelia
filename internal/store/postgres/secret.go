package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/angelia-dev/angelia/internal/store"
)

// Stored secrets ([store.SecretStore], story #75, ADR 0024 §6).

// InSecretTx implements [store.SecretStore]. The transaction takes the config
// lock first, so no other config, API or secret write interleaves with a
// rotation batch (ADR 0028 §2); UpdateSecret keeps its revision fence too.
func (s *Store) InSecretTx(ctx context.Context, fn func(tx store.SecretTx) error) error {
	return s.withTx(ctx, lockConfig, func(q *Queries) error { return fn(s.ops(q, kindConfig)) })
}

// ListSecrets implements [store.SecretReader].
func (s *Store) ListSecrets(ctx context.Context) ([]store.Secret, error) {
	return s.stmt().ListSecrets(ctx)
}

// SecretByName implements [store.SecretReader].
func (s *Store) SecretByName(ctx context.Context, name string) (store.Secret, error) {
	return s.stmt().SecretByName(ctx, name)
}

// CountSecretsByKeyID implements [store.SecretReader].
func (s *Store) CountSecretsByKeyID(ctx context.Context) (map[string]int64, error) {
	return s.stmt().CountSecretsByKeyID(ctx)
}

func (o txOps) ListSecrets(ctx context.Context) ([]store.Secret, error) {
	rows, err := o.q.ListSecrets(ctx)
	if err != nil {
		return nil, wrap(err, "list secrets", "")
	}
	out := make([]store.Secret, 0, len(rows))
	for _, r := range rows {
		sec, err := secretFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, sec)
	}
	return out, nil
}

func (o txOps) SecretByName(ctx context.Context, name string) (store.Secret, error) {
	r, err := o.q.GetSecretByName(ctx, name)
	if err != nil {
		return store.Secret{}, wrap(err, "secret by name", name)
	}
	return secretFromRow(r)
}

func (o txOps) GetSecret(ctx context.Context, id string) (store.Secret, error) {
	r, err := o.q.GetSecret(ctx, id)
	if err != nil {
		return store.Secret{}, wrap(err, "get secret", id)
	}
	return secretFromRow(r)
}

func (o txOps) CountSecretsByKeyID(ctx context.Context) (map[string]int64, error) {
	rows, err := o.q.CountSecretsByKeyID(ctx)
	if err != nil {
		return nil, wrap(err, "count secrets", "")
	}
	out := make(map[string]int64, len(rows))
	for _, r := range rows {
		out[r.KeyID] = r.N
	}
	return out, nil
}

func (o txOps) CreateSecret(ctx context.Context, s store.Secret) error {
	hosts, err := secretFields(s)
	if err != nil {
		return err
	}
	return wrap(o.q.CreateSecret(ctx, CreateSecretParams{
		ID: s.ID, Name: s.Name, HostsJson: hosts, KeyID: s.KeyID, Ciphertext: s.Ciphertext,
		CreatedAt: millis(s.CreatedAt), UpdatedAt: millis(s.UpdatedAt),
	}), "create secret", s.Name)
}

func (o txOps) UpdateSecret(ctx context.Context, s store.Secret) (bool, error) {
	hosts, err := secretFields(s)
	if err != nil {
		return false, err
	}
	n, err := o.q.UpdateSecret(ctx, UpdateSecretParams{
		HostsJson: hosts, KeyID: s.KeyID, Ciphertext: s.Ciphertext, UpdatedAt: millis(s.UpdatedAt),
		ID: s.ID, Revision: s.Revision,
	})
	if err != nil {
		return false, wrap(err, "update secret", s.Name)
	}
	return n == 1, nil
}

func (o txOps) DeleteSecret(ctx context.Context, id string) error {
	n, err := o.q.DeleteSecret(ctx, id)
	return deleted(n, err, "delete secret", id)
}

// secretFields checks the fields every write stores and encodes the hosts.
func secretFields(s store.Secret) (string, error) {
	if s.ID == "" || s.Name == "" || s.KeyID == "" || len(s.Ciphertext) == 0 || s.UpdatedAt.IsZero() {
		return "", fmt.Errorf("store: secret %s: empty field: %w", s.Name, store.ErrInvalid)
	}
	hosts := s.Hosts
	if hosts == nil {
		hosts = []string{}
	}
	b, err := json.Marshal(hosts)
	if err != nil {
		return "", fmt.Errorf("store: secret %s: hosts: %w", s.Name, err)
	}
	return string(b), nil
}

func secretFromRow(r Secret) (store.Secret, error) {
	hosts := []string{}
	if err := json.Unmarshal([]byte(r.HostsJson), &hosts); err != nil {
		return store.Secret{}, fmt.Errorf("store: secret %s: hosts: %w", r.Name, err)
	}
	if hosts == nil {
		hosts = []string{}
	}
	return store.Secret{
		ID: r.ID, Name: r.Name, Hosts: hosts, KeyID: r.KeyID, Ciphertext: r.Ciphertext, Revision: r.Revision,
		CreatedAt: fromMillis(r.CreatedAt), UpdatedAt: fromMillis(r.UpdatedAt),
	}, nil
}
