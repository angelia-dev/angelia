package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCheck(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs(append([]string{"config", "check"}, args...))
	err := root.ExecuteContext(context.Background())
	return out.String(), errb.String(), err
}

func writeCfg(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConfigCheckDefaultsOK(t *testing.T) {
	t.Setenv("ANGELIA_CONFIG", "")
	out, _, err := runCheck(t)
	if err != nil || !strings.Contains(out, "configuration OK") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestConfigCheckFileFlagOK(t *testing.T) {
	out, _, err := runCheck(t, "--config", writeCfg(t, "listen: :9\n"))
	if err != nil || !strings.Contains(out, "configuration OK") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

// AC: exits non-zero with a readable message on invalid config.
func TestConfigCheckInvalidFails(t *testing.T) {
	p := writeCfg(t, "bogus: 1\n")
	_, errb, err := runCheck(t, "--config", p)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(errb, "invalid configuration") || !strings.Contains(errb, p) || !strings.Contains(errb, "line 1") {
		t.Fatalf("unreadable message: %q", errb)
	}
}

func TestConfigCheckEnvPath(t *testing.T) {
	t.Setenv("ANGELIA_CONFIG", writeCfg(t, "log:\n  level: loud\n"))
	if _, _, err := runCheck(t); err == nil {
		t.Fatal("expected error from ANGELIA_CONFIG file")
	}
}

func TestConfigCheckInvalidEnvOverride(t *testing.T) {
	t.Setenv("ANGELIA_CONFIG", "")
	t.Setenv("ANGELIA_LISTEN", "garbage")
	if _, _, err := runCheck(t); err == nil {
		t.Fatal("expected error")
	}
}

func TestConfigCheckRejectsArgs(t *testing.T) {
	if _, _, err := runCheck(t, "extra"); err == nil {
		t.Fatal("expected error")
	}
}

func TestConfigCheckWarnsOnUnsetTokenEnv(t *testing.T) {
	p := writeCfg(t, "sources:\n  pypi:\n    token_env: ANGELIA_TEST_UNSET_TOK\n")
	out, errb, err := runCheck(t, "--config", p)
	if err != nil || !strings.Contains(out, "configuration OK") || !strings.Contains(errb, "warning:") || !strings.Contains(errb, "ANGELIA_TEST_UNSET_TOK") {
		t.Fatalf("out=%q err=%q %v", out, errb, err)
	}
}
