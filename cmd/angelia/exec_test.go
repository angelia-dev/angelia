//go:build unix

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/netguard"
	execsink "github.com/angelia-dev/angelia/internal/sink/exec"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

// The exec tests run this test binary as the command: argv[1] is
// execHelperArg. Mode "record <dir> <code>" writes stdin to a new file in
// dir and exits with code.
const execHelperArg = "angelia-exec-helper"

func TestMain(m *testing.M) {
	if len(os.Args) == 5 && os.Args[1] == execHelperArg && os.Args[2] == "record" {
		b, _ := io.ReadAll(os.Stdin)
		_ = os.WriteFile(filepath.Join(os.Args[3], strconv.Itoa(os.Getpid())), b, 0o600) //nolint:gosec // G703: a directory under the test's own t.TempDir()
		code, _ := strconv.Atoi(os.Args[4])
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func testBinary(t *testing.T) string {
	t.Helper()
	p, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// internal/config and internal/sink/exec bound a command alike.
func TestExecBoundsAgree(t *testing.T) {
	if config.ExecDefaultTimeout != execsink.DefaultTimeout || config.ExecMinTimeout != execsink.MinTimeout ||
		config.ExecMaxTimeout != execsink.MaxTimeout || config.ExecDefaultMaxConcurrent != execsink.DefaultMaxConcurrent ||
		config.ExecMaxMaxConcurrent != execsink.MaxMaxConcurrent || config.ExecMaxArgs != execsink.MaxArgs {
		t.Fatal("exec bounds differ between internal/config and internal/sink/exec")
	}
}

// ADR 0042 §1: the exec kind is registered, in the dispatcher and the
// plugin catalogue, only when exec.commands defines commands.
func TestBuiltinSinksExecOnlyWithCommands(t *testing.T) {
	cfg := &config.Config{Exec: config.Exec{Commands: map[string]config.ExecCommand{"hook": {Argv: []string{"/bin/true"}}}}}
	nc, err := netguard.New(netguard.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sinks, err := builtinSinks(cfg, nc, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, s := range sinks {
		kinds = append(kinds, s.Kind())
	}
	if want := append(slices.Clone(watchfile.SinkKinds), "exec"); !slices.Equal(kinds, want) {
		t.Fatalf("kinds %v, want %v", kinds, want)
	}
	var exec, webhook bool
	for _, p := range builtinPlugins(nil, execSinkKinds(cfg)...) {
		switch p.Kind {
		case "exec":
			exec = len(p.ConfigSchema) > 0 && bytes.Contains(p.ConfigSchema, []byte(`"command"`))
		case "webhook":
			webhook = true
		}
	}
	if !exec || !webhook {
		t.Fatalf("catalogue: exec with schema %v, webhook %v", exec, webhook)
	}
	for _, p := range builtinPlugins(nil, execSinkKinds(&config.Config{})...) {
		if p.Kind == "exec" {
			t.Fatal("exec listed without commands")
		}
	}
}

// config check fails on a missing or non-executable argv[0], naming the
// command, and warns about an unset env variable.
func TestConfigCheckExecCommands(t *testing.T) {
	t.Setenv("ANGELIA_CONFIG", "")
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for argv0, want := range map[string]string{
		"/nonexistent/angelia-hook": "exec.commands.hook.argv[0]: /nonexistent/angelia-hook: no such file or directory",
		plain:                       "exec.commands.hook.argv[0]: " + plain + ": not executable",
		filepath.Dir(plain):         "not a regular file",
	} {
		_, errb, err := runCheck(t, "--config", writeCfg(t, fmt.Sprintf("exec:\n  commands:\n    hook:\n      argv: [%s]\n", argv0)))
		if err == nil || !strings.Contains(errb, want) {
			t.Errorf("%s: err %v, stderr %q", argv0, err, errb)
		}
	}
	_, errb, err := runCheck(t, "--config", writeCfg(t, fmt.Sprintf("exec:\n  commands:\n    hook:\n      argv: [%s]\n      env: [ANGELIA_SECRETS_KEY]\n", testBinary(t))))
	if err == nil || !strings.Contains(errb, "names starting with ANGELIA_ are always refused") {
		t.Fatalf("reserved name: %v %q", err, errb)
	}
	out, errb, err := runCheck(t, "--config", writeCfg(t, fmt.Sprintf("exec:\n  commands:\n    hook:\n      argv: [%s]\n      env: [EXEC_TEST_UNSET_VAR]\n", testBinary(t))))
	if err != nil || !strings.Contains(out, "configuration OK") ||
		!strings.Contains(errb, "warning: exec.commands.hook.env: environment variable EXEC_TEST_UNSET_VAR is not set") {
		t.Fatalf("out %q stderr %q err %v", out, errb, err)
	}
}

// serve refuses to start the dispatcher role when a command is missing.
func TestServeDispatcherNeedsExecCommands(t *testing.T) {
	cfgPath, _ := instanceFiles(t, fmt.Sprintf("listen: %q\nexec:\n  commands:\n    hook:\n      argv: [/nonexistent/angelia-hook]\n", freeAddr(t)))
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"serve", "--config", cfgPath, "--roles=dispatcher"})
	err := root.ExecuteContext(t.Context())
	if err == nil || !strings.Contains(err.Error(), "exec.commands.hook.argv[0]") {
		t.Fatalf("serve: %v\n%s", err, out.String())
	}
}

// End to end: one event, an exec sink whose command succeeds and one whose
// command exits 1. The first gets the CloudEvents JSON on stdin and its
// delivery is delivered; the second stays pending with only the outcome in
// last_error.
func TestServeDeliversToExecSink(t *testing.T) {
	reg := &fakePyPI{}
	reg.set("1.0.0", "0.9.0")
	registry := listenOn(t, "127.0.0.1:0", reg)
	t.Setenv("ANGELIA_WATCHES", "")
	runs := t.TempDir()
	bin := testBinary(t)
	listen := freeAddr(t)
	cfgPath, dbPath := instanceFiles(t, fmt.Sprintf(`listen: %q
instance:
  name: e2e
http:
  allow_networks: [127.0.0.1/32]
  allow_http_hosts: [127.0.0.1]
sources:
  pypi:
    base_url: %s
secrets:
  key_file: %s
exec:
  commands:
    deploy-hook:
      argv: [%[4]s, %[5]s, record, %[6]s, "0"]
    broken-hook:
      argv: [%[4]s, %[5]s, record, %[6]s, "1"]
`, listen, registry.URL, writeKeyFile(t, 7), bin, execHelperArg, runs))
	st, err := sqlite.Open(t.Context(), dbPath, slog.New(slog.DiscardHandler)) // migrate before serve
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	watches := writeFile(t, "w.yaml", `version: 1
sinks:
  - {name: deploy, kind: exec, config: {command: deploy-hook}}
  - {name: broken, kind: exec, config: {command: broken-hook}}
watches:
  - {name: demo, source: pypi, coordinates: {name: demo}}
subscriptions:
  - {name: to-deploy, sink: deploy}
  - {name: to-broken, sink: broken}
`)
	logs := startServe(t, "--config", cfgPath, "--watches", watches, "--roles=all")
	db := openDB(t, dbPath)
	waitFor(t, "the baseline check", logs, func() bool {
		return queryInt(t, db, `SELECT last_checked_at FROM target`) > 0 &&
			queryInt(t, db, `SELECT COUNT(*) FROM watch WHERE baseline_done = 1`) == 1
	})
	reg.set("1.1.0", "1.0.0", "0.9.0")
	recheckSoon(t, db)
	waitFor(t, "both attempts", logs, func() bool {
		return queryInt(t, db, `SELECT COUNT(*) FROM delivery WHERE status = 'delivered'`) == 1 &&
			queryInt(t, db, `SELECT COUNT(*) FROM delivery WHERE status = 'pending' AND attempts >= 1 AND last_error IS NOT NULL`) == 1
	})
	var lastErr string
	if err := db.QueryRowContext(t.Context(), `SELECT last_error FROM delivery WHERE status = 'pending'`).Scan(&lastErr); err != nil {
		t.Fatal(err)
	}
	if lastErr != "exec broken-hook: exit status 1" {
		t.Fatalf("last_error %q", lastErr)
	}
	entries, err := os.ReadDir(runs)
	if err != nil || len(entries) != 2 {
		t.Fatalf("%d runs, %v", len(entries), err)
	}
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(runs, e.Name())) //nolint:gosec // G304: a file under t.TempDir()
		var ev struct {
			Type string `json:"type"`
			Data struct {
				Package, Version string
			} `json:"data"`
		}
		if err := json.Unmarshal(b, &ev); err != nil {
			t.Fatalf("stdin is not the event JSON: %q", b)
		}
		if ev.Type != "dev.angelia.release.published.v1" || ev.Data.Package != "demo" || ev.Data.Version != "1.1.0" {
			t.Fatalf("event %+v", ev)
		}
	}
}

// The examples of docs/exec.md load: the angelia.yaml block with config
// check's rules (but for argv[0], which is not installed here), and the
// watch file block with the command it defines.
func TestExecDocExamples(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "exec.md"))
	if err != nil {
		t.Fatal(err)
	}
	var blocks []string
	for _, part := range strings.Split(string(b), "```yaml\n")[1:] {
		blocks = append(blocks, part[:strings.Index(part, "```\n")])
	}
	if len(blocks) != 2 {
		t.Fatalf("%d yaml blocks in docs/exec.md", len(blocks))
	}
	t.Setenv("ANGELIA_CONFIG", "")
	cfg, err := config.Load(t.Context(), config.Options{Path: writeCfg(t, blocks[0])})
	if err != nil {
		t.Fatal(err)
	}
	if c := cfg.Exec.Commands["deploy-hook"]; c.Argv[0] != "/usr/local/bin/deploy-hook" || c.MaxConcurrent != 1 {
		t.Fatalf("%+v", c)
	}
	reg, err := watchSources(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f, err := watchfile.Parse([]byte(blocks[1]), "exec.md", watchfile.Options{Sources: reg, ExecCommands: cfg.Exec.CommandNames()})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Sinks) != 1 || string(f.Sinks[0].Config) != `{"command":"deploy-hook"}` {
		t.Fatalf("%+v", f.Sinks)
	}
}
