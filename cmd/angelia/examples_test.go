package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/angelia-dev/angelia/internal/config"
)

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
