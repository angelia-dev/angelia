package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/api"
	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
)

// cliCanary is a synthetic secret value: no command may print it.
const cliCanary = "whsec_CLICANARYvalueCLICANARYvalue01234"

func masterKey(b byte) []byte { return bytes.Repeat([]byte{b}, secrets.KeySize) }

func keyring(t *testing.T, primary byte, previous ...byte) *secrets.Keyring {
	t.Helper()
	var prev [][]byte
	for _, p := range previous {
		prev = append(prev, masterKey(p))
	}
	k, err := secrets.NewKeyring(masterKey(primary), prev...)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// writeKeyFile writes master key b as a key file with mode 0600.
func writeKeyFile(t *testing.T, b byte) string {
	t.Helper()
	return writeTemp(t, fmt.Sprintf("key-%d", b), base64.StdEncoding.EncodeToString(masterKey(b))+"\n")
}

func noCanary(t *testing.T, what string, outs ...string) {
	t.Helper()
	for _, out := range outs {
		for _, bad := range []string{cliCanary, base64.StdEncoding.EncodeToString([]byte(cliCanary)), "CLICANARY"} {
			if strings.Contains(out, bad) {
				t.Fatalf("%s prints the secret: %s", what, out)
			}
		}
	}
}

// newSecretServer runs the API with a secrets key on a SQLite store.
func newSecretServer(t *testing.T, logs *bytes.Buffer) (*httptest.Server, *sqlite.Store, *secrets.Keyring) {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(logs, nil))
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "api.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sources, err := watchSources(&config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	keys := keyring(t, 1)
	h, err := api.New(api.Options{
		Store: st, Auth: e2eAuth{}, Sources: sources, Plugins: builtinPlugins(sources.Kinds()), Secrets: keys, Logger: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, st, keys
}

func storedValue(t *testing.T, st *sqlite.Store, keys *secrets.Keyring, name string) string {
	t.Helper()
	row, err := st.SecretByName(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	v, err := keys.Open(row.ID, row.Name, row.Ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	return string(v.Reveal())
}

func TestSecretCommands(t *testing.T) {
	isolate(t)
	var logs bytes.Buffer
	srv, st, keys := newSecretServer(t, &logs)
	conn := []string{"--server", srv.URL, "--token", e2eAdmin}
	run := func(stdin string, args ...string) (string, string, error) {
		root := newRootCmd()
		var o, e bytes.Buffer
		root.SetOut(&o)
		root.SetErr(&e)
		root.SetIn(strings.NewReader(stdin))
		root.SetArgs(append(append([]string{}, args...), conn...))
		err := root.ExecuteContext(t.Context())
		return o.String(), e.String(), err
	}
	var all []string
	must := func(stdin string, args ...string) string {
		t.Helper()
		out, errOut, err := run(stdin, args...)
		all = append(all, out, errOut)
		if err != nil {
			t.Fatalf("angelia %s: %v\n%s%s", strings.Join(args, " "), err, out, errOut)
		}
		return out + errOut
	}

	// --from-file and stdin strip exactly one trailing newline.
	for name, tc := range map[string]struct{ content, want string }{
		"lf":       {cliCanary + "\n", cliCanary},
		"crlf":     {cliCanary + "\r\n", cliCanary},
		"two-lf":   {cliCanary + "\n\n", cliCanary + "\n"},
		"spaces":   {" " + cliCanary + " \n", " " + cliCanary + " "},
		"no-lf":    {cliCanary, cliCanary},
		"cr-alone": {cliCanary + "\r", cliCanary + "\r"},
	} {
		must("", "secret", "set", name, "--from-file", writeTemp(t, name, tc.content))
		if got := storedValue(t, st, keys, name); got != tc.want {
			t.Errorf("%s: stored %q, want %q", name, got, tc.want)
		}
	}
	out := must(cliCanary+"\n", "secret", "set", "from-stdin", "--from-file", "-", "--host", "ci.example.com", "--host", "ci.example.com:8443")
	if !strings.Contains(out, "ci.example.com,ci.example.com:8443") || !strings.Contains(out, `secret "from-stdin": created`) {
		t.Fatalf("set output: %s", out)
	}
	if got := storedValue(t, st, keys, "from-stdin"); got != cliCanary {
		t.Fatalf("stdin: %q", got)
	}
	// Any bytes survive.
	bin := string([]byte{0, 1, 2, 0xff, '\n', 0xfe})
	must("", "secret", "set", "binary", "--from-file", writeTemp(t, "bin", bin+"\n"))
	if got := storedValue(t, st, keys, "binary"); got != bin {
		t.Fatalf("binary: %q", got)
	}
	out = must(cliCanary, "secret", "set", "from-stdin", "--from-file", "-", "--host", "ci.example.com", "--host", "ci.example.com:8443")
	if !strings.Contains(out, "unchanged") {
		t.Fatalf("same value again: %s", out)
	}
	out = must("other", "secret", "set", "from-stdin", "--from-file", "-", "--dry-run")
	if !strings.Contains(out, "dry run") || storedValue(t, st, keys, "from-stdin") != cliCanary {
		t.Fatalf("dry run: %s", out)
	}

	out = must("", "secret", "list")
	for _, n := range []string{"from-stdin", "lf", "two-lf", keys.PrimaryID()} {
		if !strings.Contains(out, n) {
			t.Fatalf("list lacks %s: %s", n, out)
		}
	}
	must("", "secret", "list", "-o", "json")
	out = must("", "secret", "get", "lf", "-o", "json")
	if !strings.Contains(out, `"key_id"`) || strings.Contains(out, `"value`) {
		t.Fatalf("get json: %s", out)
	}
	must("", "secret", "get", "lf")
	must("", "secret", "delete", "lf")
	if _, err := st.SecretByName(t.Context(), "lf"); err == nil {
		t.Fatal("delete kept the secret")
	}

	// Refusals, before anything is sent.
	for _, args := range [][]string{
		{"secret", "set", "Bad Name", "--from-file", "-"},
		{"secret", "set", "x", "--from-file", "-", "--host", "*.example.com"},
		{"secret", "set", "x", "--from-file", writeTemp(t, "empty", "\n")},
		{"secret", "set", "x", "--from-file", writeTemp(t, "big", strings.Repeat("a", secrets.MaxValueBytes+1))},
		{"secret", "set", "x", "--from-file", filepath.Join(t.TempDir(), "missing")},
		{"secret", "set", "x", cliCanary},
		{"secret", "set", "x"},
	} {
		o, e, err := run(cliCanary, args...)
		all = append(all, o, e)
		if err == nil {
			t.Errorf("angelia %s: no error", strings.Join(args, " "))
		}
		if err != nil {
			all = append(all, err.Error())
		}
	}
	noCanary(t, "secret commands", all...)
	noCanary(t, "server log", logs.String())
}

func TestSecretSetNeedsAdmin(t *testing.T) {
	isolate(t)
	var logs bytes.Buffer
	srv, _, _ := newSecretServer(t, &logs)
	root := newRootCmd()
	var o, e bytes.Buffer
	root.SetOut(&o)
	root.SetErr(&e)
	root.SetIn(strings.NewReader(cliCanary))
	root.SetArgs([]string{"secret", "set", "x", "--from-file", "-", "--server", srv.URL, "--token", e2eRead})
	err := root.ExecuteContext(t.Context())
	if err == nil || !strings.Contains(err.Error()+e.String(), "admin") {
		t.Fatalf("read token: %v %s", err, e.String())
	}
	noCanary(t, "forbidden", o.String(), e.String(), err.Error())
}

func TestSecretsKeygen(t *testing.T) {
	out, _, err := runCLI(t, "secrets", "keygen")
	if err != nil || len(out) != 45 || !strings.HasSuffix(out, "\n") {
		t.Fatalf("keygen: %q %v", out, err)
	}
	if k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out)); err != nil || len(k) != secrets.KeySize {
		t.Fatalf("keygen output: %v", err)
	}
	p := filepath.Join(t.TempDir(), "new.key")
	stdout, stderr, err := runCLI(t, "secrets", "keygen", "--out", p)
	if err != nil || stdout != "" || !strings.Contains(stderr, p) {
		t.Fatalf("keygen --out: %q %q %v", stdout, stderr, err)
	}
	info, err := os.Stat(p)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("key file: %v %v", info, err)
	}
	if _, _, err := runCLI(t, "secrets", "keygen", "--out", p); err == nil {
		t.Fatal("keygen overwrote a file")
	}
}

// instanceFiles writes an instance config with a database in a temporary
// directory and returns the config path and the database path.
func instanceFiles(t *testing.T, extra string) (string, string) {
	t.Helper()
	t.Setenv("ANGELIA_CONFIG", "")
	t.Setenv(config.KeyEnv, "")
	db := filepath.Join(t.TempDir(), "angelia.db")
	return writeCfg(t, "database:\n  dsn: sqlite://"+db+"\n"+extra), db
}

// seedSecrets stores sealed secrets in the database at db.
func seedSecrets(t *testing.T, db string, keys *secrets.Keyring, names ...string) {
	t.Helper()
	st, err := sqlite.Open(t.Context(), db, slog.New(slog.DiscardHandler))
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

func TestSecretsRotateAndStatus(t *testing.T) {
	oldKey, newKey := writeKeyFile(t, 1), writeKeyFile(t, 2)
	cfg, db := instanceFiles(t, "secrets:\n  key_file: "+newKey+"\n  previous_key_files: ["+oldKey+"]\n")
	seedSecrets(t, db, keyring(t, 1), "a", "b", "c")
	oldID, newID := keyring(t, 1).PrimaryID(), keyring(t, 2).PrimaryID()

	out, errOut, err := runCLI(t, "secrets", "status", "--config", cfg)
	if err != nil || !strings.Contains(out, oldID) || !strings.Contains(out, "previous") || !strings.Contains(out, "3 under other keys") {
		t.Fatalf("status: %v\n%s%s", err, out, errOut)
	}
	out, _, err = runCLI(t, "secrets", "rotate", "--dry-run", "--config", cfg)
	if err != nil || !strings.Contains(out, "would re-encrypt 3 secrets under key "+newID) {
		t.Fatalf("dry run: %v %s", err, out)
	}
	out, errOut, err = runCLI(t, "secrets", "rotate", "--batch", "2", "--config", cfg)
	if err != nil || !strings.Contains(out, "re-encrypted 3 secrets") || !strings.Contains(out, "from key "+oldID+": 3") {
		t.Fatalf("rotate: %v\n%s%s", err, out, errOut)
	}
	noCanary(t, "rotate", out, errOut)
	if !strings.Contains(errOut, `"action":"secrets.rotate"`) {
		t.Fatalf("no audit record: %s", errOut)
	}
	out, _, err = runCLI(t, "secrets", "status", "--config", cfg)
	if err != nil || !strings.Contains(out, "seals no row") {
		t.Fatalf("status after rotate: %v %s", err, out)
	}
	// The old key can go: every row opens with the new one alone.
	cfgNew := writeCfg(t, "database:\n  dsn: sqlite://"+db+"\nsecrets:\n  key_file: "+newKey+"\n")
	if _, _, err := runCLI(t, "secrets", "status", "--config", cfgNew); err != nil {
		t.Fatalf("new key alone: %v", err)
	}
	// Rows under a key that is not configured: status fails, naming it.
	cfgOld := writeCfg(t, "database:\n  dsn: sqlite://"+db+"\nsecrets:\n  key_file: "+oldKey+"\n")
	_, _, err = runCLI(t, "secrets", "status", "--config", cfgOld)
	if err == nil || !strings.Contains(err.Error(), newID) {
		t.Fatalf("unknown key: %v", err)
	}
	cfgNone := writeCfg(t, "database:\n  dsn: sqlite://"+db+"\n")
	if _, _, err := runCLI(t, "secrets", "rotate", "--config", cfgNone); err == nil {
		t.Fatal("rotate without a key")
	}
}

// Acceptance criterion "missing key at startup with existing secrets =
// clear error, no plaintext fallback".
//
// serve gets a free port (the config refuses port 0) and a deadline: if the check regressed, serve
// would start and run, and the deadline ends it with a clear failure
// instead of the package timeout.
func TestServeRefusesStoredSecretsWithoutKey(t *testing.T) {
	listen := fmt.Sprintf("listen: %q\n", freeAddr(t))
	cfg, db := instanceFiles(t, listen)
	if err := checkSecretsAtStartup(t.Context(), mustLoad(t, cfg), nil); err != nil {
		t.Fatalf("no database yet: %v", err)
	}
	seedSecrets(t, db, keyring(t, 1), "a", "b", "c")
	_, errOut, err := runServeWithin(t, "--config", cfg, "--roles", "worker")
	if err == nil || err.Error() != "secrets: 3 stored secrets but no key configured; set secrets.key_file" {
		t.Fatalf("serve without a key: %v\n%s", err, errOut)
	}
	cfgOther := writeCfg(t, listen+"database:\n  dsn: sqlite://"+db+"\nsecrets:\n  key_file: "+writeKeyFile(t, 2)+"\n")
	_, errOut, err = runServeWithin(t, "--config", cfgOther, "--roles", "worker")
	if err == nil || !strings.Contains(err.Error(), keyring(t, 1).PrimaryID()+" (3 rows)") {
		t.Fatalf("serve with another key: %v\n%s", err, errOut)
	}
	noCanary(t, "serve", errOut, err.Error())
	if err := checkSecretsAtStartup(context.Background(), mustLoad(t, cfgOther), keyring(t, 2, 1)); err != nil {
		t.Fatalf("both keys: %v", err)
	}
}

// runServeWithin runs serve with a 20-second deadline and fails the test if
// serve was still running when it expired.
func runServeWithin(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	root := newRootCmd()
	var o, e bytes.Buffer
	root.SetOut(&o)
	root.SetErr(&e)
	root.SetArgs(append([]string{"serve"}, args...))
	err = root.ExecuteContext(ctx)
	if ctx.Err() != nil {
		t.Fatalf("serve ran until the deadline (the startup check did not refuse): %v\n%s", err, e.String())
	}
	return o.String(), e.String(), err
}

func mustLoad(t *testing.T, path string) *config.Config {
	t.Helper()
	cfg, err := config.Load(t.Context(), config.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// ANGELIA_SECRETS_KEY is unset once read (ADR 0024 §4).
func TestLoadKeysUnsetsEnvKey(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(masterKey(3))
	t.Setenv(config.KeyEnv, key)
	cfg, err := config.Load(t.Context(), config.Options{Environ: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	keys, err := loadKeys(cfg, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil || keys.PrimaryID() != keyring(t, 3).PrimaryID() {
		t.Fatalf("env key: %v", err)
	}
	if _, set := os.LookupEnv(config.KeyEnv); set {
		t.Fatal("ANGELIA_SECRETS_KEY is still set")
	}
	if strings.Contains(logs.String(), key) || strings.Contains(fmt.Sprintf("%+v", cfg), key) {
		t.Fatal("the key was logged")
	}
}

func TestMissingSecretRefsWarn(t *testing.T) {
	_, db := instanceFiles(t, "")
	seedSecrets(t, db, keyring(t, 1), "present")
	st, err := sqlite.Open(t.Context(), db, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	path := writeTemp(t, "w.yaml", "version: 1\nsinks:\n"+
		"  - {name: a, kind: webhook, config: {url: 'https://a.example.org/x', secret_ref: present}}\n"+
		"  - {name: b, kind: webhook, config: {url: 'https://b.example.org/x', secret_ref: absent}}\n")
	f, err := parseWatchFile(&config.Config{}, path)
	if err != nil {
		t.Fatal(err)
	}
	w := missingSecretRefs(t.Context(), st, f)
	if len(w) != 1 || !strings.Contains(w[0], "sink b: config.secret_ref: no stored secret named absent") {
		t.Fatalf("warnings: %q", w)
	}
}

func TestConfigCheckSecrets(t *testing.T) {
	t.Setenv("ANGELIA_PLUGINS_DIR", filepath.Join(t.TempDir(), "none"))
	cfg, db := instanceFiles(t, "")
	out, _, err := runCLI(t, "config", "check", "--config", cfg)
	if err != nil || !strings.Contains(out, "secrets: no key configured (0 stored)") {
		t.Fatalf("no key, no database: %v %s", err, out)
	}
	if _, err := os.Stat(db); err == nil {
		t.Fatal("config check created the database")
	}
	seedSecrets(t, db, keyring(t, 1), "a", "b")
	_, _, err = runCLI(t, "config", "check", "--config", cfg)
	if err == nil || err.Error() != "secrets: 2 stored secrets but no key configured; set secrets.key_file" {
		t.Fatalf("rows, no key: %v", err)
	}
	key := writeKeyFile(t, 1)
	withKey := writeCfg(t, "database:\n  dsn: sqlite://"+db+"\nsecrets:\n  key_file: "+key+"\n")
	out, _, err = runCLI(t, "config", "check", "--config", withKey)
	if err != nil || !strings.Contains(out, "secrets: key "+keyring(t, 1).PrimaryID()+" (2 stored)") {
		t.Fatalf("with key: %v %s", err, out)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(key, 0o644); err != nil { //nolint:gosec // the loose mode under test
			t.Fatal(err)
		}
		_, _, err = runCLI(t, "config", "check", "--config", withKey)
		if err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Fatalf("world-readable key: %v", err)
		}
		if err := os.Chmod(key, 0o640); err != nil { //nolint:gosec // the loose mode under test
			t.Fatal(err)
		}
		_, errOut, err := runCLI(t, "config", "check", "--config", withKey)
		if err != nil || !strings.Contains(errOut, "warning: secrets:") {
			t.Fatalf("group-readable key: %v %s", err, errOut)
		}
	}
	bad := writeCfg(t, "database:\n  dsn: sqlite://"+db+"\nsecrets:\n  key_file: "+writeTemp(t, "bad.key", cliCanary)+"\n")
	_, errOut, err := runCLI(t, "config", "check", "--config", bad)
	if err == nil || !strings.Contains(err.Error(), "not a secrets key") {
		t.Fatalf("bad key file: %v", err)
	}
	noCanary(t, "config check", errOut, err.Error())
	// NIT3 (#75 review): a Postgres store is not counted, and says so.
	pg := writeCfg(t, "database:\n  dsn: postgres://angelia@db.example.com/angelia\nsecrets:\n  key_file: "+writeKeyFile(t, 2)+"\n")
	out, _, err = runCLI(t, "config", "check", "--config", pg)
	if err != nil || !strings.Contains(out, "secrets: key "+keyring(t, 2).PrimaryID()+"; stored secrets not checked (postgres)\n") {
		t.Fatalf("postgres: %v %s", err, out)
	}
	pgNone := writeCfg(t, "database:\n  dsn: postgres://angelia@db.example.com/angelia\n")
	if out, _, err = runCLI(t, "config", "check", "--config", pgNone); err != nil || !strings.Contains(out, "secrets: no key configured; stored secrets not checked (postgres)\n") {
		t.Fatalf("postgres, no key: %v %s", err, out)
	}
}

// NIT5 (#75 review): the instance-side commands never create a database, so
// a mistyped DSN is an error, not a new empty database.
func TestSecretsCommandsNeedADatabase(t *testing.T) {
	cfg, db := instanceFiles(t, "secrets:\n  key_file: "+writeKeyFile(t, 1)+"\n")
	for _, cmd := range []string{"status", "rotate"} {
		_, _, err := runCLI(t, "secrets", cmd, "--config", cfg)
		if !errors.Is(err, sqlite.ErrNoDatabase) || !strings.Contains(err.Error(), db) {
			t.Errorf("%s: %v", cmd, err)
		}
		if _, err := os.Stat(db); err == nil {
			t.Fatalf("secrets %s created the database", cmd)
		}
	}
	// On a database the secret migration has not reached, status reads no row.
	seedSecrets(t, db, keyring(t, 1))
	if out, _, err := runCLI(t, "secrets", "status", "--config", cfg); err != nil || !strings.Contains(out, "(0 stored)") {
		t.Fatalf("status: %v %s", err, out)
	}
}
