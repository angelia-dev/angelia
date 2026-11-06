package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// keyCanary is a synthetic ANGELIA_SECRETS_KEY value.
const keyCanary = "S0VZQ0FOQVJZa2V5Y2FuYXJ5S0VZQ0FOQVJZa2V5Y2E="

func TestSecretsKeys(t *testing.T) {
	cfg, err := load(t, writeFile(t, "secrets:\n  key_file: /run/secrets/angelia.key\n  previous_key_files: [/run/secrets/old.key]\n"))
	if err != nil {
		t.Fatal(err)
	}
	kc := cfg.Secrets.KeyConfig()
	if kc.KeyFile != "/run/secrets/angelia.key" || len(kc.PreviousKeyFiles) != 1 || kc.PreviousKeyFiles[0] != "/run/secrets/old.key" || !kc.Key.IsZero() {
		t.Fatalf("KeyConfig = %+v", kc)
	}

	cfg, err = load(t, "", "ANGELIA_SECRETS_KEY="+keyCanary, "ANGELIA_SECRETS_PREVIOUS_KEY_FILES=/a.key, /b.key,")
	if err != nil {
		t.Fatal(err)
	}
	if string(cfg.Secrets.Key.Reveal()) != keyCanary || strings.Join(cfg.Secrets.PreviousKeyFiles, "|") != "/a.key|/b.key" {
		t.Fatalf("env: %+v", cfg.Secrets)
	}
	var sb strings.Builder
	slog.New(slog.NewJSONHandler(&sb, nil)).Info("cfg", "cfg", cfg)
	b, _ := json.Marshal(cfg)
	for what, out := range map[string]string{"%v": fmt.Sprintf("%v", cfg), "%+v": fmt.Sprintf("%+v", *cfg), "%#v": fmt.Sprintf("%#v", *cfg), "slog": sb.String(), "json": string(b)} {
		if strings.Contains(out, keyCanary) || strings.Contains(out, "S0VZQ0FO") {
			t.Fatalf("%s leaks the key: %s", what, out)
		}
	}

	_, err = load(t, writeFile(t, "secrets:\n  key_file: /k\n"), "ANGELIA_SECRETS_KEY="+keyCanary)
	if err == nil || !strings.Contains(err.Error(), "both") || strings.Contains(err.Error(), keyCanary) {
		t.Fatalf("both set: %v", err)
	}
	_, err = load(t, writeFile(t, "secrets:\n  previous_key_files: [/old.key]\n"))
	if err == nil || !strings.Contains(err.Error(), "previous_key_files") {
		t.Fatalf("previous only: %v", err)
	}
	_, err = load(t, writeFile(t, "secrets:\n  key_file: /k\n  previous_key_files: ['']\n"))
	if err == nil || !strings.Contains(err.Error(), "previous_key_files[0]") {
		t.Fatalf("empty previous: %v", err)
	}
	// An empty ANGELIA_SECRETS_KEY counts as unset.
	if cfg, err := load(t, "", "ANGELIA_SECRETS_KEY="); err != nil || !cfg.Secrets.Key.IsZero() {
		t.Fatalf("empty env key: %v", err)
	}
}
