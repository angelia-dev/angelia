//go:build unix && !linux

package exec

import "syscall"

func setPdeathsig(*syscall.SysProcAttr) {}
