package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/config"
)

// The admin UI example (docs/admin-ui.md) passes config check once its client
// secret variable is set; the secret itself is a test value.
func TestAdminUIExampleConfigChecks(t *testing.T) {
	t.Setenv("ANGELIA_UI_GITHUB_CLIENT_SECRET", "test-client-secret-0001")
	out, errb, err := runCheck(t, "--config", filepath.Join("..", "..", "examples", "admin-ui.yaml"))
	if err != nil || !strings.Contains(out, "configuration OK") {
		t.Fatalf("out=%q err=%q %v", out, errb, err)
	}
}

// AC (review S6): the example configs of deploy/compose load with the real
// instance-config loader and the real watch-file parser, so they cannot drift
// from the code silently.
func TestComposeExampleConfigsLoadWithTheRealLoaders(t *testing.T) {
	for _, dir := range []string{"sqlite", "full", "ci"} {
		t.Run(dir, func(t *testing.T) {
			base := filepath.Join("..", "..", "deploy", "compose", dir)
			cfg, err := config.Load(context.Background(), config.Options{
				Path:    filepath.Join(base, "angelia.yaml"),
				Environ: []string{},
			})
			if err != nil {
				t.Fatalf("config: %v", err)
			}
			if cfg.Listen != config.DefaultListen {
				t.Errorf("listen = %q, want %q (the compose files publish 8080)", cfg.Listen, config.DefaultListen)
			}
			if _, err := parseWatchFile(cfg, filepath.Join(base, "watches.yaml")); err != nil {
				t.Fatalf("watch file: %v", err)
			}
		})
	}
}

// The example of `angelia import` (docs/cli.md, "Import") is what the command
// prints for its manifests, and passes the watch-file loader of serve --watches.
func TestImportExampleIsCurrent(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	out, errOut, err := runCLI(t, "import", "examples/import/requirements.txt", "--dev", "examples/import/requirements-dev.txt",
		"--project", "shop", "--interval", "6h")
	if err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	want, err := os.ReadFile("examples/import/watches.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Errorf("examples/import/watches.yaml is stale; regenerate it with the command in examples/import/requirements.txt:\n%s", out)
	}
	cfg, err := config.Load(context.Background(), config.Options{Environ: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseWatchFile(cfg, "examples/import/watches.yaml"); err != nil {
		t.Fatalf("watch file: %v", err)
	}
}
