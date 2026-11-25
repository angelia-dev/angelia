package config

import (
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Bounds of exec.commands (ADR 0042 §4). internal/sink/exec has the same
// values; a test in cmd/angelia checks that they agree.
const (
	ExecDefaultTimeout       = 10 * time.Second
	ExecMinTimeout           = time.Second
	ExecMaxTimeout           = 12 * time.Second
	ExecDefaultMaxConcurrent = 1
	ExecMaxMaxConcurrent     = 8
	ExecMaxArgs              = 64
)

// execNameRE is the pattern of a command name: a sink name (watch file
// NamePattern), so that a sink's config can name it.
var execNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// Exec is the exec section: the commands exec sinks may run (ADR 0042).
// Only the instance configuration defines them, and there is no
// environment override: a sink (watch file, API, UI) can only name one.
type Exec struct {
	Commands map[string]ExecCommand `yaml:"commands"`
}

// ExecCommand is exec.commands.<name>.
type ExecCommand struct {
	// Argv is the command line, started without a shell; Argv[0] is an
	// absolute path.
	Argv []string `yaml:"argv"`
	// Dir is the working directory, absolute; default "/".
	Dir string `yaml:"dir"`
	// Env names variables copied from Angelia's environment into the
	// command's, when set. Never an ANGELIA_ name.
	Env []string `yaml:"env"`
	// Timeout bounds one run, 1s to 12s; 0 means 10s.
	Timeout time.Duration `yaml:"timeout"`
	// MaxConcurrent bounds the runs at the same time, 1 to 8; 0 means 1.
	MaxConcurrent int `yaml:"max_concurrent"`
}

// ExecSupported reports whether this platform runs exec sinks (Linux and
// macOS; ADR 0042 §5).
func ExecSupported() bool { return execSupported }

// CommandNames returns the names of exec.commands, sorted.
func (e Exec) CommandNames() []string { return slices.Sorted(maps.Keys(e.Commands)) }

func (e Exec) validate() []error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if len(e.Commands) > 0 && !execSupported {
		bad("exec.commands: exec sinks are not supported on %s", runtime.GOOS)
		return errs
	}
	for _, name := range e.CommandNames() {
		c := e.Commands[name]
		if !execNameRE.MatchString(name) {
			bad("exec.commands: invalid command name %q (want ^[a-z0-9][a-z0-9._-]{0,62}$)", name)
			continue
		}
		key := "exec.commands." + name
		switch {
		case len(c.Argv) == 0:
			bad("%s.argv: required, a list whose first entry is the absolute path of the program", key)
		case len(c.Argv) > ExecMaxArgs:
			bad("%s.argv: at most %d entries", key, ExecMaxArgs)
		case !filepath.IsAbs(c.Argv[0]) || filepath.Clean(c.Argv[0]) != c.Argv[0]:
			bad("%s.argv[0]: must be an absolute, clean path such as /usr/local/bin/hook (no PATH lookup, no shell)", key)
		}
		for i, a := range c.Argv {
			if strings.ContainsRune(a, 0) {
				bad("%s.argv[%d]: must not contain a NUL byte", key, i)
			}
		}
		if c.Dir != "" && (!filepath.IsAbs(c.Dir) || strings.ContainsRune(c.Dir, 0)) {
			bad("%s.dir: must be an absolute path", key)
		}
		seen := map[string]bool{}
		for i, n := range c.Env {
			switch {
			case !envNameRE.MatchString(n):
				bad("%s.env[%d]: %q is not a valid environment variable name", key, i, n)
			case ReservedEnvName(n):
				bad("%s.env[%d]: %s", key, i, ReservedEnvRefusal)
			case seen[n]:
				bad("%s.env[%d]: %s is listed twice", key, i, n)
			}
			seen[n] = true
		}
		if c.Timeout != 0 && (c.Timeout < ExecMinTimeout || c.Timeout > ExecMaxTimeout) {
			bad("%s.timeout: must be from %v to %v, got %v", key, ExecMinTimeout, ExecMaxTimeout, c.Timeout)
		}
		if c.MaxConcurrent < 0 || c.MaxConcurrent > ExecMaxMaxConcurrent {
			bad("%s.max_concurrent: must be from 1 to %d, got %d", key, ExecMaxMaxConcurrent, c.MaxConcurrent)
		}
	}
	return errs
}

// execWarnings warns about a listed variable that is not set.
func (e Exec) execWarnings(env map[string]string) []string {
	var w []string
	for _, name := range e.CommandNames() {
		for _, n := range e.Commands[name].Env {
			if _, ok := env[n]; !ok {
				w = append(w, fmt.Sprintf("exec.commands.%s.env: environment variable %s is not set; the command will not get it", name, n))
			}
		}
	}
	return w
}
