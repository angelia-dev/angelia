package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// pluginDir writes <root>/<kind>/manifest.json (unless manifest is empty)
// and grants.yaml (unless grants is empty), and returns root.
func pluginDir(t *testing.T, root, kind, manifest, grants string) string {
	t.Helper()
	if root == "" {
		root = t.TempDir()
	}
	d := filepath.Join(root, kind)
	if err := os.MkdirAll(d, 0o755); err != nil { //nolint:gosec // G301: test fixture.
		t.Fatal(err)
	}
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(d, ManifestFile), []byte(manifest), 0o644); err != nil { //nolint:gosec // G306: test fixture.
			t.Fatal(err)
		}
	}
	if grants != "" {
		if err := os.WriteFile(filepath.Join(d, GrantsFile), []byte(grants), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func manifestOf(kind string, hosts ...string) string {
	q := make([]string, len(hosts))
	for i, h := range hosts {
		q[i] = fmt.Sprintf("%q", h)
	}
	return fmt.Sprintf(`{"kind":%q,"allowed_hosts":[%s],"config_schema":{"type":"object"}}`, kind, strings.Join(q, ","))
}

func loadPlugins(t *testing.T, dir string, env ...string) ([]InstalledPlugin, []string, error) {
	t.Helper()
	return LoadPlugins(context.Background(), dir, env)
}

const cratesGrants = `
allowed_hosts: [crates-mirror.example.com, 192.0.2.7]
credentials:
  crates-mirror.example.com:
    token_env: CRATES_MIRROR_TOKEN
    scheme: Bearer
  crates.io:
    token_env: CRATES_IO_TOKEN
  crates-mirror.example.com:8443:
    token_env: CRATES_MIRROR_TOKEN
    header: PRIVATE-TOKEN
`

// AC (Q1, ADR 0025): a plugin's grants live in <plugins_dir>/<kind>/grants.yaml,
// next to its manifest, with credentials read from the environment.
func TestLoadPluginsGrants(t *testing.T) {
	dir := pluginDir(t, "", "crates", manifestOf("crates", "crates.io"), cratesGrants)
	ps, warns, err := loadPlugins(t, dir, "CRATES_MIRROR_TOKEN=tok-123456789")
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 {
		t.Fatalf("plugins = %+v", ps)
	}
	p := ps[0]
	want := PluginGrants{
		AllowedHosts: []string{"crates-mirror.example.com", "192.0.2.7"},
		Credentials: map[string]PluginCredential{
			"crates-mirror.example.com":      {TokenEnv: "CRATES_MIRROR_TOKEN", Scheme: "Bearer"},
			"crates.io":                      {TokenEnv: "CRATES_IO_TOKEN"}, //nolint:gosec // G101: a variable name, not a token.
			"crates-mirror.example.com:8443": {TokenEnv: "CRATES_MIRROR_TOKEN", Header: "PRIVATE-TOKEN"},
		},
	}
	if p.Kind != "crates" || p.Dir != filepath.Join(dir, "crates") || !reflect.DeepEqual(p.ManifestHosts, []string{"crates.io"}) {
		t.Errorf("plugin = %+v", p)
	}
	if !reflect.DeepEqual(p.Grants, want) {
		t.Errorf("grants = %+v, want %+v", p.Grants, want)
	}
	if got := p.Token("crates-mirror.example.com"); got != "tok-123456789" {
		t.Errorf("Token = %q", got)
	}
	if got := p.Token("crates.io"); got != "" {
		t.Errorf("Token of an unset variable = %q, want empty", got)
	}
	if w := strings.Join(warns, "\n"); !strings.Contains(w, "crates.io: token_env: environment variable CRATES_IO_TOKEN is not set or empty") {
		t.Errorf("warnings = %q", w)
	}
	for _, s := range []string{fmt.Sprintf("%v", ps), fmt.Sprintf("%+v", ps), fmt.Sprintf("%#v", ps)} {
		if strings.Contains(s, "tok-123456789") {
			t.Errorf("plugins print the token: %s", s)
		}
	}
}

func TestLoadPluginsNoGrantsAndMissingDir(t *testing.T) {
	ps, _, err := loadPlugins(t, filepath.Join(t.TempDir(), "absent"))
	if err != nil || len(ps) != 0 {
		t.Fatalf("missing dir: %+v, %v", ps, err)
	}
	// grants.yaml is optional: the manifest's hosts only.
	dir := pluginDir(t, "", "crates", manifestOf("crates", "crates.io"), "")
	// Hidden entries and plain files are not plugins.
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	ps, warns, err := loadPlugins(t, dir)
	if err != nil || len(ps) != 1 || ps[0].Grants.AllowedHosts != nil || ps[0].Grants.Credentials != nil {
		t.Fatalf("plugins = %+v, %v", ps, err)
	}
	if w := strings.Join(warns, "\n"); !strings.Contains(w, "README: not a directory, ignored") || strings.Contains(w, ".cache") {
		t.Errorf("warnings = %q", w)
	}
}

func TestLoadPluginsSortedByKind(t *testing.T) {
	dir := pluginDir(t, "", "zeta", manifestOf("zeta"), "")
	pluginDir(t, dir, "alpha", manifestOf("alpha"), "")
	ps, _, err := loadPlugins(t, dir)
	if err != nil || len(ps) != 2 || ps[0].Kind != "alpha" || ps[1].Kind != "zeta" {
		t.Fatalf("plugins = %+v, %v", ps, err)
	}
}

func TestLoadPluginsRejected(t *testing.T) {
	m := manifestOf("crates", "crates.io")
	cases := map[string]struct{ kind, manifest, grants, want string }{ //nolint:gosec // G101: synthetic YAML, no real token.
		"no manifest":          {"crates", "", "allowed_hosts: [a.example]\n", "manifest.json"},
		"manifest kind":        {"crates", manifestOf("other"), "", `declares kind "other"`},
		"manifest not json":    {"crates", "{", "", "manifest.json"},
		"bad folder name":      {"Crates_X", manifestOf("Crates_X"), "", "not a valid plugin kind"},
		"unknown key":          {"crates", m, "allowed_host: [a.example]\n", `unknown key "allowed_host"`},
		"inline token":         {"crates", m, "credentials:\n  crates.io:\n    token: abc\n", `unknown key "token"`},
		"token_env missing":    {"crates", m, "credentials:\n  crates.io: {}\n", "token_env: required"},
		"token_env not a name": {"crates", m, "credentials:\n  crates.io:\n    token_env: a-b\n", "environment variable name"},
		"wildcard host":        {"crates", m, "allowed_hosts: ['*.example.com']\n", "allowed_hosts[0]"},
		"host with scheme":     {"crates", m, "allowed_hosts: ['https://example.com']\n", "allowed_hosts[0]"},
		"host with port":       {"crates", m, "allowed_hosts: ['example.com:443']\n", "allowed_hosts[0]"},
		"upper-case host":      {"crates", m, "allowed_hosts: [Example.com]\n", "allowed_hosts[0]"},
		"credential key":       {"crates", m, "credentials:\n  'example.com/x':\n    token_env: T\n", "key must be host or host:port"},
		"credential not allowed": {
			"crates", m, "credentials:\n  elsewhere.example:\n    token_env: T\n",
			"not in the manifest's or this file's allowed_hosts",
		},
		"bad scheme":            {"crates", m, "credentials:\n  crates.io:\n    token_env: T\n    scheme: 'Bearer x'\n", "scheme"},
		"header not credential": {"crates", m, "credentials:\n  crates.io:\n    token_env: T\n    header: X-Api-Key\n", "header"},
		"header not a token":    {"crates", m, "credentials:\n  crates.io:\n    token_env: T\n    header: 'Private Token'\n", "header"},
		"scheme without Authorization": {
			"crates", m, "credentials:\n  crates.io:\n    token_env: T\n    header: PRIVATE-TOKEN\n    scheme: Bearer\n",
			"scheme applies to the Authorization header only",
		},
		"two documents": {"crates", m, "allowed_hosts: []\n---\nallowed_hosts: []\n", "multiple YAML documents"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := pluginDir(t, "", c.kind, c.manifest, c.grants)
			_, _, err := loadPlugins(t, dir)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestPluginGrantsTokenEnvValueNeverEchoed(t *testing.T) {
	v := "ghp_abcdefghijklmnop"
	dir := pluginDir(t, "", "x", manifestOf("x", "api.github.com"), "credentials:\n  api.github.com:\n    token_env: "+v+"\n")
	_, _, err := loadPlugins(t, dir)
	if err == nil || strings.Contains(err.Error(), v) {
		t.Fatalf("err = %v, want an error that does not echo the value", err)
	}
}

// Review S1 of #145: a credential key may name a port; the host part
// follows the allowed_hosts rule (canonical form).
func TestPluginCredentialKeys(t *testing.T) {
	m := manifestOf("x", "mirror.example.com", "192.0.2.1", "2001:db8::1")
	cred := func(key string) string { return "credentials:\n  '" + key + "':\n    token_env: T\n" }
	for _, k := range []string{"mirror.example.com", "mirror.example.com:8443", "192.0.2.1:8443", "2001:db8::1", "[2001:db8::1]:8443"} {
		if _, _, err := loadPlugins(t, pluginDir(t, "", "x", m, cred(k))); err != nil {
			t.Errorf("%q rejected: %v", k, err)
		}
	}
	for _, k := range []string{
		"mirror.example.com:0", "mirror.example.com:0443", "mirror.example.com:65536", "mirror.example.com:https",
		"[2001:db8::1]", "Mirror.example.com:8443", "mirror.example.com.", "https://mirror.example.com",
	} {
		if _, _, err := loadPlugins(t, pluginDir(t, "", "x", m, cred(k))); err == nil {
			t.Errorf("%q accepted", k)
		}
	}
}

// AC: grants.yaml grants network access, so a file that others can write
// is refused (tampering, not reading, is the risk: it holds no secret).
func TestPluginGrantsFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	for _, mode := range []os.FileMode{0o620, 0o602, 0o666} {
		dir := pluginDir(t, "", "crates", manifestOf("crates"), "allowed_hosts: []\n")
		p := filepath.Join(dir, "crates", GrantsFile)
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		_, _, err := loadPlugins(t, dir)
		if err == nil || !strings.Contains(err.Error(), "writable by group or others") || !strings.Contains(err.Error(), "chmod go-w "+p) {
			t.Errorf("mode %04o: err = %v, want refused", mode, err)
		}
	}
	// Readable by others is fine: the file holds variable names only.
	dir := pluginDir(t, "", "crates", manifestOf("crates"), "allowed_hosts: []\n")
	if err := os.Chmod(filepath.Join(dir, "crates", GrantsFile), 0o644); err != nil { //nolint:gosec // G302: the point of the test.
		t.Fatal(err)
	}
	if _, _, err := loadPlugins(t, dir); err != nil {
		t.Errorf("0644 refused: %v", err)
	}
	// A directory named grants.yaml is not a grants file.
	dir = pluginDir(t, "", "crates", manifestOf("crates"), "")
	if err := os.Mkdir(filepath.Join(dir, "crates", GrantsFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadPlugins(t, dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("err = %v, want not a regular file", err)
	}
}

// AC (Q1): the old plugins.* instance keys fail with a pointer to the new
// location.
func TestInstanceConfigPluginsKeyMoved(t *testing.T) {
	y := "plugins_dir: /srv/angelia/plugins\nplugins:\n  crates:\n    allowed_hosts: [crates-mirror.example.com]\n  npm-x: {}\n"
	_, err := load(t, writeFile(t, y))
	if err == nil {
		t.Fatal("plugins.* accepted")
	}
	for _, want := range []string{
		"line 2", "plugins: moved", "/srv/angelia/plugins/crates/grants.yaml", "/srv/angelia/plugins/npm-x/grants.yaml",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
	// Even an empty plugins key is refused, with the default location.
	_, err = load(t, writeFile(t, "plugins: {}\n"))
	if err == nil || !strings.Contains(err.Error(), "/var/lib/angelia/plugins/<kind>/grants.yaml") {
		t.Errorf("err = %v", err)
	}
}

// AC: plugins_dir defaults to the data directory (the sqlite database's
// directory, else /var/lib/angelia) and can be set in the file or the
// environment; it must be absolute.
func TestPluginsDir(t *testing.T) {
	for _, c := range []struct{ yaml, env, want string }{
		{"", "", "/var/lib/angelia/plugins"},
		{"database:\n  dsn: sqlite:///data/angelia/a.db\n", "", "/data/angelia/plugins"},
		{"database:\n  dsn: postgres://db.example/angelia\n", "", "/var/lib/angelia/plugins"},
		{"plugins_dir: /opt/angelia/plugins/\n", "", "/opt/angelia/plugins"},
		{"plugins_dir: /opt/a\n", "ANGELIA_PLUGINS_DIR=/srv/p", "/srv/p"},
		{"", "ANGELIA_DATABASE_DSN=sqlite:///x/y.db", "/x/plugins"},
	} {
		var env []string
		if c.env != "" {
			env = append(env, c.env)
		}
		cfg, err := load(t, writeFile(t, c.yaml), env...)
		if err != nil {
			t.Fatalf("%q: %v", c.yaml, err)
		}
		if cfg.PluginsDir != c.want {
			t.Errorf("%q %q: PluginsDir = %q, want %q", c.yaml, c.env, cfg.PluginsDir, c.want)
		}
	}
	if _, err := load(t, writeFile(t, "plugins_dir: plugins\n")); err == nil || !strings.Contains(err.Error(), "plugins_dir") {
		t.Errorf("relative plugins_dir: err = %v", err)
	}
	if got := Default().PluginsDir; got != "/var/lib/angelia/plugins" {
		t.Errorf("Default().PluginsDir = %q", got)
	}
}
