package config

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// AC: the installed-plugin config has an operator allowed_hosts extension
// and per-host credentials read from the environment, never inline.
func TestPluginsConfig(t *testing.T) {
	p := writeFile(t, `
plugins:
  crates:
    allowed_hosts: [crates-mirror.example.com, 192.0.2.7]
    credentials:
      crates-mirror.example.com:
        token_env: CRATES_MIRROR_TOKEN
        scheme: Bearer
      crates.io:
        token_env: CRATES_IO_TOKEN
`)
	cfg, err := load(t, p, "CRATES_MIRROR_TOKEN=tok-123")
	if err != nil {
		t.Fatal(err)
	}
	want := Plugin{
		AllowedHosts: []string{"crates-mirror.example.com", "192.0.2.7"},
		Credentials: map[string]PluginCredential{
			"crates-mirror.example.com": {TokenEnv: "CRATES_MIRROR_TOKEN", Scheme: "Bearer"},
			"crates.io":                 {TokenEnv: "CRATES_IO_TOKEN"}, //nolint:gosec // G101: a variable name, not a token.
		},
	}
	if got := cfg.Plugins["crates"]; !reflect.DeepEqual(got, want) {
		t.Errorf("plugins.crates = %+v, want %+v", got, want)
	}
	if got := cfg.PluginToken("crates", "crates-mirror.example.com"); got != "tok-123" {
		t.Errorf("PluginToken = %q", got)
	}
	if got := cfg.PluginToken("crates", "crates.io"); got != "" {
		t.Errorf("PluginToken of an unset variable = %q, want empty", got)
	}
	if w := strings.Join(cfg.Warnings(), "\n"); !strings.Contains(w, "plugins.crates.credentials.crates.io.token_env: environment variable CRATES_IO_TOKEN is not set") {
		t.Errorf("warnings = %q", w)
	}
	for _, s := range []string{fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg)} {
		if strings.Contains(s, "tok-123") {
			t.Errorf("config prints the token: %s", s)
		}
	}
}

func TestPluginsConfigRejected(t *testing.T) {
	cases := map[string]string{ //nolint:gosec // G101: synthetic YAML, no real token.
		"inline token":         "plugins:\n  crates:\n    credentials:\n      crates.io:\n        token: abc\n",
		"token at plugin":      "plugins:\n  crates:\n    token: abc\n",
		"token_env missing":    "plugins:\n  crates:\n    credentials:\n      crates.io: {}\n",
		"token_env not a name": "plugins:\n  crates:\n    credentials:\n      crates.io:\n        token_env: a-b\n",
		"wildcard host":        "plugins:\n  crates:\n    allowed_hosts: ['*.example.com']\n",
		"host with scheme":     "plugins:\n  crates:\n    allowed_hosts: ['https://example.com']\n",
		"host with port":       "plugins:\n  crates:\n    allowed_hosts: ['example.com:443']\n",
		"upper-case host":      "plugins:\n  crates:\n    allowed_hosts: [Example.com]\n",
		"credential key":       "plugins:\n  crates:\n    credentials:\n      'example.com/x':\n        token_env: T\n",
		"bad scheme":           "plugins:\n  crates:\n    credentials:\n      crates.io:\n        token_env: T\n        scheme: 'Bearer x'\n",
		"bad kind":             "plugins:\n  Crates_X: {}\n",
	}
	for name, y := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := load(t, writeFile(t, y)); err == nil {
				t.Fatalf("accepted:\n%s", y)
			}
		})
	}
}

func TestPluginTokenEnvValueNeverEchoed(t *testing.T) {
	v := "ghp_abcdefghijklmnop"
	_, err := load(t, writeFile(t, "plugins:\n  x:\n    credentials:\n      api.github.com:\n        token_env: "+v+"\n"))
	if err == nil || strings.Contains(err.Error(), v) {
		t.Fatalf("err = %v, want an error that does not echo the value", err)
	}
}

// Review S1: a credential key may name a port; the host part follows the
// allowed_hosts rule (canonical form).
func TestPluginCredentialKeys(t *testing.T) {
	cred := func(key string) string {
		return "plugins:\n  x:\n    credentials:\n      '" + key + "':\n        token_env: T\n"
	}
	for _, k := range []string{"mirror.example.com", "mirror.example.com:8443", "192.0.2.1:8443", "2001:db8::1", "[2001:db8::1]:8443"} {
		if _, err := load(t, writeFile(t, cred(k))); err != nil {
			t.Errorf("%q rejected: %v", k, err)
		}
	}
	for _, k := range []string{
		"mirror.example.com:0", "mirror.example.com:0443", "mirror.example.com:65536", "mirror.example.com:https",
		"[2001:db8::1]", "Mirror.example.com:8443", "mirror.example.com.", "https://mirror.example.com",
	} {
		if _, err := load(t, writeFile(t, cred(k))); err == nil {
			t.Errorf("%q accepted", k)
		}
	}
}
