//go:build unix

package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestExecCommands(t *testing.T) {
	cfg, err := load(t, writeFile(t, `
exec:
  commands:
    deploy-hook:
      argv: [/usr/local/bin/deploy-hook, --env, staging]
      dir: /var/lib/angelia
      env: [DEPLOY_TOKEN, UNSET_ONE]
      timeout: 10s
      max_concurrent: 2
    plain:
      argv: [/bin/true]
`), "DEPLOY_TOKEN=x")
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Exec.Commands["deploy-hook"]
	if !slices.Equal(c.Argv, []string{"/usr/local/bin/deploy-hook", "--env", "staging"}) || c.Dir != "/var/lib/angelia" ||
		!slices.Equal(c.Env, []string{"DEPLOY_TOKEN", "UNSET_ONE"}) || c.Timeout != 10*time.Second || c.MaxConcurrent != 2 {
		t.Fatalf("%+v", c)
	}
	if !slices.Equal(cfg.Exec.CommandNames(), []string{"deploy-hook", "plain"}) {
		t.Fatal(cfg.Exec.CommandNames())
	}
	warns := strings.Join(cfg.Warnings(), "\n")
	if !strings.Contains(warns, "exec.commands.deploy-hook.env: environment variable UNSET_ONE is not set") || strings.Contains(warns, "DEPLOY_TOKEN") {
		t.Fatalf("warnings %q", warns)
	}
	if !ExecSupported() {
		t.Fatal("unix must support exec")
	}
}

func TestExecCommandsInvalid(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want string }{
		{"name", "Bad_Name:\n      argv: [/bin/true]", `invalid command name "Bad_Name"`},
		{"no argv", "a:\n      dir: /", "exec.commands.a.argv: required"},
		{"empty argv", "a:\n      argv: []", "exec.commands.a.argv: required"},
		{"too many", "a:\n      argv: [/bin/true" + strings.Repeat(", x", 64) + "]", "at most 64 entries"},
		{"relative", "a:\n      argv: [true]", "exec.commands.a.argv[0]: must be an absolute, clean path"},
		{"unclean", "a:\n      argv: [/usr/bin/../bin/true]", "exec.commands.a.argv[0]: must be an absolute, clean path"},
		{"dir", "a:\n      argv: [/bin/true]\n      dir: var/lib", "exec.commands.a.dir: must be an absolute path"},
		{"env name", "a:\n      argv: [/bin/true]\n      env: [1BAD]", `exec.commands.a.env[0]: "1BAD" is not a valid`},
		{"env reserved", "a:\n      argv: [/bin/true]\n      env: [ANGELIA_SECRETS_KEY]", "exec.commands.a.env[0]: names starting with ANGELIA_ are always refused"},
		{"env twice", "a:\n      argv: [/bin/true]\n      env: [X, X]", "exec.commands.a.env[1]: X is listed twice"},
		{"timeout low", "a:\n      argv: [/bin/true]\n      timeout: 500ms", "exec.commands.a.timeout: must be from 1s to 12s"},
		{"timeout high", "a:\n      argv: [/bin/true]\n      timeout: 13s", "exec.commands.a.timeout: must be from 1s to 12s"},
		{"concurrency", "a:\n      argv: [/bin/true]\n      max_concurrent: 9", "exec.commands.a.max_concurrent: must be from 1 to 8"},
		{"negative", "a:\n      argv: [/bin/true]\n      max_concurrent: -1", "exec.commands.a.max_concurrent: must be from 1 to 8"},
		{"unknown key", "a:\n      argv: [/bin/true]\n      shell: true", "shell"},
		{"unknown section key", "a:\n      argv: [/bin/true]\n  allow: [x]", "allow"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, writeFile(t, "exec:\n  commands:\n    "+tc.yaml+"\n"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
		})
	}
}
