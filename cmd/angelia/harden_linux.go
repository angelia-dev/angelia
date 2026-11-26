package main

import (
	"fmt"
	"syscall"
)

// prctl(2) options, not in package syscall.
const (
	prGetDumpable = 3
	prSetDumpable = 4
)

// setNotDumpable marks the serve process not dumpable (prctl
// PR_SET_DUMPABLE 0): its /proc/<pid>/environ, mem and fds become owned by
// root, so another process of the same user, such as an exec sink's
// command, cannot read the environment Angelia started with (the secrets
// key, tokens) through /proc (ADR 0042 §3). It also disables core dumps.
// Children get the flag reset at execve.
func setNotDumpable() error {
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); e != 0 {
		return fmt.Errorf("prctl PR_SET_DUMPABLE: %w", e)
	}
	return nil
}

// dumpable reports the process's dumpable flag, for tests.
func dumpable() (int, error) {
	r, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prGetDumpable, 0, 0)
	if e != 0 {
		return 0, e
	}
	return int(r), nil
}
