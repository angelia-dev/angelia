//go:build unix && !linux

package exec

import (
	osexec "os/exec"
	"syscall"
)

func setPdeathsig(*syscall.SysProcAttr) {}

// killGroupAtExit is Linux only (waitid with WNOWAIT): elsewhere run kills
// the group just after Wait has reaped the leader.
func killGroupAtExit(*osexec.Cmd) bool { return false }
