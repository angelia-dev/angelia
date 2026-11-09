//go:build benchprof

package main

import (
	"context"
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
	if err := writeBenchProfiles(filepath.Join(dir, "missing"), time.Millisecond); err == nil {
		t.Error("unwritable directory: want error")
	}
}
