package main

import (
	"context"
	"debug/buildinfo"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// The profiling hook of a -tags benchprof binary (cmd/angelia/benchprof.go):
// it watches ANGELIA_BENCH_PROFILE_DIR for profileRequest and answers with
// the profiles, each renamed into place when complete. No listener is opened.
const (
	profileTag     = "benchprof"
	profileRequest = "profile.request"
)

var profileFiles = []string{"cpu.pprof", "heap.pprof"}

// hasBuildTag reports whether the Go binary at path was built with tag.
func hasBuildTag(path, tag string) bool {
	bi, err := buildinfo.ReadFile(path)
	if err != nil {
		return false
	}
	for _, s := range bi.Settings {
		if s.Key == "-tags" && slices.Contains(strings.Split(s.Value, ","), tag) {
			return true
		}
	}
	return false
}

// profileEnv is the environment that turns the hook on, with dir as angelia
// sees the output directory.
func profileEnv(o options, dir string) []string {
	if !o.profile {
		return nil
	}
	return []string{"ANGELIA_BENCH_PROFILE_DIR=" + dir, "ANGELIA_BENCH_PROFILE_CPU=" + o.profileCPU.String()}
}

// clearProfiles removes the profiles and request of an earlier run in dir.
func clearProfiles(dir string) error {
	for _, name := range append([]string{profileRequest}, profileFiles...) {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func requestProfiles(dir string) error {
	return os.WriteFile(filepath.Join(dir, profileRequest), nil, 0o600)
}

// awaitProfiles waits until deadline for the profiles in dir and returns the
// ones found, adding a note for each missing one.
func awaitProfiles(ctx context.Context, dir string, deadline time.Time, notes []string) ([]string, []string) {
	var found []string
	for _, name := range profileFiles {
		for {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				found = append(found, name)
				break
			}
			if ctx.Err() != nil || time.Now().After(deadline) {
				notes = append(notes, name+": not written by the profiling hook (see angelia.log)")
				break
			}
			select {
			case <-ctx.Done():
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	return found, notes
}
