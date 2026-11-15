package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/apitoken"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/open"
)

func TestTokenGenerate(t *testing.T) {
	out, errOut, err := runCLI(t, "token", "generate")
	if err != nil || errOut != "" {
		t.Fatalf("generate: %v %q", err, errOut)
	}
	tok, ok := strings.CutSuffix(out, "\n")
	if !ok {
		t.Fatalf("no newline: %q", out)
	}
	if _, err := apitoken.Parse(tok); err != nil {
		t.Fatalf("not a token: %q", out)
	}
	// The output is a valid bootstrap file.
	if _, _, _, err := apitoken.ReadBootstrapFile(writeTemp(t, "tok", out)); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "bootstrap")
	out, errOut, err = runCLI(t, "token", "generate", "--out", path)
	if err != nil || out != "" || !strings.Contains(errOut, "mode 0600") {
		t.Fatalf("--out: %v %q %q", err, out, errOut)
	}
	id, written, warn, err := apitoken.ReadBootstrapFile(path)
	if err != nil || warn != "" || !strings.Contains(errOut, id) || strings.Contains(errOut, written) {
		t.Fatalf("--out file: %v %q %q", err, warn, errOut)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("mode: %v %v", info.Mode(), err)
		}
	}
	if _, _, err := runCLI(t, "token", "generate", "--out", path); err == nil {
		t.Fatal("--out overwrote a file")
	}
	if _, again, _, _ := apitoken.ReadBootstrapFile(path); again != written {
		t.Fatal("the file changed")
	}
}

// tokenInstance writes an instance config on dsn, with no secrets key, and
// returns its path.
func tokenInstance(t *testing.T, dsn string) string {
	t.Helper()
	isolate(t)
	t.Setenv("ANGELIA_CONFIG", "")
	t.Setenv("ANGELIA_SECRETS_KEY", "")
	return writeCfg(t, yamlDSN(dsn))
}

func migrateDSN(t *testing.T, dsn string) open.Store {
	t.Helper()
	st, err := open.Open(t.Context(), dsn, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// checkCreateLocal runs `token create --local` on a migrated store and checks
// the output and the row.
func checkCreateLocal(t *testing.T, dsn string, st open.Store) {
	t.Helper()
	cfg := tokenInstance(t, dsn)
	out, errOut, err := runCLI(t, "token", "create", "--local", "--config", cfg, "--name", "me")
	if err != nil {
		t.Fatalf("create --local: %v\n%s", err, errOut)
	}
	tok, ok := strings.CutSuffix(out, "\n")
	if !ok || strings.Contains(tok, "\n") {
		t.Fatalf("stdout is not the token alone: %q", out)
	}
	id, err := apitoken.Parse(tok)
	if err != nil {
		t.Fatalf("stdout is not a token: %q", out)
	}
	if strings.Contains(errOut, tok) || !strings.Contains(errOut, id) || !strings.Contains(errOut, "scope admin") ||
		!strings.Contains(errOut, "name me") || !strings.Contains(errOut, "expires never") || !strings.Contains(errOut, "shown once") {
		t.Fatalf("stderr: %q", errOut)
	}
	row, err := st.APITokenByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Name != "me" || row.Scope != "admin" || row.CreatedBy != "local" || !bytes.Equal(row.Hash, apitoken.Hash(tok)) || row.ExpiresAt != nil {
		t.Fatalf("row %+v", row)
	}

	// Another active "me" is a conflict; a read token with an expiry is not.
	_, errOut, err = runCLI(t, "token", "create", "--local", "--config", cfg, "--name", "me")
	if err == nil || !strings.Contains(err.Error(), `already named "me"`) {
		t.Fatalf("duplicate name: %v %q", err, errOut)
	}
	out, _, err = runCLI(t, "token", "create", "--local", "--config", cfg, "--name", "ci.read", "--scope", "read", "--expires-in", "90d")
	if err != nil {
		t.Fatal(err)
	}
	id, _ = apitoken.Parse(strings.TrimSpace(out))
	row, err = st.APITokenByID(t.Context(), id)
	if err != nil || row.Scope != "read" || row.ExpiresAt == nil || time.Until(*row.ExpiresAt) < 89*24*time.Hour {
		t.Fatalf("read token %+v %v", row, err)
	}
}

// `token create --local` on SQLite works with stored secrets and no secrets
// key: it loads no keyring (where `serve` would refuse to start).
func TestTokenCreateLocalSQLite(t *testing.T) {
	db := filepath.Join(t.TempDir(), "angelia.db")
	seedSecrets(t, db, keyring(t, 1), "a")
	st := migrateDSN(t, "sqlite://"+db)
	checkCreateLocal(t, "sqlite://"+db, st)
}

func TestTokenCreateLocalPostgres(t *testing.T) {
	dsn, _, _ := pgSchema(t)
	st := migrateDSN(t, dsn)
	seedPGSecrets(t, dsn, keyring(t, 1), "a")
	checkCreateLocal(t, dsn, st)
}

// Flags are checked before anything is opened; a missing database is an
// error and is not created; without --local the command refuses.
func TestTokenCreateLocalErrors(t *testing.T) {
	db := filepath.Join(t.TempDir(), "missing.db")
	cfg := tokenInstance(t, "sqlite://"+db)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--name", "me"}, "only --local"},
		{[]string{"--local"}, `"name" not set`},
		{[]string{"--local", "--name", "Me"}, "--name"},
		{[]string{"--local", "--name", "_me"}, "--name"},
		{[]string{"--local", "--name", strings.Repeat("a", 64)}, "--name"},
		{[]string{"--local", "--name", "me", "--scope", "root"}, "--scope"},
		{[]string{"--local", "--name", "me", "--expires-at", "tomorrow"}, "--expires-at"},
		{[]string{"--local", "--name", "me", "--expires-at", "2020-01-01T00:00:00Z"}, "not in the future"},
		{[]string{"--local", "--name", "me", "--expires-in", "0d"}, "--expires-in"},
		{[]string{"--local", "--name", "me", "--expires-in", "-1h"}, "not in the future"},
		{[]string{"--local", "--name", "me", "--expires-in", "soon"}, "--expires-in"},
		{[]string{"--local", "--name", "me", "--expires-in", "1d", "--expires-at", "2099-01-01T00:00:00Z"}, "none of the others"},
		{[]string{"--local", "--name", "me"}, "does not exist"},
	} {
		out, _, err := runCLI(t, append([]string{"token", "create", "--config", cfg}, c.args...)...)
		if err == nil || !strings.Contains(err.Error(), c.want) || out != "" {
			t.Errorf("%v: %v %q (want %q)", c.args, err, out, c.want)
		}
	}
	if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the database was created: %v", err)
	}
}

func TestTokenExpiry(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 123456789, time.UTC)
	got, err := tokenExpiry("", "", now)
	if err != nil || got != nil {
		t.Fatalf("none: %v %v", got, err)
	}
	got, err = tokenExpiry("2027-01-31T01:00:00+01:00", "", now)
	if err != nil || !got.Equal(time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC)) || got.Location() != time.UTC {
		t.Fatalf("at: %v %v", got, err)
	}
	got, err = tokenExpiry("", "90d", now)
	if err != nil || !got.Equal(now.Add(90*24*time.Hour).Truncate(time.Millisecond)) {
		t.Fatalf("90d: %v %v", got, err)
	}
	got, err = tokenExpiry("", "36h", now)
	if err != nil || !got.Equal(now.Add(36*time.Hour).Truncate(time.Millisecond)) {
		t.Fatalf("36h: %v %v", got, err)
	}
}

// At api role start the bootstrap file is stored once and the warning goes
// away; nothing logs the token.
func TestBootstrapAPITokens(t *testing.T) {
	db := filepath.Join(t.TempDir(), "angelia.db")
	st := migrateDSN(t, "sqlite://"+db)
	id, tok := apitoken.Generate(time.Now())
	file := writeTemp(t, "bootstrap", tok+"\n")
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))

	cfg := mustLoad(t, tokenInstance(t, "sqlite://"+db))
	if err := bootstrapAPITokens(t.Context(), cfg, st, log); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), apitoken.NoAdminWarning[:30]) {
		t.Fatalf("no warning without a file: %s", logs.String())
	}
	logs.Reset()
	cfg.API.BootstrapTokenFile = file
	for range 2 {
		if err := bootstrapAPITokens(t.Context(), cfg, st, log); err != nil {
			t.Fatal(err)
		}
	}
	if row, err := st.APITokenByID(t.Context(), id); err != nil || row.Name != "bootstrap" || row.CreatedBy != "bootstrap" {
		t.Fatalf("row %+v %v", row, err)
	}
	if strings.Contains(logs.String(), "no active admin") || strings.Contains(logs.String(), tok) || strings.Count(logs.String(), "stored") != 1 {
		t.Fatalf("logs: %s", logs.String())
	}
}

// serve refuses to start the api role with a malformed bootstrap file, and
// neither the error nor the log shows the file's content.
func TestServeRefusesMalformedBootstrapFile(t *testing.T) {
	_, tok := apitoken.Generate(time.Now())
	bad := tok[:len(tok)-1] + "x"
	if bad == tok {
		bad = tok[:len(tok)-1] + "y"
	}
	cfg, _ := instanceFiles(t, fmt.Sprintf("listen: %q\napi:\n  bootstrap_token_file: %s\n", freeAddr(t), writeTemp(t, "bootstrap", bad+"\n")))
	_, errOut, err := runServeWithin(t, "--config", cfg, "--roles", "api")
	if err == nil || !strings.Contains(err.Error(), "api.bootstrap_token_file") || !strings.Contains(err.Error(), "not an API token") {
		t.Fatalf("serve: %v\n%s", err, errOut)
	}
	for _, s := range []string{err.Error(), errOut} {
		if strings.Contains(s, bad[:40]) || strings.Contains(s, bad[31:60]) {
			t.Fatalf("the file's content leaked: %s", s)
		}
	}
}

// serve with a world-readable bootstrap file starts the api role, stores the
// token, warns about the mode, and logs no token.
func TestServeBootstrapsFromFile(t *testing.T) {
	id, tok := apitoken.Generate(time.Now())
	file := writeTemp(t, "bootstrap", tok+"\n")
	if err := os.Chmod(file, 0o644); err != nil { //nolint:gosec // G302: the world-readable case under test
		t.Fatal(err)
	}
	addr := freeAddr(t)
	cfg, db := instanceFiles(t, fmt.Sprintf("listen: %q\n", addr))
	t.Setenv("ANGELIA_API_BOOTSTRAP_TOKEN_FILE", file)
	var errb syncBuf
	root := newRootCmd()
	root.SetOut(&errb)
	root.SetErr(&errb)
	root.SetArgs([]string{"serve", "--config", cfg, "--roles=api", "--drain-timeout=2s"})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	ready := false
	for range 200 {
		if resp, err := http.Get("http://" + addr + "/readyz"); err == nil { //nolint:noctx // a local test server
			_ = resp.Body.Close()
			ready = resp.StatusCode == http.StatusOK
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil || !ready {
		t.Fatalf("serve: ready=%v %v\n%s", ready, err, errb.String())
	}
	out := errb.String()
	if !strings.Contains(out, "bootstrap admin token stored") || !strings.Contains(out, id) {
		t.Fatalf("no bootstrap log: %s", out)
	}
	if runtime.GOOS != "windows" && !strings.Contains(out, "readable by group or others") {
		t.Fatalf("no mode warning: %s", out)
	}
	if strings.Contains(out, "no active admin") || strings.Contains(out, tok) || strings.Contains(out, tok[31:70]) {
		t.Fatalf("logs: %s", out)
	}
	w, err := open.TokenWriter(t.Context(), "sqlite://"+db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if n, err := w.CountActiveAdminTokens(t.Context(), time.Now()); err != nil || n != 1 {
		t.Fatalf("admins %d %v", n, err)
	}
	if _, err := w.APITokenByID(t.Context(), id); errors.Is(err, store.ErrNotFound) {
		t.Fatal("not stored")
	}
}
