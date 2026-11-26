package main

import (
	"bytes"
	"syscall"
	"testing"
)

// ADR 0042 §3: serve marks itself not dumpable before anything else, so a
// command running as the same user cannot read /proc/<pid>/environ.
func TestServeSetsNotDumpable(t *testing.T) {
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 1, 0); e != 0 {
		t.Fatal(e)
	}
	if d, err := dumpable(); err != nil || d != 1 {
		t.Fatalf("dumpable %d, %v", d, err)
	}
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"serve", "--config", writeCfg(t, "bogus: 1\n")})
	if err := root.ExecuteContext(t.Context()); err == nil {
		t.Fatal("serve accepted an invalid configuration")
	}
	if d, err := dumpable(); err != nil || d != 0 {
		t.Fatalf("after serve: dumpable %d, %v", d, err)
	}
}
