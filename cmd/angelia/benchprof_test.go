//go:build benchprof

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Run with: go test -tags benchprof -run TestBenchProfile ./cmd/angelia
// A request file yields a CPU and a heap profile, and is consumed.
func TestBenchProfileRequest(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchProfileRequest(ctx, dir, 50*time.Millisecond, 10*time.Millisecond)
	if err := os.WriteFile(filepath.Join(dir, benchProfileRequest), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for _, name := range []string{"cpu.pprof", "heap.pprof"} {
		for {
			if fi, err := os.Stat(filepath.Join(dir, name)); err == nil && fi.Size() > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s not written", name)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, benchProfileRequest)); !os.IsNotExist(err) {
		t.Errorf("request not consumed: %v", err)
	}
}

// An unwritable directory fails without leaving a temporary file behind.
func TestBenchProfileUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // the test's own directory
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // let TempDir clean up
	if err := writeBenchProfiles(dir, time.Millisecond); err == nil {
		t.Error("read-only directory: want error")
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

// A failing write removes its temporary file.
func TestBenchProfileWriteRemovesTemp(t *testing.T) {
	dir := t.TempDir()
	err := writeAtomic(filepath.Join(dir, "x.pprof"), func(io.Writer) error { return errors.New("boom") })
	if err == nil {
		t.Fatal("want error")
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}
