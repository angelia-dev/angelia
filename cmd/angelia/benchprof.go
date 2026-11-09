//go:build benchprof

// Profiling hook for the Phase 0 benchmarks (bench/README.md, story #63). It
// is compiled only with -tags benchprof, never into a release binary, and
// opens no listener: with ANGELIA_BENCH_PROFILE_DIR set, it polls that
// directory for the file profile.request. When the harness creates it, the
// hook removes it, records a CPU profile for ANGELIA_BENCH_PROFILE_CPU
// (default 30s) into cpu.pprof, then writes heap.pprof after a GC. Each file
// is written to a new temporary file and renamed into place when complete.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"time"
)

const benchProfileRequest = "profile.request"

func init() {
	dir := os.Getenv("ANGELIA_BENCH_PROFILE_DIR")
	if dir == "" {
		return
	}
	cpu := 30 * time.Second
	if v := os.Getenv("ANGELIA_BENCH_PROFILE_CPU"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			slog.Error("benchprof: ANGELIA_BENCH_PROFILE_CPU must be a positive duration", "value", v)
			return
		}
		cpu = d
	}
	go watchProfileRequest(context.Background(), dir, cpu, time.Second)
}

// watchProfileRequest serves each profile request found in dir until ctx ends.
func watchProfileRequest(ctx context.Context, dir string, cpu, poll time.Duration) {
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if os.Remove(filepath.Join(dir, benchProfileRequest)) != nil { //nolint:gosec // G703: the operator's directory
			continue // no request
		}
		if err := writeBenchProfiles(dir, cpu); err != nil {
			slog.Error("benchprof: cannot write profiles", "dir", dir, "error", err)
		}
	}
}

func writeBenchProfiles(dir string, cpu time.Duration) error {
	err := writeAtomic(filepath.Join(dir, "cpu.pprof"), func(w io.Writer) error {
		if err := pprof.StartCPUProfile(w); err != nil {
			return err
		}
		time.Sleep(cpu)
		pprof.StopCPUProfile()
		return nil
	})
	if err != nil {
		return err
	}
	runtime.GC() // the heap profile shows the state as of the last GC
	return writeAtomic(filepath.Join(dir, "heap.pprof"), func(w io.Writer) error {
		return pprof.Lookup("heap").WriteTo(w, 0)
	})
}

func writeAtomic(path string, write func(io.Writer) error) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp") // O_EXCL
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name()) //nolint:gosec // G703: the temporary file just created
		}
	}()
	if err := write(f); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path) //nolint:gosec // G703: the operator's directory
}
