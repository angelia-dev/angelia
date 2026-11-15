package postgres

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/pgtest"
)

func tableCount(t *testing.T, schema string) int {
	t.Helper()
	db, err := sql.Open(DriverName, pgtest.ServerDSN(t))
	must(t, err)
	defer func() { _ = db.Close() }()
	var n int
	must(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1`, schema).Scan(&n))
	return n
}

// The reader never creates or migrates: on an empty schema it reads as empty,
// leaves the schema empty, and its connections refuse a write.
func TestReaderNeverCreatesOrMigrates(t *testing.T) {
	dsn, schema := pgtest.Schema(t)
	r, err := OpenReader(t.Context(), dsn)
	must(t, err)
	t.Cleanup(func() { _ = r.Close() })
	ctx := t.Context()
	if ok, err := r.Initialized(ctx); err != nil || ok {
		t.Fatalf("Initialized = %v, %v; want false", ok, err)
	}
	if l, err := r.ListSecrets(ctx); err != nil || len(l) != 0 {
		t.Fatalf("ListSecrets = %v, %v", l, err)
	}
	if c, err := r.CountSecretsByKeyID(ctx); err != nil || c == nil || len(c) != 0 {
		t.Fatalf("CountSecretsByKeyID = %v, %v", c, err)
	}
	if _, err := r.SecretByName(ctx, "x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("SecretByName = %v, want ErrNotFound", err)
	}
	if ts, err := r.TargetSources(ctx); err != nil || len(ts) != 0 {
		t.Fatalf("TargetSources = %v, %v", ts, err)
	}
	if _, err := r.db.ExecContext(ctx, `CREATE TABLE intruder (a int)`); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("a write on the reader: %v, want a read-only transaction error", err)
	}
	if n := tableCount(t, schema); n != 0 {
		t.Fatalf("the schema holds %d relations after reading, want 0", n)
	}
}

func TestReaderReadsAMigratedDatabase(t *testing.T) {
	dsn := pgtest.DSN(t)
	s, err := Open(t.Context(), dsn, nil)
	must(t, err)
	t.Cleanup(func() { _ = s.Close() })
	ctx := t.Context()
	seedStore(t, s)
	must(t, s.InSecretTx(ctx, func(tx store.SecretTx) error {
		return tx.CreateSecret(ctx, store.Secret{ID: "sec1", Name: "tok", Hosts: []string{"a.example"}, KeyID: "k1", Ciphertext: []byte("c"), CreatedAt: t0, UpdatedAt: t0})
	}))
	must(t, s.CreateTarget(ctx, store.Target{ID: "t2", Kind: "mypypi", SourceKind: "pypi", Coordinates: map[string]string{"name": "b"}, NextCheckAt: t0}))

	r, err := OpenReader(ctx, dsn)
	must(t, err)
	t.Cleanup(func() { _ = r.Close() })
	if ok, err := r.Initialized(ctx); err != nil || !ok {
		t.Fatalf("Initialized = %v, %v; want true", ok, err)
	}
	if c, err := r.CountSecretsByKeyID(ctx); err != nil || c["k1"] != 1 {
		t.Fatalf("CountSecretsByKeyID = %v, %v", c, err)
	}
	if l, err := r.ListSecrets(ctx); err != nil || len(l) != 1 || l[0].Hosts[0] != "a.example" {
		t.Fatalf("ListSecrets = %v, %v", l, err)
	}
	if sec, err := r.SecretByName(ctx, "tok"); err != nil || sec.ID != "sec1" {
		t.Fatalf("SecretByName = %v, %v", sec, err)
	}
	if _, err := r.SecretByName(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("SecretByName(nope) = %v", err)
	}
	want := []store.TargetSource{{Name: "mypypi", Kind: "pypi", Targets: 1}, {Name: "pypi", Targets: 1}}
	if got, err := r.TargetSources(ctx); err != nil || !slices.Equal(got, want) {
		t.Fatalf("TargetSources = %v, %v; want %v", got, err, want)
	}
	if got, err := s.TargetSources(ctx); err != nil || !slices.Equal(got, want) {
		t.Fatalf("Store.TargetSources = %v, %v; want %v", got, err, want)
	}
}

func TestStoreTargetDemand(t *testing.T) {
	s, err := Open(t.Context(), pgtest.DSN(t), nil)
	must(t, err)
	t.Cleanup(func() { _ = s.Close() })
	ctx := t.Context()
	must(t, s.CreateTarget(ctx, store.Target{ID: "t1", Kind: "pypi", Coordinates: map[string]string{"name": "a"}, NextCheckAt: t0}))
	must(t, s.CreateTarget(ctx, store.Target{ID: "t2", Kind: "pypi", Coordinates: map[string]string{"name": "b"}, NextCheckAt: t0}))
	got, err := s.TargetDemand(ctx)
	if err != nil || len(got) != 1 || got[0].Name != "pypi" || got[0].Targets != 2 || got[0].Interval != 0*time.Second {
		t.Fatalf("TargetDemand = %+v, %v", got, err)
	}
}

func TestReaderMissingDatabase(t *testing.T) {
	asURL, err := pgtest.ToURL(pgtest.ServerDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	server, err := url.Parse(asURL)
	if err != nil {
		t.Fatal(err)
	}
	server.Path = "/angelia_no_such_database"
	if _, err := OpenReader(t.Context(), server.String()); !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("OpenReader on a missing database = %v, want ErrNoDatabase", err)
	}
}

func TestReaderErrorsNeverHoldThePassword(t *testing.T) {
	const pw = "s3cr3t-pa55w0rd"
	// Port 1 refuses the connection; the DSN is never reached by a server.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := OpenReader(ctx, "postgres://angelia:"+pw+"@127.0.0.1:1/angelia?sslmode=disable")
	if err == nil {
		t.Fatal("no error from a refused connection")
	}
	if strings.Contains(err.Error(), pw) {
		t.Fatalf("the error holds the password: %v", err)
	}
	_, err = OpenReader(ctx, "postgres://angelia:"+pw+"@127.0.0.1:1/angelia?sslmode=disable&pool_min_conns=2")
	if err == nil || strings.Contains(err.Error(), pw) {
		t.Fatalf("pool_min_conns: %v", err)
	}
}
