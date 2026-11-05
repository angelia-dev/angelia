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

// writePlugin writes <dir>/<kind>/manifest.json and grants.yaml.
func writePlugin(t *testing.T, dir, kind, manifest, grants string) {
	t.Helper()
	d := filepath.Join(dir, kind)
	if err := os.MkdirAll(d, 0o755); err != nil { //nolint:gosec // G301: test fixture.
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "manifest.json"), []byte(manifest), 0o644); err != nil { //nolint:gosec // G306: test fixture.
		t.Fatal(err)
	}
	if grants != "" {
		if err := os.WriteFile(filepath.Join(d, "grants.yaml"), []byte(grants), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// AC (Q4, #184): config check prints, for each plugin, its kind, its hosts
// and its credential hosts with scheme and header, never a token value.
func TestConfigCheckPrintsPluginHosts(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "crates", `{"kind":"crates","allowed_hosts":["crates.io"]}`, `
allowed_hosts: [crates-mirror.example.com]
credentials:
  crates-mirror.example.com:
    token_env: ANGELIA_TEST_MIRROR_TOK
    scheme: Bearer
  crates-mirror.example.com:8443:
    token_env: ANGELIA_TEST_UNSET_TOK
    header: PRIVATE-TOKEN
`)
	writePlugin(t, dir, "bare", `{"kind":"bare","allowed_hosts":[]}`, "")
	t.Setenv("ANGELIA_CONFIG", "")
	t.Setenv("ANGELIA_PLUGINS_DIR", dir)
	t.Setenv("ANGELIA_TEST_MIRROR_TOK", "tok-value-123456")
	out, errb, err := runCheck(t)
	if err != nil {
		t.Fatalf("err = %v, stderr %q", err, errb)
	}
	want := "configuration OK\n" +
		"plugins in " + dir + ": 2\n" +
		"  bare\n" +
		"    allowed hosts: none\n" +
		"    credentials: none\n" +
		"  crates\n" +
		"    allowed hosts: crates.io (manifest), crates-mirror.example.com (grants)\n" +
		"    credentials:\n" +
		"      crates-mirror.example.com:443  Authorization: Bearer <token from $ANGELIA_TEST_MIRROR_TOK>\n" +
		"      crates-mirror.example.com:8443  PRIVATE-TOKEN: <token from $ANGELIA_TEST_UNSET_TOK, not set>\n"
	if out != want {
		t.Errorf("out =\n%s\nwant\n%s", out, want)
	}
	if strings.Contains(out+errb, "tok-value-123456") {
		t.Error("a token value was printed")
	}
	if !strings.Contains(errb, "warning:") || !strings.Contains(errb, "ANGELIA_TEST_UNSET_TOK is not set") {
		t.Errorf("stderr = %q, want the unset variable warning", errb)
	}
}

func TestConfigCheckNoPlugins(t *testing.T) {
	t.Setenv("ANGELIA_CONFIG", "")
	dir := filepath.Join(t.TempDir(), "absent")
	t.Setenv("ANGELIA_PLUGINS_DIR", dir)
	out, _, err := runCheck(t)
	if err != nil || out != "configuration OK\nplugins in "+dir+": none\n" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestConfigCheckBadPluginFails(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "crates", `{"kind":"crates"}`, "bogus: 1\n")
	t.Setenv("ANGELIA_CONFIG", "")
	t.Setenv("ANGELIA_PLUGINS_DIR", dir)
	out, errb, err := runCheck(t)
	if err == nil || strings.Contains(out, "configuration OK") || !strings.Contains(errb, "invalid plugins") || !strings.Contains(errb, "grants.yaml") {
		t.Fatalf("out=%q stderr=%q err=%v", out, errb, err)
	}
}

func TestConfigCheckOldPluginsKeyFails(t *testing.T) {
	p := writeCfg(t, "plugins_dir: /srv/p\nplugins:\n  crates: {}\n")
	_, errb, err := runCheck(t, "--config", p)
	if err == nil || !strings.Contains(errb, "moved") || !strings.Contains(errb, "/srv/p/crates/grants.yaml") {
		t.Fatalf("stderr=%q err=%v", errb, err)
	}
}
