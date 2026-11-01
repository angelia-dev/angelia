// Package goplugintest starts the crates go-plugin from a test binary: the
// test binary re-executes itself as the plugin, so tests need no separate
// build step. Production starts the angelia-plugin-crates binary instead.
package goplugintest

import (
	"os"
	"os/exec"

	"github.com/angelia-dev/angelia/bench/plugins/goplugin/crates"
)

const envServe = "ANGELIA_GOPLUGIN_TEST_SERVE"

// ServeIfChild serves the crates plugin and returns true when the process
// was started by Command. Call it first in TestMain and return if it
// returns true.
func ServeIfChild() bool {
	if os.Getenv(envServe) != "crates" {
		return false
	}
	crates.Serve()
	return true
}

// Command returns a command that starts the current test binary as the
// crates plugin. Its environment holds only the switch.
func Command() *exec.Cmd {
	cmd := exec.Command(os.Args[0]) //nolint:gosec // G204: the test binary itself.
	cmd.Env = []string{envServe + "=crates"}
	return cmd
}
