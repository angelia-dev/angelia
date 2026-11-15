//go:build !windows

package apitoken

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A FIFO as the bootstrap file is refused at once: the start never blocks
// in an open that waits for a writer.
func TestReadBootstrapFileRefusesAFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, _, err := ReadBootstrapFile(path)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("FIFO: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadBootstrapFile blocked on a FIFO")
	}
}
