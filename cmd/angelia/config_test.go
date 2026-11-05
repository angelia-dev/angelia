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

// docBlock returns the body of the first fenced block with the given
// opening fence that follows heading in docs/configuration.md.
func docBlock(t *testing.T, heading, fence string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "configuration.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, heading)
	if i < 0 {
		t.Fatalf("heading %q not found", heading)
	}
	j := strings.Index(s[i:], fence+"\n")
	if j < 0 {
		t.Fatalf("no %s block after %q", fence, heading)
	}
	body := s[i+j+len(fence)+1:]
	return body[:strings.Index(body, "```\n")]
}

// Review S2 (#184): the documented grants.yaml is valid, and the documented
// config check output is what it prints.
func TestConfigCheckDocumentedExample(t *testing.T) {
	grants := docBlock(t, "### `grants.yaml`", "```yaml")
	want := docBlock(t, "### `angelia config check` output", "```")
	dir := t.TempDir()
	writePlugin(t, dir, "crates", `{"kind":"crates","allowed_hosts":["crates.io"]}`, grants)
	t.Setenv("ANGELIA_CONFIG", "")
	t.Setenv("ANGELIA_PLUGINS_DIR", dir)
	t.Setenv("CRATES_MIRROR_TOKEN", "mirror-token-123456")
	t.Setenv("GITLAB_TOKEN", "")
	out, errb, err := runCheck(t)
	if err != nil {
		t.Fatalf("err = %v, stderr %q", err, errb)
	}
	want = strings.Replace(want, "/var/lib/angelia/plugins", dir, 1)
	if out != want {
		t.Errorf("out =\n%s\ndocumented\n%s", out, want)
	}
}

// Q3 (#184): an unreadable default plugins_dir is a warning; a configured
// one fails the check.
func TestConfigCheckUnreadablePluginsDir(t *testing.T) {
	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "plugins"), nil, 0o600); err != nil { // not a directory
		t.Fatal(err)
	}
	t.Setenv("ANGELIA_CONFIG", "")
	t.Setenv("ANGELIA_PLUGINS_DIR", "")
	t.Setenv("ANGELIA_DATABASE_DSN", "sqlite://"+filepath.Join(data, "angelia.db"))
	out, errb, err := runCheck(t)
	if err != nil || !strings.Contains(out, "not readable, not checked") || !strings.Contains(errb, "warning: plugins_dir cannot be read") {
		t.Fatalf("default: out=%q stderr=%q err=%v", out, errb, err)
	}

	t.Setenv("ANGELIA_PLUGINS_DIR", filepath.Join(data, "plugins"))
	out, errb, err = runCheck(t)
	if err == nil || strings.Contains(out, "configuration OK") || !strings.Contains(errb, "invalid plugins: plugins_dir cannot be read") {
		t.Fatalf("configured: out=%q stderr=%q err=%v", out, errb, err)
	}
}
