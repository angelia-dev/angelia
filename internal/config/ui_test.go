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
		"short idle":        {uiOK + "  session:\n    idle_timeout: 1s\n", "at least 15m"},
		"idle under 15m":    {uiOK + "  session:\n    idle_timeout: 14m\n", "at least 15m"},
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

// A ui section with OIDC providers only (#268).
const uiOIDC = `
ui:
  enabled: true
  public_url: https://angelia.example.com
  oidc:
    - name: authentik
      display_name: Company SSO
      issuer: https://auth.example.com/application/o/angelia/
      client_id: angelia
      client_secret_env: UI_OIDC_SECRET
    - name: google
      issuer: https://accounts.google.com
      client_id: 123.apps.googleusercontent.com
      client_secret_env: UI_GOOGLE_SECRET
      scopes: [openid, email]
      allowed_algs: [RS256]
  access:
    - provider: authentik
      subject: 5f1c0a7e-0000-4000-8000-000000000001
      role: admin
    - provider: google
      email: Alice@Example.com
      role: viewer
`

func TestUIOIDC(t *testing.T) {
	cfg, err := load(t, writeFile(t, uiOIDC), "UI_OIDC_SECRET=oidc-secret", "UI_GOOGLE_SECRET=g")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UI.GitHub.Configured() || len(cfg.UI.OIDC) != 2 || len(cfg.UI.Access) != 2 {
		t.Fatalf("%+v", cfg.UI)
	}
	a, g := cfg.UI.OIDC[0], cfg.UI.OIDC[1]
	if strings.Join(a.EffectiveScopes(), " ") != "openid email profile" || len(a.EffectiveAlgs()) != 10 || a.Label() != "Company SSO" {
		t.Fatalf("%+v", a)
	}
	if strings.Join(g.EffectiveScopes(), " ") != "openid email" || strings.Join(g.EffectiveAlgs(), " ") != "RS256" || g.Label() != "google" {
		t.Fatalf("%+v", g)
	}
	v, w, err := a.ClientSecret()
	if err != nil || w != "" || string(v.Reveal()) != "oidc-secret" {
		t.Fatal(err)
	}
	if s := fmt.Sprintf("%+v", cfg.UI); strings.Contains(s, "oidc-secret") {
		t.Fatal("the client secret leaks in formatting")
	}
	// An unset variable warns, as for GitHub.
	cfg, err = load(t, writeFile(t, uiOIDC), "UI_GOOGLE_SECRET=g")
	if err != nil {
		t.Fatal(err)
	}
	if w := cfg.Warnings(); len(w) != 1 || !strings.Contains(w[0], "ui.oidc[0].client_secret_env: environment variable UI_OIDC_SECRET") {
		t.Fatal(w)
	}
	if _, _, err := cfg.UI.OIDC[0].ClientSecret(); err == nil || !strings.Contains(err.Error(), "ui.oidc[authentik]") {
		t.Fatal(err)
	}
	// Both GitHub and OIDC, with a loopback http issuer.
	both := strings.Replace(uiOK, "  access:\n", "  oidc:\n    - name: kc\n      issuer: http://127.0.0.1:8080/realms/x\n      client_id: a\n      client_secret_file: /run/s\n  access:\n    - provider: kc\n      email: a@b.example\n      role: editor\n", 1)
	if _, err := load(t, writeFile(t, both), "UI_GITHUB_SECRET=x"); err != nil {
		t.Fatal(err)
	}
}

func TestUIOIDCValidation(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"no provider":       {strings.Replace(uiOK, "  github:\n    client_id: Ov23liEXAMPLE\n    client_secret_env: UI_GITHUB_SECRET\n", "", 1), "configure ui.github or at least one ui.oidc"},
		"no name":           {strings.Replace(uiOIDC, "    - name: authentik\n      display_name: Company SSO\n", "    - display_name: x\n", 1), "ui.oidc[0].name: required"},
		"bad name":          {strings.Replace(uiOIDC, "name: authentik", "name: Auth_entik", 1), "lower-case letters"},
		"github name":       {strings.Replace(uiOIDC, "name: google", "name: github", 1), "github is reserved"},
		"twice":             {strings.Replace(uiOIDC, "name: google", "name: authentik", 1), "listed twice"},
		"long display":      {strings.Replace(uiOIDC, "Company SSO", strings.Repeat("x", 65), 1), "display_name"},
		"no issuer":         {strings.Replace(uiOIDC, "      issuer: https://accounts.google.com\n", "", 1), "ui.oidc[1].issuer: required"},
		"http issuer":       {strings.Replace(uiOIDC, "https://accounts.google.com", "http://accounts.google.com", 1), "loopback host only"},
		"issuer scheme":     {strings.Replace(uiOIDC, "https://accounts.google.com", "ftp://accounts.google.com", 1), "absolute https URL"},
		"issuer query":      {strings.Replace(uiOIDC, "https://accounts.google.com", "https://accounts.google.com?x=1", 1), "query"},
		"no client id":      {strings.Replace(uiOIDC, "      client_id: angelia\n", "", 1), "ui.oidc[0].client_id: required"},
		"client id space":   {strings.Replace(uiOIDC, "client_id: angelia", "client_id: an gelia", 1), "one word"},
		"no secret":         {strings.Replace(uiOIDC, "      client_secret_env: UI_OIDC_SECRET\n", "", 1), "ui.oidc[0]: set client_secret_env"},
		"secret env value":  {strings.Replace(uiOIDC, "UI_OIDC_SECRET", "abc-def", 1), "ui.oidc[0].client_secret_env: must be an environment variable name"},
		"no openid":         {strings.Replace(uiOIDC, "[openid, email]", "[email]", 1), "must contain openid"},
		"bad scope":         {strings.Replace(uiOIDC, "[openid, email]", "[openid, \"a b\"]", 1), "not a scope token"},
		"HS256":             {strings.Replace(uiOIDC, "[RS256]", "[RS256, HS256]", 1), `"HS256" is not accepted`},
		"none":              {strings.Replace(uiOIDC, "[RS256]", "[none]", 1), `"none" is not accepted`},
		"unknown provider":  {strings.Replace(uiOIDC, "provider: google", "provider: gitlab", 1), `provider "gitlab": no ui.oidc provider`},
		"provider github":   {strings.Replace(uiOIDC, "provider: google", "provider: github", 1), "use github_user or github_id"},
		"subject and email": {strings.Replace(uiOIDC, "      email: Alice@Example.com\n", "      email: a@b.c\n      subject: x\n", 1), "not both"},
		"neither":           {strings.Replace(uiOIDC, "      email: Alice@Example.com\n", "", 1), "set subject"},
		"bad subject":       {strings.Replace(uiOIDC, "5f1c0a7e-0000-4000-8000-000000000001", "\"a b\"", 1), "printable ASCII"},
		"bad email":         {strings.Replace(uiOIDC, "Alice@Example.com", "alice", 1), "not an email address"},
		"mixed entry":       {strings.Replace(uiOIDC, "      email: Alice@Example.com\n", "      email: a@b.c\n      github_id: 5\n", 1), "not both"},
		"email no provider": {strings.Replace(uiOIDC, "    - provider: google\n      email:", "    - email:", 1), "need provider"},
		"github entry":      {strings.Replace(uiOIDC, "    - provider: google\n      email: Alice@Example.com\n", "    - github_id: 5\n", 1), "needs ui.github"},
		"empty entry":       {strings.Replace(uiOIDC, "    - provider: google\n      email: Alice@Example.com\n", "    -\n", 1), "set github_user"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, writeFile(t, tc.yaml), "UI_OIDC_SECRET=x", "UI_GOOGLE_SECRET=y")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// An inline client_secret in an OIDC entry is refused without its value.
func TestUIOIDCInlineClientSecretRefused(t *testing.T) {
	y := "ui:\n  oidc:\n    - name: a\n      client_secret: hunter2-secret-value\n"
	_, err := load(t, writeFile(t, y))
	if err == nil || !strings.Contains(err.Error(), "line 4: ui.oidc[0].client_secret: an inline client secret is refused") ||
		strings.Contains(err.Error(), "hunter2") {
		t.Fatal(err)
	}
}

func TestUIOIDCClientSecretFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s")
	if err := os.WriteFile(p, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, _, err := UIOIDC{Name: "kc", ClientSecretFile: p}.ClientSecret()
	if err != nil || string(v.Reveal()) != "file-secret" {
		t.Fatal(err)
	}
	if _, _, err := (UIOIDC{Name: "kc"}).ClientSecret(); err == nil || !strings.Contains(err.Error(), "ui.oidc[kc]: no client secret") {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(p, 0o666); err != nil { //nolint:gosec // G302: the test makes the file writable by others
			t.Fatal(err)
		}
		if _, _, err := (UIOIDC{Name: "kc", ClientSecretFile: p}).ClientSecret(); err == nil || !strings.Contains(err.Error(), "ui.oidc[kc].client_secret_file") {
			t.Fatal(err)
		}
	}
}
