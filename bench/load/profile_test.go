package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A run with -profile asks the hook for profiles before the end and links
// them from report.md; a stale profile of an earlier run is removed first.
func TestRunCollectsProfiles(t *testing.T) {
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "heap.pprof"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fakeProfileHook(ctx, t, out) // stands in for cmd/angelia/benchprof.go
	o := testOptions(t, out)
	o.profile, o.profileCPU = true, 20*time.Millisecond
	rep, err := bench(context.Background(), o, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rep.Profiles, " ") != "cpu.pprof heap.pprof" {
		t.Errorf("profiles = %v, notes %v", rep.Profiles, rep.Notes)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "heap.pprof")); string(b) != "profile" { //nolint:gosec // test temp dir
		t.Errorf("heap.pprof = %q, want the new profile", b)
	}
	md, _ := os.ReadFile(filepath.Join(out, "report.md")) //nolint:gosec // test temp dir
	for _, want := range []string{"## Profiles", "CPU profile for 20ms from 20", "[cpu.pprof](cpu.pprof)", "[heap.pprof](heap.pprof)"} {
		if !bytes.Contains(md, []byte(want)) {
			t.Errorf("report.md lacks %q", want)
		}
	}
	// Nobody answers: the run still reports, with a note per missing profile.
	notes := []string{"x"}
	found, notes := awaitProfiles(context.Background(), t.TempDir(), time.Now(), notes)
	if len(found) != 0 || len(notes) != 3 || !strings.Contains(notes[1], "cpu.pprof") {
		t.Errorf("found %v, notes %v", found, notes)
	}
}

func fakeProfileHook(ctx context.Context, t *testing.T, dir string) {
	for ctx.Err() == nil {
		if os.Remove(filepath.Join(dir, profileRequest)) == nil {
			for _, f := range profileFiles {
				if err := os.WriteFile(filepath.Join(dir, f), []byte("profile"), 0o600); err != nil {
					t.Error(err)
				}
			}
		}
		time.Sleep(time.Millisecond)
	}
}

func testOptions(t *testing.T, out string) options {
	t.Helper()
	ft := &fakeTarget{srv: httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))} // 200 everywhere
	t.Cleanup(ft.srv.Close)
	cfgPath := filepath.Join(out, "fakereg.yaml")
	if err := os.WriteFile(cfgPath, []byte("defaults: {latency: 0s, latency_jitter: 0s}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return options{
		n: 5, duration: 40 * time.Millisecond, interval: 5 * time.Millisecond, fakeregConfig: cfgPath,
		bin: "bin/angelia", out: out, readyTimeout: 5 * time.Second, loader: watchFileLoader{interval: 5 * time.Minute},
		checkInterval: 5 * time.Millisecond, releaseCensor: 10 * time.Millisecond, hostLimit: DefaultHostLimit,
		newTarget: func(options, string) Target { return ft },
		probe:     fakeProbe(nil, map[string]string{"git rev-parse": "deadbeef", "docker image": "21000000\n"}),
	}
}

// Benchmark 2 checks memory after the warm-up, the binary that ran and the image.
func TestRunIdle(t *testing.T) {
	out := t.TempDir()
	o := testOptions(t, out)
	o.kind, o.measureImage = "idle", "angelia-bench:idle"
	o.bin = filepath.Join(out, "angelia")
	if err := os.WriteFile(o.bin, make([]byte, 1000), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := bench(context.Background(), o, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	id := rep.Idle
	if id == nil || id.MemSamples == 0 || id.MemSamples >= len(rep.Samples) || id.MemMaxMiB != 25 || id.BinaryBytes != 1000 || id.ImageBytes != 21e6 {
		t.Fatalf("idle = %+v (%d samples)", id, len(rep.Samples))
	}
	if rep.Verdict != "pass" || len(rep.Checks) != 3 || rep.Params.Kind != "idle" || !strings.Contains(rep.Benchmark, "benchmark 2") {
		t.Errorf("verdict %s, checks %+v", rep.Verdict, rep.Checks)
	}
	md, _ := os.ReadFile(filepath.Join(out, "report.md")) //nolint:gosec // test temp dir
	for _, want := range []string{"| idle RSS (VmRSS) max after warm-up | < 38.1 MiB (40 MB) | 25.0 MiB | pass |", "| binary size (angelia) | < 40 MB | 0.0 MB | pass |", "| image size (angelia-bench:idle) | < 50 MB | 21.0 MB | pass |"} {
		if !bytes.Contains(md, []byte(want)) {
			t.Errorf("report.md lacks %q:\n%s", want, md)
		}
	}
}

func TestIdleChecks(t *testing.T) {
	for name, tc := range map[string]struct {
		idle Idle
		want string
	}{
		"pass":                {Idle{MemMaxMiB: 30, MemSamples: 3, BinaryBytes: 30e6, Image: "i", ImageBytes: 40e6}, "pass"},
		"no Docker":           {Idle{MemMaxMiB: 30, MemSamples: 3, BinaryBytes: 30e6}, "pass"},
		"memory fails":        {Idle{MemMaxMiB: 38.2, MemSamples: 3, BinaryBytes: 30e6}, "fail"},
		"binary fails":        {Idle{MemMaxMiB: 30, MemSamples: 3, BinaryBytes: 40e6}, "fail"},
		"image fails":         {Idle{MemMaxMiB: 30, MemSamples: 3, BinaryBytes: 30e6, Image: "i", ImageBytes: 50e6}, "fail"},
		"no memory sample":    {Idle{BinaryBytes: 30e6}, "incomplete"},
		"binary not measured": {Idle{MemMaxMiB: 30, MemSamples: 3}, "incomplete"},
	} {
		t.Run(name, func(t *testing.T) {
			r := &Report{Idle: &tc.idle, Resources: Resources{MemorySource: "RSS"}}
			r.evaluate()
			if r.Verdict != tc.want {
				t.Errorf("verdict = %s, want %s (checks %+v)", r.Verdict, tc.want, r.Checks)
			}
		})
	}
	r := &Report{Idle: &Idle{}}
	r.evaluate()
	if c := r.Checks[2]; c.Name != "image size (no Docker)" || c.Result != "not measured" || !c.Optional {
		t.Errorf("image check = %+v", c)
	}
}

func TestProfileHelpers(t *testing.T) {
	self, _ := os.Executable()
	if hasBuildTag(self, profileTag) || hasBuildTag("/does/not/exist", profileTag) {
		t.Error("the test binary is not built with -tags benchprof")
	}
	o := options{profile: true, profileCPU: time.Minute}
	if env := strings.Join(profileEnv(o, "/work"), " "); env != "ANGELIA_BENCH_PROFILE_DIR=/work ANGELIA_BENCH_PROFILE_CPU=1m0s" {
		t.Errorf("env = %q", env)
	}
	if profileEnv(options{}, "/work") != nil {
		t.Error("no -profile: no env")
	}
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"-profile", "-angelia", self}, &bytes.Buffer{}, &stderr); code != 2 || !strings.Contains(stderr.String(), "-tags benchprof") {
		t.Errorf("-profile with a binary without the hook: exit %d, %s", code, stderr.String())
	}
	if code := run(context.Background(), []string{"-profile", "-profile-cpu", "0s"}, &bytes.Buffer{}, &stderr); code != 2 {
		t.Errorf("-profile-cpu 0: exit %d", code)
	}
	if code := run(context.Background(), []string{"-benchmark", "fast"}, &bytes.Buffer{}, &stderr); code != 2 {
		t.Errorf("-benchmark fast: exit %d", code)
	}
	if err := clearProfiles(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Errorf("clearProfiles on a new directory: %v", err)
	}
}
