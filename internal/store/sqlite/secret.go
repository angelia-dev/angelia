package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"

	"github.com/angelia-dev/angelia/internal/store"
)

// Stored secrets ([store.SecretStore], story #75, ADR 0024 §6).

var _ store.SecretStore = (*Store)(nil)

// InSecretTx implements [store.SecretStore]. The transaction takes SQLite's
// write lock at BEGIN (_txlock=immediate), so no other writer interleaves.
func (s *Store) InSecretTx(ctx context.Context, fn func(tx store.SecretTx) error) error {
	return s.withTx(ctx, func(q *Queries) error { return fn(txOps{q: q, config: true}) })
}

// ListSecrets implements [store.SecretReader].
func (s *Store) ListSecrets(ctx context.Context) ([]store.Secret, error) {
	return txOps{q: s.q}.ListSecrets(ctx)
}

// SecretByName implements [store.SecretReader].
func (s *Store) SecretByName(ctx context.Context, name string) (store.Secret, error) {
	return txOps{q: s.q}.SecretByName(ctx, name)
}

// CountSecretsByKeyID implements [store.SecretReader].
func (s *Store) CountSecretsByKeyID(ctx context.Context) (map[string]int64, error) {
	return txOps{q: s.q}.CountSecretsByKeyID(ctx)
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
	err = o.q.CreateSecret(ctx, CreateSecretParams{
		ID: s.ID, Name: s.Name, HostsJson: hosts, KeyID: s.KeyID, Ciphertext: s.Ciphertext,
		CreatedAt: millis(s.CreatedAt), UpdatedAt: millis(s.UpdatedAt),
	})
	return wrap(err, "create secret", s.Name)
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
	if err != nil {
		return wrap(err, "delete secret", id)
	}
	if n == 0 {
		return fmt.Errorf("store: delete secret %s: %w", id, store.ErrNotFound)
	}
	return nil
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
	var hosts []string
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

// ErrNoDatabase is returned by SecretKeyCounts when the database file does
// not exist.
var ErrNoDatabase = errors.New("store: database file does not exist")

// openReadOnly opens the database file at path read-only, without creating
// it or applying migrations; ErrNoDatabase when it does not exist.
func openReadOnly(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoDatabase
	}
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	return db, nil
}

// SecretHosts reads the name and hosts of every stored secret from the
// database file at path, read-only like SecretKeyCounts and without touching
// a ciphertext. `angelia config check` uses it to check the secrets a
// configuration names. A database the secret migration has not reached has
// none. ErrNoDatabase when the file does not exist.
func SecretHosts(ctx context.Context, path string) (map[string][]string, error) {
	db, err := openReadOnly(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	var exists bool
	err = db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'secret')`).Scan(&exists)
	if err != nil {
		return nil, fmt.Errorf("store: read %s: %w", path, err)
	}
	out := map[string][]string{}
	if !exists {
		return out, nil
	}
	rows, err := New(db).ListSecrets(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list secrets in %s: %w", path, err)
	}
	for _, r := range rows {
		s, err := secretFromRow(r)
		if err != nil {
			return nil, err
		}
		out[s.Name] = s.Hosts
	}
	return out, nil
}

// SecretKeyCounts opens the database file at path read-only, without
// creating it or applying migrations, and counts the stored secrets per key
// id. `angelia config check` uses it so that a check has no side effect. A
// database the secret migration has not reached has no secret: the result
// is empty. ErrNoDatabase when the file does not exist.
func SecretKeyCounts(ctx context.Context, path string) (map[string]int64, error) {
	db, err := openReadOnly(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	var exists bool
	err = db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'secret')`).Scan(&exists)
	if err != nil {
		return nil, fmt.Errorf("store: read %s: %w", path, err)
	}
	if !exists {
		return map[string]int64{}, nil
	}
	counts, err := New(db).CountSecretsByKeyID(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: count secrets in %s: %w", path, err)
	}
	out := make(map[string]int64, len(counts))
	for _, c := range counts {
		out[c.KeyID] = c.N
	}
	return out, nil
}

// SecretReader reads stored secrets from a database opened read-only, for
// `angelia config check` (ADR 0026 decided questions 4 and 7). It
// implements secrets.Lookup. Close it after use.
type SecretReader struct {
	db *sql.DB
	q  *Queries
}

// OpenSecretReader opens the database file at path read-only, without
// creating it or applying migrations. ErrNoDatabase when the file does not
// exist; a database without the secret table reads as empty.
func OpenSecretReader(ctx context.Context, path string) (*SecretReader, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoDatabase
	}
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	return &SecretReader{db: db, q: New(db)}, nil
}

// SecretByName returns the secret with this name, or store.ErrNotFound.
func (r *SecretReader) SecretByName(ctx context.Context, name string) (store.Secret, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'secret')`).Scan(&exists)
	switch {
	case err != nil:
		return store.Secret{}, fmt.Errorf("store: read secrets: %w", err)
	case !exists:
		return store.Secret{}, store.ErrNotFound
	}
	return txOps{q: r.q}.SecretByName(ctx, name)
}

// Close closes the database.
func (r *SecretReader) Close() error { return r.db.Close() }
