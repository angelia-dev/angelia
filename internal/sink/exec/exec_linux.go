package exec

import "syscall"

// setPdeathsig kills the direct child if Angelia dies (ADR 0042 §4).
func setPdeathsig(attr *syscall.SysProcAttr) { attr.Pdeathsig = syscall.SIGKILL }
