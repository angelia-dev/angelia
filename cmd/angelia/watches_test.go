package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
)

const cliWatches = `version: 1
sinks:
  - {name: hook, kind: webhook, config: {url: 'https://h.example.org/x', secret_env: HOOK}}
watches:
  - {name: a, source: pypi, coordinates: {name: requests}}
  - {name: b, source: npm, coordinates: {name: react}}
subscriptions:
  - {name: all, sink: hook}
`

func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func countWatches(t *testing.T, db string) int {
	t.Helper()
	st, err := sqlite.Open(t.Context(), db, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	n := 0
	if err := st.InConfigTx(t.Context(), func(tx store.ConfigTx) error {
		ws, err := tx.ListManagedWatches(t.Context(), store.ManagedByFile)
		n = len(ws)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// Removing a watch from the file removes it on the next start.
func TestApplyWatchFileOnRestart(t *testing.T) {
	t.Setenv("ANGELIA_CONFIG", "")
	db := filepath.Join(t.TempDir(), "a.db")
	cfg := config.Default()
	cfg.Database.DSN = "sqlite://" + db
	log := newLogger(&syncBuf{}, cfg.Log)

	path := writeFile(t, "watches.yaml", cliWatches)
	if err := applyWatchFile(context.Background(), cfg, path, log); err != nil {
		t.Fatal(err)
	}
	if n := countWatches(t, db); n != 2 {
		t.Fatalf("watches = %d, want 2", n)
	}
	path = writeFile(t, "watches.yaml", strings.Replace(cliWatches, "  - {name: b, source: npm, coordinates: {name: react}}\n", "", 1))
	if err := applyWatchFile(context.Background(), cfg, path, log); err != nil {
		t.Fatal(err)
	}
	if n := countWatches(t, db); n != 1 {
		t.Fatalf("watches after removal = %d, want 1", n)
	}
}

func TestApplyWatchFileErrors(t *testing.T) {
	cfg := config.Default()
	cfg.Database.DSN = "postgres://db.example.org/angelia"
	log := newLogger(&syncBuf{}, cfg.Log)
	ctx := context.Background()
	if err := applyWatchFile(ctx, cfg, writeFile(t, "w.yaml", cliWatches), log); err == nil || !strings.Contains(err.Error(), "only sqlite") {
		t.Fatalf("postgres: %v", err)
	}
	if err := applyWatchFile(ctx, cfg, writeFile(t, "w.yaml", "version: 1\nwatches: [{name: a}]\n"), log); err == nil || !strings.Contains(err.Error(), "line 2: watches[0].source: missing required key") {
		t.Fatalf("invalid file: %v", err)
	}
	if err := applyWatchFile(ctx, cfg, "/nonexistent.yaml", log); err == nil {
		t.Fatal("missing file")
	}
}

func TestServeWatchesNeedsAPIRole(t *testing.T) {
	t.Setenv("ANGELIA_CONFIG", "")
	t.Setenv("ANGELIA_WATCHES", writeFile(t, "w.yaml", cliWatches))
	cfg := writeCfg(t, fmt.Sprintf("listen: %q\ndatabase:\n  dsn: sqlite://%s\n", freeAddr(t), filepath.Join(t.TempDir(), "a.db")))
	var errb syncBuf
	root := newRootCmd()
	root.SetOut(&errb)
	root.SetErr(&errb)
	root.SetArgs([]string{"serve", "--config", cfg, "--roles=worker"})
	err := root.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "applied by the api role") {
		t.Fatalf("err = %v", err)
	}
}

func TestConfigCheckWatches(t *testing.T) {
	t.Setenv("ANGELIA_CONFIG", "")
	t.Setenv("ANGELIA_WATCHES", "")
	run := func(args ...string) (string, error) {
		var out syncBuf
		root := newRootCmd()
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(append([]string{"config", "check"}, args...))
		err := root.ExecuteContext(context.Background())
		return out.String(), err
	}
	out, err := run("--watches", writeFile(t, "w.yaml", cliWatches))
	if err != nil || !strings.Contains(out, "watch file OK: 1 sinks, 2 watches, 1 subscriptions") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if _, err := run("--watches", writeFile(t, "w.yaml", "version: 2\n")); err == nil || !strings.Contains(err.Error(), "line 1: version: must be 1") {
		t.Fatalf("err = %v", err)
	}
}
