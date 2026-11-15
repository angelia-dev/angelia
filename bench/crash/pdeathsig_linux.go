package main

import (
	"os/exec"
	"syscall"
)

// setPdeathsig makes the kernel SIGKILL the child when the harness thread
// that started it ends, so that a crashed or interrupted harness leaves no
// replica running on the database.
func setPdeathsig(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
