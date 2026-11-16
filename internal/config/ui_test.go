package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A complete, valid ui section (ADR 0033 §3).
const uiOK = `
ui:
  enabled: true
  public_url: https://angelia.example.com
  github:
    client_id: Ov23liEXAMPLE
    client_secret_env: UI_GITHUB_SECRET
  access:
    - github_user: mmornati
      role: admin
    - github_id: 139323
      role: viewer
`

func TestUIDefaultsAndValid(t *testing.T) {
	cfg, err := load(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UI.Enabled || cfg.UI.Session.IdleTimeout != 12*time.Hour || cfg.UI.Session.MaxLifetime != 7*24*time.Hour {
		t.Fatalf("defaults: %+v", cfg.UI)
	}
	cfg, err = load(t, writeFile(t, uiOK+"  session:\n    idle_timeout: 1h\n    max_lifetime: 24h\n"), "UI_GITHUB_SECRET=s3cr3t-value")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UI.Session.IdleTimeout != time.Hour || cfg.UI.Session.MaxLifetime != 24*time.Hour || len(cfg.UI.Access) != 2 {
		t.Fatalf("ui: %+v", cfg.UI)
	}
	v, w, err := cfg.UI.GitHub.ClientSecret()
	if err != nil || w != "" || string(v.Reveal()) != "s3cr3t-value" {
		t.Fatalf("secret: %v %q %v", string(v.Reveal()), w, err)
	}
	if s := fmt.Sprintf("%+v %v", cfg, cfg.UI); strings.Contains(s, "s3cr3t-value") {
		t.Fatal("the client secret leaks in formatting")
	}
	if cfg.UI.PublicOrigin() != "https://angelia.example.com" || cfg.UI.LocalDev() {
		t.Fatalf("origin %q, local %v", cfg.UI.PublicOrigin(), cfg.UI.LocalDev())
	}
}

// A disabled section is not checked, so it may be left half-written.
func TestUIDisabledIsNotChecked(t *testing.T) {
	if _, err := load(t, writeFile(t, "ui:\n  enabled: false\n  public_url: nope\n")); err != nil {
		t.Fatal(err)
	}
}

func TestUIValidation(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"no public_url":     {strings.Replace(uiOK, "  public_url: https://angelia.example.com\n", "", 1), "ui.public_url: required"},
		"public_url path":   {strings.Replace(uiOK, "angelia.example.com", "angelia.example.com/ui", 1), "without a path"},
		"public_url http":   {strings.Replace(uiOK, "https://angelia.example.com", "http://angelia.example.com", 1), "loopback host only"},
		"public_url scheme": {strings.Replace(uiOK, "https://angelia.example.com", "ftp://angelia.example.com", 1), "absolute https URL"},
		"no client id":      {strings.Replace(uiOK, "    client_id: Ov23liEXAMPLE\n", "", 1), "ui.github.client_id: required"},
		"no secret":         {strings.Replace(uiOK, "    client_secret_env: UI_GITHUB_SECRET\n", "", 1), "set client_secret_env"},
		"both secrets":      {strings.Replace(uiOK, "    client_secret_env: UI_GITHUB_SECRET\n", "    client_secret_env: A\n    client_secret_file: /run/s\n", 1), "not both"},
		"secret env value":  {strings.Replace(uiOK, "UI_GITHUB_SECRET", "abc-def", 1), "must be an environment variable name"},
		"api_url http":      {strings.Replace(uiOK, "    client_id:", "    api_url: http://ghe.example.com/api/v3\n    client_id:", 1), "ui.github.api_url"},
		"no admin":          {strings.Replace(uiOK, "      role: admin\n", "      role: editor\n", 1), "at least one entry with role admin"},
		"unknown role":      {strings.Replace(uiOK, "      role: viewer\n", "      role: owner\n", 1), `role "owner"`},
		"user and id":       {strings.Replace(uiOK, "    - github_id: 139323\n", "    - github_id: 139323\n      github_user: x\n", 1), "not both"},
		"neither":           {strings.Replace(uiOK, "    - github_id: 139323\n", "    - github_id: 0\n", 1), "set github_user"},
		"bad login":         {strings.Replace(uiOK, "mmornati", "-bad-", 1), "not a GitHub login"},
		"negative id":       {strings.Replace(uiOK, "139323", "-5", 1), "positive"},
		"idle > max":        {uiOK + "  session:\n    idle_timeout: 48h\n    max_lifetime: 24h\n", "longer than"},
		"short idle":        {uiOK + "  session:\n    idle_timeout: 1s\n", "at least 1m"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, writeFile(t, tc.yaml), "UI_GITHUB_SECRET=x")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// An inline client_secret is refused with its own message, which never
// echoes the value, even when the section is disabled.
func TestUIInlineClientSecretRefused(t *testing.T) {
	for _, enabled := range []string{"true", "false"} {
		y := "ui:\n  enabled: " + enabled + "\n  github:\n    client_id: x\n    client_secret: hunter2-secret-value\n"
		_, err := load(t, writeFile(t, y))
		if err == nil || !strings.Contains(err.Error(), "line 5: ui.github.client_secret: an inline client secret is refused") {
			t.Fatalf("enabled=%s: %v", enabled, err)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Fatal("the error echoes the secret")
		}
	}
	// Other keys keep their strict check.
	if _, err := load(t, writeFile(t, "ui:\n  github:\n    client_ids: x\n")); err == nil || !strings.Contains(err.Error(), "client_ids") {
		t.Fatalf("unknown key: %v", err)
	}
}

func TestUIUnsetSecretEnvWarns(t *testing.T) {
	cfg, err := load(t, writeFile(t, uiOK))
	if err != nil {
		t.Fatal(err)
	}
	if w := cfg.Warnings(); len(w) != 1 || !strings.Contains(w[0], "UI_GITHUB_SECRET is not set") {
		t.Fatalf("warnings: %v", w)
	}
	if _, _, err := cfg.UI.GitHub.ClientSecret(); err == nil {
		t.Fatal("ClientSecret with an unset variable")
	}
}

func TestUIClientSecretFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	g := UIGitHub{ClientSecretFile: write("ok", "file-secret\n", 0o600)}
	v, w, err := g.ClientSecret()
	if err != nil || w != "" || string(v.Reveal()) != "file-secret" {
		t.Fatalf("%q %q %v", v.Reveal(), w, err)
	}
	for name, content := range map[string]string{"empty": "", "two lines": "a\nb\n", "too large": strings.Repeat("x", 5000)} {
		g := UIGitHub{ClientSecretFile: write(name, content, 0o600)}
		if _, _, err := g.ClientSecret(); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "xxxx") {
			t.Errorf("%s: the error holds the content", name)
		}
	}
	if _, _, err := (UIGitHub{ClientSecretFile: dir}).ClientSecret(); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory: %v", err)
	}
	if _, _, err := (UIGitHub{ClientSecretFile: filepath.Join(dir, "missing")}).ClientSecret(); err == nil {
		t.Fatal("missing file accepted")
	}
	if _, _, err := (UIGitHub{}).ClientSecret(); err == nil {
		t.Fatal("no secret configured")
	}
	if runtime.GOOS == "windows" {
		return
	}
	g = UIGitHub{ClientSecretFile: write("readable", "s\n", 0o644)}
	if _, w, err := g.ClientSecret(); err != nil || !strings.Contains(w, "readable by group or others") {
		t.Fatalf("readable: %q %v", w, err)
	}
	g = UIGitHub{ClientSecretFile: write("writable", "s\n", 0o666)}
	if _, _, err := g.ClientSecret(); err == nil || !strings.Contains(err.Error(), "writable by group or others") {
		t.Fatalf("writable: %v", err)
	}
}

func TestUILocalDevAndOrigin(t *testing.T) {
	for url, want := range map[string]struct {
		origin string
		local  bool
	}{
		"http://localhost:8080":            {"http://localhost:8080", true},
		"http://127.0.0.1:8080/":           {"http://127.0.0.1:8080", true},
		"http://[::1]:8080":                {"http://[::1]:8080", true},
		"HTTPS://Angelia.Example.com:443":  {"https://angelia.example.com", false},
		"https://angelia.example.com:8443": {"https://angelia.example.com:8443", false},
		"http://angelia.example.com":       {"http://angelia.example.com", false},
		"::nope":                           {"", false},
	} {
		u := UI{PublicURL: url}
		if u.PublicOrigin() != want.origin || u.LocalDev() != want.local {
			t.Errorf("%s: origin %q local %v, want %+v", url, u.PublicOrigin(), u.LocalDev(), want)
		}
	}
	y := strings.Replace(uiOK, "https://angelia.example.com", "http://localhost:8080", 1)
	if _, err := load(t, writeFile(t, y), "UI_GITHUB_SECRET=x"); err != nil {
		t.Fatalf("loopback http: %v", err)
	}
}
