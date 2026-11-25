package exec

import (
	osexec "os/exec"
	"syscall"
	"unsafe"
)

// setPdeathsig kills the direct child if the thread that started it
// exits, which run's locked thread ties to Angelia's lifetime (ADR 0042
// §4). It reaches the direct child only, not its descendants.
func setPdeathsig(attr *syscall.SysProcAttr) { attr.Pdeathsig = syscall.SIGKILL }

// Constants of waitid(2), not in package syscall.
const (
	pPID    = 1
	wExited = 0x4
	wNoWait = 0x1000000
)

// killGroupAtExit waits until the leader has exited, without reaping it
// (waitid with WNOWAIT), then sends SIGKILL to its process group. While
// the leader is an unreaped zombie its pid, the group id, cannot be
// reused, so the signal reaches only what is left of this run. It reports
// false when the wait failed; run then kills the group after Wait.
func killGroupAtExit(cmd *osexec.Cmd) bool {
	var info [128]byte // siginfo_t
	for {
		_, _, e := syscall.Syscall6(syscall.SYS_WAITID, pPID, uintptr(cmd.Process.Pid),
			uintptr(unsafe.Pointer(&info[0])), wExited|wNoWait, 0, 0) //nolint:gosec // G103: siginfo_t buffer for waitid(2)
		if e == syscall.EINTR {
			continue
		}
		if e != 0 {
			return false
		}
		killGroup(cmd)
		return true
	}
}
