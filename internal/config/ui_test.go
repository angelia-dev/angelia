package config

import "testing"

// ui.enabled defaults to false, can be set, and unknown ui keys are refused.
func TestUIEnabled(t *testing.T) {
	cfg, err := load(t, writeFile(t, ""))
	if err != nil || cfg.UI.Enabled {
		t.Fatalf("default: %+v, %v", cfg, err)
	}
	cfg, err = load(t, writeFile(t, "ui:\n  enabled: true\n"))
	if err != nil || !cfg.UI.Enabled {
		t.Fatalf("enabled: %+v, %v", cfg, err)
	}
	if _, err = load(t, writeFile(t, "ui:\n  enabled: true\n  nope: 1\n")); err == nil {
		t.Fatal("unknown ui key accepted")
	}
	if _, err = load(t, writeFile(t, "ui:\n  enabled: maybe\n")); err == nil {
		t.Fatal("non-bool ui.enabled accepted")
	}
}
