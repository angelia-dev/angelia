package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeClientConfig(t *testing.T, dir, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, "angelia", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // WriteFile is subject to the umask
		t.Fatal(err)
	}
	return p
}

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestClientConfigPath(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"home", map[string]string{"HOME": "/home/u"}, "/home/u/.config/angelia/config.yaml"},
		{"xdg", map[string]string{"HOME": "/home/u", "XDG_CONFIG_HOME": "/x/cfg"}, "/x/cfg/angelia/config.yaml"},
		{"relative xdg is ignored", map[string]string{"HOME": "/home/u", "XDG_CONFIG_HOME": "cfg"}, "/home/u/.config/angelia/config.yaml"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := clientConfigPath(envMap(c.env))
			if err != nil {
				t.Fatal(err)
			}
			if filepath.ToSlash(got) != c.want {
				t.Errorf("path = %s, want %s", got, c.want)
			}
		})
	}
}

func TestResolveConnectionPrecedence(t *testing.T) {
	dir := t.TempDir()
	writeClientConfig(t, dir, "server: http://file.example.com\ntoken: file-token\n", 0o600)
	base := map[string]string{"XDG_CONFIG_HOME": dir}
	with := func(kv ...string) func(string) string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return envMap(m)
	}
	cases := []struct {
		name                string
		flagServer, flagTok string
		getenv              func(string) string
		wantServer, wantTok string
	}{
		{"file only", "", "", with(), "http://file.example.com", "file-token"},
		{"env beats file", "", "", with(envServer, "http://env.example.com", envToken, "env-token"), "http://env.example.com", "env-token"},
		{"flag beats env and file", "http://flag.example.com", "flag-token", with(envServer, "http://env.example.com", envToken, "env-token"), "http://flag.example.com", "flag-token"},
		{"each value on its own", "http://flag.example.com", "", with(envToken, "env-token"), "http://flag.example.com", "env-token"},
		{"empty env counts as unset", "", "", with(envServer, "", envToken, ""), "http://file.example.com", "file-token"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveConnection(c.flagServer, c.flagTok, c.getenv)
			if err != nil {
				t.Fatal(err)
			}
			if got.Server != c.wantServer || got.Token != c.wantTok {
				t.Errorf("got %v, want %s with token %s", got, c.wantServer, c.wantTok)
			}
		})
	}
}

func TestResolveConnectionDefaults(t *testing.T) {
	got, err := resolveConnection("", "", envMap(map[string]string{"XDG_CONFIG_HOME": t.TempDir()}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Server != DefaultServer || got.Token != "" {
		t.Errorf("got %v", got)
	}
}

func TestClientFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX modes")
	}
	const secret = "s3cret-file-token"
	t.Run("token with group or world access is refused", func(t *testing.T) {
		for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o660, 0o666, 0o604 | 0o100} {
			dir := t.TempDir()
			path := writeClientConfig(t, dir, "token: "+secret+"\n", mode)
			_, err := resolveConnection("", "", envMap(map[string]string{"XDG_CONFIG_HOME": dir}))
			if err == nil {
				t.Fatalf("mode %04o: accepted", mode)
			}
			for _, want := range []string{path, "chmod 600"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("mode %04o: error %q lacks %q", mode, err, want)
				}
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("mode %04o: the error shows the token", mode)
			}
		}
	})
	t.Run("a flag does not bypass the check", func(t *testing.T) {
		dir := t.TempDir()
		writeClientConfig(t, dir, "token: "+secret+"\n", 0o644)
		if _, err := resolveConnection("http://x.example.com", "flag-token", envMap(map[string]string{"XDG_CONFIG_HOME": dir})); err == nil {
			t.Fatal("accepted")
		}
	})
	t.Run("owner-only modes are accepted", func(t *testing.T) {
		for _, mode := range []os.FileMode{0o600, 0o400, 0o700} {
			dir := t.TempDir()
			writeClientConfig(t, dir, "token: "+secret+"\n", mode)
			got, err := resolveConnection("", "", envMap(map[string]string{"XDG_CONFIG_HOME": dir}))
			if err != nil || got.Token != secret {
				t.Fatalf("mode %04o: %v, %v", mode, got, err)
			}
		}
	})
	t.Run("a file without a token may be world-readable", func(t *testing.T) {
		dir := t.TempDir()
		writeClientConfig(t, dir, "server: http://srv.example.com\n", 0o644)
		got, err := resolveConnection("", "", envMap(map[string]string{"XDG_CONFIG_HOME": dir}))
		if err != nil || got.Server != "http://srv.example.com" {
			t.Fatalf("%v, %v", got, err)
		}
	})
}

func TestClientFileErrors(t *testing.T) {
	cases := map[string]string{ //nolint:gosec // synthetic
		"unknown key":  "servr: http://x.example.com\n",
		"not yaml":     "server: [unclosed\n",
		"bad scheme":   "server: ftp://x.example.com\n",
		"no host":      "server: http://\n",
		"credentials":  "server: http://user:pw@x.example.com\n",
		"not a string": "server: {a: b}\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeClientConfig(t, dir, content, 0o600)
			_, err := resolveConnection("", "", envMap(map[string]string{"XDG_CONFIG_HOME": dir}))
			if err == nil {
				t.Fatal("accepted")
			}
			if strings.Contains(err.Error(), "pw@") {
				t.Errorf("error echoes credentials: %v", err)
			}
		})
	}
}

func TestConnectionNeverFormatsToken(t *testing.T) {
	c := Connection{Server: "http://x.example.com", Token: "tok-secret"}
	for _, s := range []string{c.String(), fmt.Sprintf("%v %+v %#v %s", c, c, c, c)} {
		if strings.Contains(s, "tok-secret") {
			t.Errorf("token in %q", s)
		}
	}
}

func TestFlagsBeatEnvAndFileInCommands(t *testing.T) {
	home := isolate(t)
	writeClientConfig(t, filepath.Join(home, ".config"), "server: http://127.0.0.1:1\ntoken: file-token\n", 0o600)
	envSrv := newFixtureServer(t, map[string]string{"GET /v1/sinks": "sinks"})
	flagSrv := newFixtureServer(t, map[string]string{"GET /v1/sinks": "sinks"})
	t.Setenv(envServer, envSrv.URL)
	t.Setenv(envToken, "env-token")

	if _, _, err := runCLI(t, "sink", "list"); err != nil {
		t.Fatal(err)
	}
	if got := envSrv.last().Auth; got != "Bearer env-token" {
		t.Errorf("env: Authorization = %q", got)
	}
	if _, _, err := runCLI(t, "sink", "list", "--server", flagSrv.URL, "--token", "flag-token"); err != nil {
		t.Fatal(err)
	}
	if got := flagSrv.last().Auth; got != "Bearer flag-token" {
		t.Errorf("flag: Authorization = %q", got)
	}
	// With neither, the file applies.
	t.Setenv(envServer, "")
	t.Setenv(envToken, "")
	if _, _, err := runCLI(t, "sink", "list"); err == nil {
		t.Error("the file points at a closed port: want a connection error")
	}
}
