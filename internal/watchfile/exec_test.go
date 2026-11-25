package watchfile

import (
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/sink"
	execsink "github.com/angelia-dev/angelia/internal/sink/exec"
)

func execSink(config string) string {
	return base + "sinks:\n  - name: deploy\n    kind: exec\n    config: " + config + "\n"
}

func execOpts(t *testing.T) Options {
	o := opts(t)
	o.ExecCommands = []string{"deploy-hook", "notify"}
	return o
}

// ADR 0042 §1: a sink only names a command of exec.commands.
func TestExecStoredConfig(t *testing.T) {
	f, err := Parse([]byte(execSink("{command: deploy-hook}")), "w.yaml", execOpts(t))
	if err != nil {
		t.Fatal(err)
	}
	if f.Sinks[0].Kind != "exec" || string(f.Sinks[0].Config) != `{"command":"deploy-hook"}` {
		t.Fatalf("%+v", f.Sinks[0])
	}
	if !config.ExecSupported() {
		return
	}
	s, err := execsink.New(execsink.Options{Commands: map[string]execsink.Command{"deploy-hook": {Argv: []string{"/bin/true"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(sink.Config(f.Sinks[0].Config)); err != nil {
		t.Fatalf("the sink rejects what the file accepted: %v", err)
	}
}

func TestExecRejected(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"unknown command": {`{command: other}`, `w.yaml: line 5: sinks[0].config.command: no command named "other" under exec.commands in angelia.yaml (commands: deploy-hook, notify)`},
		"no command":      {`{}`, `config.command: missing required key "command"`},
		"argv":            {`{command: notify, argv: [/bin/sh]}`, `config.argv: unknown key "argv"`},
		"not a string":    {`{command: [a]}`, "config.command"},
	} {
		_, err := Parse([]byte(execSink(tc.yaml)), "w.yaml", execOpts(t))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want %q", name, err, tc.want)
		}
	}
}

// Without exec.commands the kind is refused, pointing to angelia.yaml.
func TestExecKindNeedsCommands(t *testing.T) {
	_, err := Parse([]byte(execSink("{command: deploy-hook}")), "w.yaml", opts(t))
	if err == nil || !strings.Contains(err.Error(), "sinks[0].kind: the exec kind is available only when the instance defines commands under exec.commands in angelia.yaml") {
		t.Fatalf("error %v", err)
	}
	if strings.Contains(err.Error(), "config") {
		t.Fatalf("the config is checked without the kind: %v", err)
	}
}
