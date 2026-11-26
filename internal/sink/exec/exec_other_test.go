//go:build !unix

package exec

import (
	"runtime"
	"testing"
)

func TestNewUnsupported(t *testing.T) {
	_, err := New(Options{Commands: map[string]Command{"hook": {Argv: []string{`C:\hook.exe`}}}})
	if err == nil || err.Error() != "exec sinks are not supported on "+runtime.GOOS {
		t.Fatalf("New: %v", err)
	}
}
