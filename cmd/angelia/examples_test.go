package main

import (
	"context"
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
