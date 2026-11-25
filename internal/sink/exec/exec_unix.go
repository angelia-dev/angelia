//go:build unix

package exec

import (
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"syscall"

	"github.com/angelia-dev/angelia/internal/sink"
)

func supported() error { return nil }

// prepare puts the child in its own process group, so that a timeout or a
// cancellation reaches every process it started: SIGTERM to the group when
// the context is done, then SIGKILL to the group KillGrace later (st's
// timer, or run once the leader has exited; os/exec also kills the leader
// at WaitDelay). Cancel records in st that it fired.
func prepare(cmd *osexec.Cmd, st *runState) {
	attr := &syscall.SysProcAttr{Setpgid: true}
	setPdeathsig(attr)
	cmd.SysProcAttr = attr
	cmd.Cancel = func() error {
		pid := cmd.Process.Pid
		if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return fmt.Errorf("signal process group: %w", err)
		}
		st.fired.Store(true)
		st.armKill(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
		return nil
	}
}

// killGroup sends SIGKILL to the child's process group.
func killGroup(cmd *osexec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// startError classifies a failed start: a command that is missing, not
// executable or not a valid executable is permanent; anything else (out
// of processes or memory) is retried. The text names the errno only.
func startError(name string, err error) error {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return fmt.Errorf("exec %s: could not start", name)
	}
	e := fmt.Errorf("exec %s: could not start: %s", name, errno.Error())
	switch errno {
	case syscall.ENOENT, syscall.EACCES, syscall.EPERM, syscall.ENOEXEC, syscall.ENOTDIR, syscall.EISDIR, syscall.ELOOP, syscall.ENAMETOOLONG:
		return sink.Permanent(e)
	}
	return e
}

// exitError describes a run that ended in failure, never with its output.
func exitError(name string, err error) error {
	var ee *osexec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return fmt.Errorf("exec %s: killed by signal %d", name, int(ws.Signal()))
		}
		return fmt.Errorf("exec %s: exit status %d", name, ee.ExitCode())
	}
	return fmt.Errorf("exec %s: failed", name)
}
