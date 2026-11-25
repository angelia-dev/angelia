//go:build !unix

package config

import (
	"runtime"
	"strings"
	"testing"
)

func TestExecCommandsUnsupported(t *testing.T) {
	_, err := load(t, writeFile(t, "exec:\n  commands:\n    a:\n      argv: [C:\\hook.exe]\n"))
	if err == nil || !strings.Contains(err.Error(), "exec sinks are not supported on "+runtime.GOOS) {
		t.Fatalf("error %v", err)
	}
}
