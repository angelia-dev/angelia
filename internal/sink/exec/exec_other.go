//go:build !unix

package exec

import (
	"fmt"
	osexec "os/exec"
	"runtime"
)

func supported() error { return fmt.Errorf("exec sinks are not supported on %s", runtime.GOOS) }

func prepare(*osexec.Cmd, *runState) {}

func killGroupAtExit(*osexec.Cmd) bool { return false }

func killGroup(*osexec.Cmd) {}

func startError(name string, _ error) error { return fmt.Errorf("exec %s: could not start", name) }

func exitError(name string, _ error) error { return fmt.Errorf("exec %s: failed", name) }
