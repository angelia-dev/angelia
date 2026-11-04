package config

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "angelia.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func load(t *testing.T, path string, env ...string) (*Config, error) {
	t.Helper()
	return Load(context.Background(), Options{Path: path, Environ: env})
}

// AC: defaults work with no file at all.
func TestDefaultsWithoutFile(t *testing.T) {
	cfg, err := load(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8080" || cfg.Database.DSN != "sqlite:///var/lib/angelia/angelia.db" ||
		!reflect.DeepEqual(cfg.Roles, []string{"all"}) || cfg.Log.Level != "info" ||
		cfg.Log.Format != "json" || !strings.HasPrefix(cfg.HTTP.UserAgent, "angelia/") ||
		cfg.Instance.Name != "angelia" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestEmptyFileKeepsDefaults(t *testing.T) {
	cfg, err := load(t, writeFile(t, ""))
	if err != nil || cfg.Listen != ":8080" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
}

func TestFileOverridesDefaults(t *testing.T) {
	p := writeFile(t, `
listen: 127.0.0.1:9000
instance:
  name: prod.eu-1
database:
  dsn: postgres://u@h/db
roles: [api, worker]
secrets:
  key_file: /run/key
log:
  level: debug
  format: text
http:
  user_agent: test/1
sources:
  github-release:
    base_url: https://api.example.com
    token_env: GH_TOKEN
`)
	cfg, err := load(t, p, "GH_TOKEN=s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:9000" || cfg.Database.DSN != "postgres://u@h/db" ||
		!reflect.DeepEqual(cfg.Roles, []string{"api", "worker"}) || cfg.Secrets.KeyFile != "/run/key" ||
		cfg.Log.Level != "debug" || cfg.Log.Format != "text" || cfg.HTTP.UserAgent != "test/1" ||
		cfg.Instance.Name != "prod.eu-1" {
		t.Fatalf("unexpected: %+v", cfg)
	}
	if cfg.SourceToken("github-release") != "s3cret" {
		t.Fatal("token not read from token_env")
	}
}

// AC: unknown keys are an error with file path and line.
func TestUnknownKeyReportsPathAndLine(t *testing.T) {
	p := writeFile(t, "listen: :1\nbogus: 1\n")
	_, err := load(t, p)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), p) || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("want path and line 2, got %v", err)
	}
}

func TestUnknownNestedKeyAndTypeError(t *testing.T) {
	p := writeFile(t, "log:\n  levl: debug\nroles: 3\n")
	_, err := load(t, p)
	if err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), p) {
		t.Fatalf("got %v", err)
	}
}

func TestMalformedYAML(t *testing.T) {
	p := writeFile(t, "listen: [\n")
	if _, err := load(t, p); err == nil || !strings.Contains(err.Error(), p) {
		t.Fatalf("got %v", err)
	}
}

func TestMissingFileIsError(t *testing.T) {
	if _, err := load(t, filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected error")
	}
}

func TestCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Load(ctx, Options{}); err == nil {
		t.Fatal("expected error")
	}
}

// AC: env overrides tested for every key type (string, list, nested, map).
func TestEnvOverrides(t *testing.T) {
	p := writeFile(t, "listen: :1\nsources:\n  github-release:\n    base_url: https://old.example.com\n")
	cfg, err := load(t, p,
		"ANGELIA_LISTEN=:2",
		"ANGELIA_INSTANCE_NAME=env-name",
		"ANGELIA_DATABASE_DSN=sqlite:///tmp/x.db",
		"ANGELIA_ROLES=api, worker",
		"ANGELIA_SECRETS_KEY_FILE=/k",
		"ANGELIA_LOG_LEVEL=warn",
		"ANGELIA_LOG_FORMAT=text",
		"ANGELIA_HTTP_USER_AGENT=ua/2",
		"ANGELIA_SOURCES_GITHUB_RELEASE_BASE_URL=https://new.example.com",
		"ANGELIA_SOURCES_PYPI_BASE_URL=https://pypi.example.com",
		"ANGELIA_SOURCES_PYPI_TOKEN_ENV=PYPI_T",
		"PYPI_T=tok",
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":2" || cfg.Database.DSN != "sqlite:///tmp/x.db" ||
		!reflect.DeepEqual(cfg.Roles, []string{"api", "worker"}) || cfg.Secrets.KeyFile != "/k" ||
		cfg.Log.Level != "warn" || cfg.Log.Format != "text" || cfg.HTTP.UserAgent != "ua/2" ||
		cfg.Instance.Name != "env-name" {
		t.Fatalf("unexpected: %+v", cfg)
	}
	if cfg.Sources["github-release"].BaseURL != "https://new.example.com" {
		t.Fatalf("file kind not overridden: %+v", cfg.Sources)
	}
	if cfg.Sources["pypi"].BaseURL != "https://pypi.example.com" || cfg.SourceToken("pypi") != "tok" {
		t.Fatalf("env-only source: %+v", cfg.Sources)
	}
}

func TestEnvBadSourcesKeys(t *testing.T) {
	for _, e := range []string{"ANGELIA_SOURCES__BASE_URL=x", "ANGELIA_SOURCES_PYPI-X_BASE_URL=x"} {
		if _, err := load(t, "", e); err == nil {
			t.Errorf("%s: expected error", e)
		}
	}
}

func TestEnvCanBeInvalid(t *testing.T) {
	if _, err := load(t, "", "ANGELIA_LOG_LEVEL=loud"); err == nil {
		t.Fatal("expected validation error from env value")
	}
}

// AC: tokens are only read from env vars named by token_env, never inline.
func TestInlineTokenRejected(t *testing.T) {
	p := writeFile(t, "sources:\n  pypi:\n    token: abc\n")
	_, err := load(t, p)
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("got %v", err)
	}
}

func TestTokenEnvMustBeVariableName(t *testing.T) {
	p := writeFile(t, "sources:\n  pypi:\n    token_env: my!secret\n")
	_, err := load(t, p)
	if err == nil || !strings.Contains(err.Error(), "not a token") || strings.Contains(err.Error(), "my!secret") {
		t.Fatalf("got %v", err)
	}
}

func TestTokenOnlyFromNamedVariable(t *testing.T) {
	p := writeFile(t, "sources:\n  pypi:\n    token_env: MY_TOK\n")
	cfg, err := load(t, p, "MY_TOK=a", "PYPI_TOKEN=b", "OTHER=c")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SourceToken("pypi") != "a" || cfg.SourceToken("npm") != "" {
		t.Fatal("wrong token resolution")
	}
	cfg, err = load(t, p)
	if err != nil || cfg.SourceToken("pypi") != "" {
		t.Fatalf("unset variable should give empty token: %v", err)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"listen":    "listen: nonsense",
		"port":      "listen: ':99999'",
		"dsn":       "database:\n  dsn: /just/a/path",
		"scheme":    "database:\n  dsn: mysql://h/db",
		"roles":     "roles: [boss]",
		"empty":     "roles: []",
		"level":     "log:\n  level: loud",
		"format":    "log:\n  format: xml",
		"ua":        "http:\n  user_agent: ''",
		"base_url":  "sources:\n  pypi:\n    base_url: ftp://x",
		"base_url2": "sources:\n  pypi:\n    base_url: example.com",
		"port0":     "listen: ':0'",
		"allmixed":  "roles: [all, api]",
		"pgnohost":  "database:\n  dsn: postgres:///db",
		"sqlnopath": "database:\n  dsn: sqlite://",
		"instname":  "instance:\n  name: Prod",
		"instempty": "instance:\n  name: ''",
		"instdash":  "instance:\n  name: -a",
		"instlong":  "instance:\n  name: " + strings.Repeat("a", 64),
		"instunder": "instance:\n  name: my_instance",
		"instslash": "instance:\n  name: a/b",
		"badkind":   "sources:\n  Py_PI:\n    base_url: https://x.example.com",
	}
	for name, y := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := load(t, writeFile(t, y)); err == nil {
				t.Fatalf("expected error for %q", y)
			}
		})
	}
}

func TestValidationReportsAllProblems(t *testing.T) {
	_, err := load(t, writeFile(t, "listen: bad\nlog:\n  level: loud\n"))
	if err == nil || !strings.Contains(err.Error(), "listen") || !strings.Contains(err.Error(), "log.level") {
		t.Fatalf("got %v", err)
	}
}

func TestTokenEnvValueNeverEchoed(t *testing.T) {
	for _, v := range []string{"ghp_SECRETSECRET123", "pypi-AgEIcHlwaS5vcmc", "npm_abcdef", "my token!secret", "glpat-xyz", "github_pat_11AAA"} {
		_, err := load(t, writeFile(t, "sources:\n  pypi:\n    token_env: '"+v+"'\n"))
		if err == nil {
			t.Fatalf("%q: expected error", v)
		}
		if strings.Contains(err.Error(), v) || strings.Contains(err.Error(), v[:6]) {
			t.Fatalf("error echoes the value: %v", err)
		}
	}
}

func TestMultipleDocumentsRejected(t *testing.T) {
	_, err := load(t, writeFile(t, "listen: :1\n---\nlisten: :2\n"))
	if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("got %v", err)
	}
}

func TestReservedPrefixDoesNotBreakLoad(t *testing.T) {
	p := writeFile(t, "sources:\n  github-release:\n    token_env: ANGELIA_SOURCES_GITHUB_TOKEN\n")
	cfg, err := load(t, p, "ANGELIA_SOURCES_GITHUB_TOKEN=tok")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SourceToken("github-release") != "tok" {
		t.Fatal("token not resolved")
	}
}

func TestEnvKindIsNormalisedDeterministically(t *testing.T) {
	cfg, err := load(t, "", "ANGELIA_SOURCES_GITHUB_RELEASE_BASE_URL=https://a.example.com")
	if err != nil || cfg.Sources["github-release"].BaseURL != "https://a.example.com" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
}

func TestBaseURLCredentialsRejectedAndNotPrinted(t *testing.T) {
	_, err := load(t, writeFile(t, "sources:\n  pypi:\n    base_url: https://user:hunter2@example.com\n"))
	if err == nil || !strings.Contains(err.Error(), "credentials not allowed") || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("got %v", err)
	}
	_, err = load(t, writeFile(t, "sources:\n  pypi:\n    base_url: 'ftp://hunter2'\n"))
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("got %v", err)
	}
}

func TestUnsetTokenEnvWarns(t *testing.T) {
	p := writeFile(t, "sources:\n  pypi:\n    token_env: MY_TOK\n")
	cfg, err := load(t, p, "MY_TOK=")
	if err != nil {
		t.Fatal(err)
	}
	w := cfg.Warnings()
	if len(w) != 1 || !strings.Contains(w[0], "MY_TOK") || !strings.Contains(w[0], "pypi") {
		t.Fatalf("warnings=%v", w)
	}
	cfg, _ = load(t, p, "MY_TOK=x")
	if len(cfg.Warnings()) != 0 {
		t.Fatal("unexpected warning")
	}
}

func TestTokensDoNotLeakInFormatting(t *testing.T) {
	p := writeFile(t, "sources:\n  pypi:\n    token_env: MY_TOK\n")
	cfg, err := load(t, p, "MY_TOK=sup3rs3cret")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"%v", "%+v", "%#v", "%s"} {
		if out := fmt.Sprintf(f, cfg); strings.Contains(out, "sup3rs3cret") {
			t.Fatalf("%s leaks: %s", f, out)
		}
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("cfg", "tokens", cfg.Tokens, "tok", cfg.Tokens["pypi"])
	if strings.Contains(buf.String(), "sup3rs3cret") || !strings.Contains(buf.String(), "[REDACTED]") {
		t.Fatalf("slog leaks: %s", buf.String())
	}
	if cfg.SourceToken("pypi") != "sup3rs3cret" {
		t.Fatal("SourceToken must still return the value")
	}
}

func TestRolesDedupe(t *testing.T) {
	cfg, err := load(t, "", "ANGELIA_ROLES=api,worker,api")
	if err != nil || !reflect.DeepEqual(cfg.Roles, []string{"api", "worker"}) {
		t.Fatalf("cfg=%v err=%v", cfg, err)
	}
}

func TestUnknownKeyMessageHasNoGoTypeNames(t *testing.T) {
	_, err := load(t, writeFile(t, "bogus: 1\n"))
	if err == nil || !strings.Contains(err.Error(), `unknown key "bogus"`) || strings.Contains(err.Error(), "config.Config") {
		t.Fatalf("got %v", err)
	}
}

// AC: instance.name accepts the envelope pattern ^[a-z0-9][a-z0-9.-]{0,62}$.
func TestInstanceNameValid(t *testing.T) {
	for _, n := range []string{"a", "0", "my-instance", "a.b-c", "9x", strings.Repeat("a", 63)} {
		t.Run(n, func(t *testing.T) {
			cfg, err := load(t, "", "ANGELIA_INSTANCE_NAME="+n)
			if err != nil || cfg.Instance.Name != n {
				t.Fatalf("name=%q cfg=%+v err=%v", n, cfg, err)
			}
		})
	}
}

// AC: an invalid instance.name gives a clear error naming the key and the pattern.
func TestInstanceNameInvalidMessage(t *testing.T) {
	for _, n := range []string{"", "Prod", ".a", "-a", "a b", "a_b", "a/b", "a\n", strings.Repeat("a", 64)} {
		t.Run(n, func(t *testing.T) {
			_, err := load(t, "", "ANGELIA_INSTANCE_NAME="+n)
			if err == nil || !strings.Contains(err.Error(), "instance.name") ||
				!strings.Contains(err.Error(), `^[a-z0-9][a-z0-9.-]{0,62}$`) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

// api.allowed_env: exact names, empty by default, ANGELIA_* always refused.
func TestAPIAllowedEnv(t *testing.T) {
	cfg, err := load(t, "")
	if err != nil || len(cfg.API.AllowedEnv) != 0 || cfg.API.AllowsEnv("CI_WEBHOOK_SECRET") {
		t.Fatalf("default: %+v %v", cfg.API, err)
	}
	cfg, err = load(t, writeFile(t, "api:\n  allowed_env: [CI_WEBHOOK_SECRET, SLACK_URL]\n"))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"CI_WEBHOOK_SECRET": true, "SLACK_URL": true, "CI_WEBHOOK": false, "ci_webhook_secret": false} {
		if got := cfg.API.AllowsEnv(name); got != want {
			t.Errorf("%s: %v", name, got)
		}
	}
	if (API{AllowedEnv: []string{"ANGELIA_DATABASE_DSN"}}).AllowsEnv("ANGELIA_DATABASE_DSN") {
		t.Error("an ANGELIA_ name is allowed")
	}
	for _, y := range []string{"api:\n  allowed_env: [ANGELIA_TOKEN]\n", "api:\n  allowed_env: ['a b']\n"} {
		if _, err := load(t, writeFile(t, y)); err == nil || !strings.Contains(err.Error(), "api.allowed_env[0]") {
			t.Errorf("%q: %v", y, err)
		}
	}
}
