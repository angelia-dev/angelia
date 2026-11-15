package main

import (
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/sink/webhook"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/open"
	"github.com/angelia-dev/angelia/internal/store/pgtest"
)

// deadPGPassword is in the DSN of a Postgres server that is not there
// (port 1): no error, log or output may show it.
const deadPGPassword = "s3cr3t-pg-pa55w0rd" //nolint:gosec // a canary for these tests, not a credential

// pgSchema returns a DSN for a fresh schema of the test server (skipped
// without ANGELIA_TEST_POSTGRES_DSN), in the URL form the configuration
// accepts, and a connection to it.
func pgSchema(t *testing.T) (dsn string, db *sql.DB, schema string) {
	t.Helper()
	dsn, schema = pgtest.URLSchema(t)
	db, err := sql.Open(pgtest.DriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return dsn, db, schema
}

func relationCount(t *testing.T, schema string) int64 {
	t.Helper()
	db, err := sql.Open(pgtest.DriverName, pgtest.ServerDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	return queryInt(t, db, fmt.Sprintf(`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = '%s'`, schema))
}

// yamlDSN quotes a DSN for the instance configuration.
func yamlDSN(dsn string) string { return "database:\n  dsn: '" + dsn + "'\n" }

func seedPGSecrets(t *testing.T, dsn string, keys *secrets.Keyring, names ...string) {
	t.Helper()
	st, err := open.Open(t.Context(), dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	at := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	for i, n := range names {
		id := fmt.Sprintf("01SEED%020d", i)
		env, kid, err := keys.Seal(id, n, []byte(cliCanary+n))
		if err != nil {
			t.Fatal(err)
		}
		s := store.Secret{ID: id, Name: n, KeyID: kid, Ciphertext: env, CreatedAt: at, UpdatedAt: at}
		if err := st.InSecretTx(t.Context(), func(tx store.SecretTx) error { return tx.CreateSecret(t.Context(), s) }); err != nil {
			t.Fatal(err)
		}
	}
}

// `config check` counts the stored secrets per key id on Postgres, and the
// instance-side commands never create a schema, whatever the DSN.
func TestPostgresConfigCheckAndSecretsStatusAreReadOnly(t *testing.T) {
	t.Setenv("ANGELIA_CONFIG", "")
	t.Setenv(config.KeyEnv, "")
	dsn, _, schema := pgSchema(t)
	key := writeKeyFile(t, 1)
	cfg := writeCfg(t, yamlDSN(dsn)+"secrets:\n  key_file: "+key+"\n")

	if out, _, err := runCLI(t, "config", "check", "--config", cfg); err != nil || !strings.Contains(out, "(0 stored)") {
		t.Fatalf("config check on an empty schema: %v %s", err, out)
	}
	for _, cmd := range []string{"status", "rotate"} {
		if _, _, err := runCLI(t, "secrets", cmd, "--config", cfg); err == nil || !strings.Contains(err.Error(), open.ErrNoDatabase.Error()) {
			t.Errorf("secrets %s on an empty schema: %v", cmd, err)
		}
	}
	if n := relationCount(t, schema); n != 0 {
		t.Fatalf("config check and secrets left %d relations in the schema", n)
	}

	seedPGSecrets(t, dsn, keyring(t, 1), "a", "b")
	out, _, err := runCLI(t, "config", "check", "--config", cfg)
	if err != nil || !strings.Contains(out, "(2 stored)") {
		t.Fatalf("config check: %v %s", err, out)
	}
	// Rows under a key that is not configured fail the check, like on SQLite.
	other := writeCfg(t, yamlDSN(dsn)+"secrets:\n  key_file: "+writeKeyFile(t, 2)+"\n")
	if _, _, err := runCLI(t, "config", "check", "--config", other); err == nil || !strings.Contains(err.Error(), keyring(t, 1).PrimaryID()) {
		t.Fatalf("config check with another key: %v", err)
	}
	if out, _, err := runCLI(t, "secrets", "status", "--config", cfg); err != nil || !strings.Contains(out, "(2 stored)") {
		t.Fatalf("status: %v %s", err, out)
	}
}

// `secrets rotate` re-encrypts on Postgres, each row fenced by its revision.
func TestPostgresSecretsRotate(t *testing.T) {
	t.Setenv("ANGELIA_CONFIG", "")
	t.Setenv(config.KeyEnv, "")
	dsn, db, _ := pgSchema(t)
	oldKey, newKey := writeKeyFile(t, 1), writeKeyFile(t, 2)
	cfg := writeCfg(t, yamlDSN(dsn)+"secrets:\n  key_file: "+newKey+"\n  previous_key_files: ["+oldKey+"]\n")
	seedPGSecrets(t, dsn, keyring(t, 1), "a", "b", "c")
	oldID, newID := keyring(t, 1).PrimaryID(), keyring(t, 2).PrimaryID()

	out, _, err := runCLI(t, "secrets", "rotate", "--dry-run", "--config", cfg)
	if err != nil || !strings.Contains(out, "would re-encrypt 3 secrets under key "+newID) {
		t.Fatalf("dry run: %v %s", err, out)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM secret WHERE key_id = '`+oldID+`'`); n != 3 {
		t.Fatalf("a dry run changed %d rows", 3-n)
	}
	out, errOut, err := runCLI(t, "secrets", "rotate", "--batch", "2", "--config", cfg)
	if err != nil || !strings.Contains(out, "re-encrypted 3 secrets") || !strings.Contains(out, "from key "+oldID+": 3") {
		t.Fatalf("rotate: %v\n%s%s", err, out, errOut)
	}
	noCanary(t, "rotate", out, errOut)
	if n := queryInt(t, db, `SELECT count(*) FROM secret WHERE key_id = '`+newID+`' AND revision = 2`); n != 3 {
		t.Fatalf("%d rows moved to the new key with revision 2, want 3", n)
	}
	if out, _, err := runCLI(t, "secrets", "status", "--config", cfg); err != nil || !strings.Contains(out, "seals no row") {
		t.Fatalf("status after rotate: %v %s", err, out)
	}
}

// AC: `serve` starts every role on a Postgres DSN and delivers a signed
// event end to end (the Postgres twin of TestServeDeliversSignedWebhook).
func TestServeAllRolesOnPostgres(t *testing.T) {
	dsn, db, _ := pgSchema(t)
	reg := &fakePyPI{}
	reg.set("1.0.0", "0.9.0")
	registry := listenOn(t, "127.0.0.1:0", reg)
	rc := newReceiver()
	hook := listenOn(t, "127.0.0.1:0", rc)

	t.Setenv("ANGELIA_CONFIG", "")
	t.Setenv("ANGELIA_WATCHES", "")
	t.Setenv(config.KeyEnv, "")
	t.Setenv("HOOK_SECRET", envSecret)
	listen := freeAddr(t)
	cfgPath := writeCfg(t, fmt.Sprintf(`listen: %q
instance:
  name: e2e
http:
  allow_networks: [127.0.0.1/32]
  allow_http_hosts: [127.0.0.1]
sources:
  pypi:
    base_url: %s
secrets:
  key_file: %s
`, listen, registry.URL, writeKeyFile(t, 7))+yamlDSN(dsn))
	st, err := open.Open(t.Context(), dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	const id = "01JBSECRET0000000000000000"
	env, kid, err := keyring(t, 7).Seal(id, "hook-ref", []byte(refSecret))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s := store.Secret{ID: id, Name: "hook-ref", Hosts: []string{strings.TrimPrefix(hook.URL, "http://")}, KeyID: kid, Ciphertext: env, CreatedAt: now, UpdatedAt: now}
	if err := st.InSecretTx(t.Context(), func(tx store.SecretTx) error { return tx.CreateSecret(t.Context(), s) }); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	logs := startServe(t, "--config", cfgPath, "--watches", watchFile(t, hook.URL), "--roles=all")
	waitFor(t, "the baseline check", logs, func() bool {
		return queryInt(t, db, `SELECT last_checked_at FROM target`) > 0 &&
			queryInt(t, db, `SELECT COUNT(*) FROM watch WHERE baseline_done`) == 1
	})
	if n := queryInt(t, db, `SELECT COUNT(*) FROM event`); n != 0 {
		t.Fatalf("the baseline emitted %d events", n)
	}
	reg.set("1.1.0", "1.0.0", "0.9.0")
	recheckSoon(t, db)
	waitFor(t, "two deliveries", logs, func() bool { return len(rc.all()) >= 2 })
	secretFor := map[string]string{"/env": envSecret, "/ref": refSecret}
	for _, r := range rc.all() {
		if err := webhook.Verify([]string{secretFor[r.path]}, r.header, r.body, time.Now(), 5*time.Minute); err != nil {
			t.Errorf("%s: signature does not verify: %v", r.path, err)
		}
	}
	waitFor(t, "both deliveries recorded", logs, func() bool {
		return queryInt(t, db, `SELECT COUNT(*) FROM delivery WHERE status = 'delivered'`) == 2
	})
	// Neither the DSN nor its password reaches the log.
	if u, err := url.Parse(dsn); err == nil {
		if pw, _ := u.User.Password(); len(pw) >= 4 && strings.Contains(logs.String(), pw) {
			t.Fatal("the database password reached the log")
		}
	}
	if strings.Contains(logs.String(), dsn) {
		t.Fatal("the DSN reached the log")
	}
}

// A Postgres server that is not there stops serve with an error that does not
// show the password.
func TestServeOnADeadPostgresFailsWithoutThePassword(t *testing.T) {
	t.Setenv("ANGELIA_CONFIG", "")
	cfg := writeCfg(t, fmt.Sprintf("listen: %q\n", freeAddr(t))+yamlDSN("postgres://angelia:"+deadPGPassword+"@127.0.0.1:1/angelia?sslmode=disable"))
	_, errOut, err := runServeWithin(t, "--config", cfg, "--roles", "all")
	if err == nil || strings.Contains(err.Error()+errOut, deadPGPassword) {
		t.Fatalf("serve on a dead Postgres: %v\n%s", err, errOut)
	}
}

// silentPG accepts connections and never answers, like a server that is hung
// or a firewall that drops packets after the handshake. It returns a DSN.
func silentPG(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var conns []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return "postgres://angelia:" + deadPGPassword + "@" + ln.Addr().String() + "/angelia?sslmode=disable"
}

// The read-only helpers of `config check` and of the start-up check are
// bounded: against a server that never answers they return when the bound
// (shortened here) expires, with an error and no hang.
func TestPostgresHelpersAreBoundedAgainstAHungServer(t *testing.T) {
	t.Setenv("ANGELIA_CONFIG", "")
	t.Setenv(config.KeyEnv, "")
	old := helperTimeout
	helperTimeout = 200 * time.Millisecond
	t.Cleanup(func() { helperTimeout = old })
	dsn := silentPG(t)
	cfg := writeCfg(t, yamlDSN(dsn)+"secrets:\n  key_file: "+writeKeyFile(t, 1)+"\n")

	done := make(chan struct{})
	go func() {
		defer close(done)
		out, errOut, err := runCLI(t, "config", "check", "--config", cfg)
		if err != nil || !strings.Contains(errOut, "stored secrets not checked") || strings.Contains(out, "(0 stored)") {
			t.Errorf("config check: %v\n%s\n%s", err, out, errOut)
		}
		if strings.Contains(out+errOut, deadPGPassword) {
			t.Errorf("the password is shown: %s %s", out, errOut)
		}
		if err := checkSecretsAtStartup(t.Context(), mustLoad(t, cfg), nil); err == nil || strings.Contains(err.Error(), deadPGPassword) {
			t.Errorf("checkSecretsAtStartup on a hung server: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("a helper is not bounded: it is still waiting for a hung server")
	}
}

func TestHelperContextCarriesTheBound(t *testing.T) {
	if helperTimeoutDefault != 10*time.Second {
		t.Fatalf("helperTimeoutDefault = %v, want 10s", helperTimeoutDefault)
	}
	ctx, cancel := helperContext(t.Context())
	defer cancel()
	if d, ok := ctx.Deadline(); !ok || time.Until(d) > helperTimeoutDefault {
		t.Fatalf("deadline = %v, %v", d, ok)
	}
}
