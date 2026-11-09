package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Targets of benchmark 2, architecture.md section 6, in MB as written there.
const (
	targetIdleMemMB = 40
	targetBinaryMB  = 40
	targetImageMB   = 50
)

// Idle is what benchmark 2 measures. Memory covers the samples after the
// warm-up. Binary is the stripped release build (-measure-binary), checked
// against the target; RunBinary is the binary that ran, information only.
// The image is checked uncompressed; its compressed size is information.
type Idle struct {
	MemStartMiB          float64 `json:"memory_start_mib"`
	MemEndMiB            float64 `json:"memory_end_mib"`
	MemMaxMiB            float64 `json:"memory_max_mib"`
	MemSamples           int     `json:"memory_samples"`
	Binary               string  `json:"binary"`
	BinaryBytes          int64   `json:"binary_bytes"`
	RunBinary            string  `json:"run_binary"`
	RunBinaryBytes       int64   `json:"run_binary_bytes"`
	Image                string  `json:"image,omitempty"`
	ImageBytes           int64   `json:"image_bytes,omitempty"`
	ImageCompressedBytes int64   `json:"image_compressed_bytes,omitempty"`
}

func fileSize(path string) int64 {
	if fi, err := os.Stat(path); err == nil && path != "" {
		return fi.Size()
	}
	return 0
}

// measureIdle measures benchmark 2 and returns notes on what it could not.
func measureIdle(ctx context.Context, o options, samples []Sample, after time.Time) (*Idle, []string) {
	id := &Idle{Binary: o.measureBinary, RunBinary: o.bin, Image: o.measureImage}
	var notes []string
	for _, s := range samples {
		if s.At.Before(after) {
			continue
		}
		mib := float64(s.MemoryBytes) / (1 << 20)
		if id.MemSamples == 0 {
			id.MemStartMiB = mib
		}
		id.MemSamples++
		id.MemEndMiB, id.MemMaxMiB = mib, max(id.MemMaxMiB, mib)
	}
	id.BinaryBytes, id.RunBinaryBytes = fileSize(o.measureBinary), fileSize(o.bin)
	if o.measureBinary == "" {
		notes = append(notes, "binary size not measured: pass -measure-binary, a build with the release flags (-trimpath -ldflags \"-s -w\")")
	}
	if o.measureImage != "" {
		out, err := o.probe.run(ctx, "docker", "image", "inspect", "--format", "{{.Size}}", o.measureImage)
		if err == nil {
			id.ImageBytes, err = strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		}
		if err != nil {
			notes = append(notes, fmt.Sprintf("image size not measured: %v", err))
		}
		// Compressed as pushed to a registry, roughly: information only.
		out, err = o.probe.run(ctx, "sh", "-c", `docker save "$1" | gzip -c | wc -c`, "sh", o.measureImage)
		if err == nil {
			id.ImageCompressedBytes, _ = strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		}
	}
	return id, notes
}

// checks are benchmark 2's targets; the image is optional, since it needs Docker.
func (id *Idle) checks(memSource string) []Check {
	memTarget := targetIdleMemMB * 1e6 / (1 << 20)
	img := check("image size, uncompressed ("+id.Image+")", fmt.Sprintf("< %d MB", targetImageMB), mb(id.ImageBytes),
		id.ImageBytes > 0, id.ImageBytes < targetImageMB*1e6)
	img.Optional = true
	if id.Image == "" {
		img.Name = "image size (no Docker)"
	}
	return []Check{
		check("idle "+memSource+" max after warm-up", fmt.Sprintf("< %.1f MiB (%d MB)", memTarget, targetIdleMemMB),
			fmt.Sprintf("%.1f MiB", id.MemMaxMiB), id.MemSamples > 0, id.MemMaxMiB < memTarget),
		check("binary size, release flags ("+filepath.Base(id.Binary)+")", fmt.Sprintf("< %d MB", targetBinaryMB), mb(id.BinaryBytes),
			id.BinaryBytes > 0, id.BinaryBytes < targetBinaryMB*1e6),
		img,
	}
}

// markdown is the idle section of report.md.
func (id *Idle) markdown() string {
	s := fmt.Sprintf("\n## Idle footprint\n\nMemory after the warm-up: %.1f MiB at the start, %.1f MiB at the end, %.1f MiB max (%d samples).\n",
		id.MemStartMiB, id.MemEndMiB, id.MemMaxMiB, id.MemSamples)
	s += fmt.Sprintf("\nInformation: the binary that ran (`%s`, `make build`) is %s.", filepath.Base(id.RunBinary), mb(id.RunBinaryBytes))
	if id.ImageCompressedBytes > 0 {
		s += fmt.Sprintf(" The image is %s compressed (`docker save | gzip`).", mb(id.ImageCompressedBytes))
	}
	return s + "\n"
}

func mb(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/1e6) }
