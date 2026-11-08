package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/bench/fakereg"
	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/crates"
	"github.com/angelia-dev/angelia/internal/source/githubrelease"
	"github.com/angelia-dev/angelia/internal/source/npm"
	"github.com/angelia-dev/angelia/internal/source/pypi"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

// Watches go round robin over the ecosystems that have a built-in source;
// oci has none yet and gets no watch.
func TestGenerateWatches(t *testing.T) {
	w, per := GenerateWatches(7)
	if len(w) != 7 || per["pypi"] != 2 || per["oci"] != 0 {
		t.Fatalf("got %d watches, per ecosystem %v; want 7, pypi 2, oci 0", len(w), per)
	}
	kinds := map[string]int{}
	for _, x := range w {
		kinds[x.Kind]++
	}
	want := map[string]int{"pypi": 2, "npm": 2, "crates": 2, "github-release": 1}
	for k, n := range want {
		if kinds[k] != n {
			t.Errorf("%s: %d watches, want %d (all: %v)", k, kinds[k], n, kinds)
		}
	}
	if len(kinds) != len(want) {
		t.Errorf("kinds = %v", kinds)
	}
	if w[3].Coordinates["repo"] != "bench/pkg-00000" || w[5].Coordinates["name"] != "pkg-00001" {
		t.Errorf("coordinates: %+v / %+v", w[3], w[5])
	}
	if got := unmeasuredEcosystems(); len(got) != 1 || got[0] != "oci" {
		t.Errorf("unmeasured = %v", got)
	}
	c := fakereg.DefaultConfig()
	SizeRegistry(c, per)
	if b := c.Ecosystems["oci"]; b.Packages != 1 || b.ReleasesPerHour != 0 || c.Ecosystems["npm"].Packages != 2 {
		t.Error("SizeRegistry did not size each ecosystem to its watches")
	}
	_, per = GenerateWatches(2)
	SizeRegistry(c, per)
	if b := c.Ecosystems["github"]; b.Packages != 1 || b.ReleasesPerHour != 0 {
		t.Errorf("unwatched ecosystem = %+v, want 1 package without releases", b)
	}
	if c.Ecosystems["pypi"].ReleasesPerHour == 0 {
		t.Error("watched ecosystem lost its releases")
	}
}

// The harness writes the existing instance configuration format; angelia's
// own loader must accept it.
func TestAngeliaConfigIsValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "angelia.yaml")
	if err := WriteAngeliaConfig(path, "127.0.0.1:9", "http://127.0.0.1:5000", dir, "127.0.0.1/32"); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(context.Background(), config.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Sources["github-release"].BaseURL; got != "http://127.0.0.1:5000/github" {
		t.Errorf("github-release base_url = %q", got)
	}
	if got := cfg.Sources["crates"].BaseURL; got != "http://127.0.0.1:5000/crates" {
		t.Errorf("crates base_url = %q", got)
	}
	if len(cfg.Sources) != 4 {
		t.Errorf("sources = %v", cfg.Sources)
	}
	// Only the registry's address is allowed past the SSRF guard.
	if p := cfg.HTTP.AllowPrefixes(); len(p) != 1 || p[0].String() != "127.0.0.1/32" {
		t.Errorf("allow_networks = %v", p)
	}
}

// The loader writes a watch file that angelia's own parser accepts, with
// one watch per target at the requested interval, and returns the serve
// flag naming it as angelia sees it.
func TestWatchFileLoader(t *testing.T) {
	dir := t.TempDir()
	watches, _ := GenerateWatches(10000)
	l := watchFileLoader{interval: 5 * time.Minute}
	args, err := l.Prepare(context.Background(), watches, dir, "/work")
	if err != nil {
		t.Fatal(err)
	}
	if l.Name() != "watch-file" || len(args) != 2 || args[0] != "--watches" || args[1] != "/work/watches.yaml" {
		t.Errorf("name %q, args %v", l.Name(), args)
	}
	f, err := watchfile.Load(filepath.Join(dir, watchFileName), watchfile.Options{Sources: benchSources(t)})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Watches) != 10000 || len(f.Sinks) != 0 || len(f.Subscriptions) != 0 {
		t.Fatalf("watches %d, sinks %d, subscriptions %d", len(f.Watches), len(f.Sinks), len(f.Subscriptions))
	}
	per := map[string]int{}
	for _, w := range f.Watches {
		per[w.Source]++
		if w.Interval != 5*time.Minute {
			t.Fatalf("watch %s: interval %v", w.Name, w.Interval)
		}
	}
	if per["pypi"] != 2500 || per["github-release"] != 2500 || per["crates"] != 2500 || per["npm"] != 2500 {
		t.Errorf("per source = %v", per)
	}
	if w := f.Watches[3]; w.Name != "github-release-pkg-00000" || w.Coordinates["repo"] != "bench/pkg-00000" {
		t.Errorf("watch 3 = %+v", w)
	}
}

func TestWatchFileLoaderRefuses(t *testing.T) {
	watches, _ := GenerateWatches(4)
	if _, err := (watchFileLoader{interval: time.Minute}).Prepare(context.Background(), watches, t.TempDir(), "/w"); err == nil ||
		!strings.Contains(err.Error(), "5m0s") {
		t.Errorf("1m interval: err = %v", err)
	}
	big, _ := GenerateWatches(60000)
	if _, err := (watchFileLoader{interval: time.Hour}).Prepare(context.Background(), big, t.TempDir(), "/w"); err == nil ||
		!strings.Contains(err.Error(), "limit") {
		t.Errorf("oversized file: err = %v", err)
	}
	if _, err := (watchFileLoader{interval: time.Hour}).Prepare(context.Background(), watches, filepath.Join(t.TempDir(), "missing"), "/w"); err == nil {
		t.Error("unwritable directory: want error")
	}
}

// benchSources registers the built-in source kinds the harness watches,
// for validation only (no request is made).
func benchSources(t *testing.T) *source.Registry {
	t.Helper()
	reg := source.NewRegistry()
	hc := &http.Client{}
	for _, s := range []source.Source{pypi.New("", hc), npm.New("", hc), crates.New("", hc), githubrelease.New("", hc)} {
		if err := reg.Register(s); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

func TestRegistryNetwork(t *testing.T) {
	for _, tc := range []struct {
		container bool
		host      string
		want      string
	}{
		{false, "127.0.0.1", "127.0.0.1/32"},
		{true, "172.17.0.1", "172.17.0.1/32"},
	} {
		if got, err := registryNetwork(tc.container, tc.host); err != nil || got != tc.want {
			t.Errorf("registryNetwork(%v, %s) = %q, %v; want %q", tc.container, tc.host, got, err, tc.want)
		}
	}
	// Docker Desktop (registry on the host's loopback) and an unknown
	// gateway: no guess, the operator names the address.
	for _, host := range []string{"127.0.0.1", "0.0.0.0"} {
		if _, err := registryNetwork(true, host); err == nil || !strings.Contains(err.Error(), "-allow-network") {
			t.Errorf("container, registry on %s: err = %v", host, err)
		}
	}
}

func TestProcessSample(t *testing.T) {
	stat := "4242 (angelia serve) S 1 2 3 4 5 6 7 8 9 10 %d %d 0 0"
	files := map[string]string{"/proc/7/status": "Name:\tangelia\nVmRSS:\t   20480 kB\n"}
	p := newProcessTarget("x", "127.0.0.1:1", "")
	p.readFile = func(name string) ([]byte, error) {
		if s, ok := files[name]; ok {
			return []byte(s), nil
		}
		return nil, os.ErrNotExist
	}
	t0 := time.Unix(1000, 0)
	files["/proc/7/stat"] = strings.Replace(strings.Replace(stat, "%d", "100", 1), "%d", "50", 1)
	s, err := p.sample(7, t0)
	if err != nil {
		t.Fatal(err)
	}
	if s.MemoryBytes != 20<<20 || s.CPUPercent != 0 {
		t.Errorf("first sample = %+v", s)
	}
	// 30 more ticks (0.3 s of CPU) in 2 s of wall time: 15%.
	files["/proc/7/stat"] = strings.Replace(strings.Replace(stat, "%d", "120", 1), "%d", "60", 1)
	if s, err = p.sample(7, t0.Add(2*time.Second)); err != nil || s.CPUPercent < 14.99 || s.CPUPercent > 15.01 {
		t.Errorf("second sample = %+v, %v; want 15%% CPU", s, err)
	}
	if _, err := p.sample(8, t0); err == nil || !strings.Contains(err.Error(), "/proc") {
		t.Errorf("missing process: err = %v", err)
	}
	if cpu, mem := p.Limits(); !strings.Contains(cpu, "none") || !strings.Contains(mem, "none") {
		t.Errorf("limits = %q %q", cpu, mem)
	}
}

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]int64{"12.5MiB": 13107200, "800KiB": 819200, "1GiB": 1 << 30, "1.5GB": 1.5e9, "512B": 512, "3kB": 3000} {
		if got, err := parseBytes(in); err != nil || got != want {
			t.Errorf("parseBytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"12", "xMiB"} {
		if _, err := parseBytes(in); err == nil {
			t.Errorf("parseBytes(%q): want error", in)
		}
	}
}

func TestDockerTarget(t *testing.T) {
	var calls [][]string
	d := &dockerTarget{
		name: "b", image: "img", bin: "/abs/angelia", workdir: "/w", memory: "256m", cpus: "1", logPath: filepath.Join(t.TempDir(), "log"),
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, append([]string{name}, args...))
			switch args[0] {
			case "stats":
				return []byte("37.5MiB / 256MiB|12.25%\n"), nil
			case "port":
				return []byte("127.0.0.1:49153\n[::1]:49153\n"), nil
			case "logs":
				return []byte("log line\n"), nil
			case "ps":
				return []byte("c1\nc2\n"), nil
			}
			return nil, nil
		},
	}
	ctx := context.Background()
	if err := d.Start(ctx, "/work/angelia.yaml", []string{"--extra"}); err != nil {
		t.Fatal(err)
	}
	run := strings.Join(calls[2], " ") // after the labelled cleanup: ps, rm
	for _, want := range []string{"--label angelia-bench=1", "--memory 256m", "--memory-swap 256m", "--cpus 1", "/abs/angelia:/angelia:ro", "img /angelia serve --config /work/angelia.yaml --extra"} {
		if !strings.Contains(run, want) {
			t.Errorf("docker run %q lacks %q", run, want)
		}
	}
	if u, _ := d.HealthURL(ctx); u != "http://127.0.0.1:49153" {
		t.Errorf("health URL = %q", u)
	}
	s, err := d.Sample(ctx)
	if err != nil || s.MemoryBytes != 37.5*(1<<20) || s.CPUPercent != 12.25 {
		t.Errorf("sample = %+v, %v", s, err)
	}
	if err := d.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if last := strings.Join(calls[len(calls)-1], " "); last != "docker rm -f c1 c2" {
		t.Errorf("labelled cleanup = %q", last)
	}
	if b, _ := os.ReadFile(d.logPath); string(b) != "log line\n" {
		t.Errorf("logs = %q", b)
	}
	if cpu, mem := d.Limits(); !strings.Contains(cpu, "1 CPU") || !strings.Contains(mem, "256m") {
		t.Errorf("limits = %q %q", cpu, mem)
	}
}

func TestEvaluate(t *testing.T) {
	samples := []Sample{{MemoryBytes: 30 << 20}, {MemoryBytes: 40 << 20, CPUPercent: 10}}
	reg := fakereg.Stats{Total: fakereg.EcoStats{Published: 10, Lag: fakereg.LagStats{Samples: 10, P95Seconds: 30}}}
	for name, tc := range map[string]struct {
		rss  int64
		lag  float64
		want string
	}{
		"pass":      {40 << 20, 30, "pass"},
		"rss fails": {200 << 20, 30, "fail"},
		"lag fails": {40 << 20, 300, "fail"},
	} {
		t.Run(name, func(t *testing.T) {
			s := append([]Sample(nil), samples...)
			s[1].MemoryBytes = tc.rss
			r := &Report{Samples: s, Registry: reg}
			r.Registry.Total.Lag.P95Seconds = tc.lag
			r.Resources = summarizeSamples(s)
			r.evaluate()
			if r.Verdict != tc.want {
				t.Errorf("verdict = %s, want %s (checks %+v)", r.Verdict, tc.want, r.Checks)
			}
			if len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "oci: not measured") {
				t.Errorf("notes = %v", r.Notes)
			}
		})
	}
	r := &Report{Samples: samples, Resources: summarizeSamples(samples)}
	r.evaluate()
	if r.Verdict != "incomplete" || len(r.Notes) != 2 {
		t.Errorf("no releases: verdict %s, notes %v", r.Verdict, r.Notes)
	}
}

func fakeProbe(files map[string]string, outputs map[string]string) probe {
	return probe{
		goos: "linux",
		readFile: func(name string) ([]byte, error) {
			if s, ok := files[name]; ok {
				return []byte(s), nil
			}
			return nil, os.ErrNotExist
		},
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if s, ok := outputs[name+" "+args[0]]; ok {
				return []byte(s), nil
			}
			return nil, errors.New("not found")
		},
	}
}

func TestEnvironment(t *testing.T) {
	pr := fakeProbe(map[string]string{
		"/proc/cpuinfo":              "processor\t: 0\nmodel name\t: Test CPU @ 3GHz\n",
		"/proc/meminfo":              "MemTotal:       16777216 kB\n",
		"/proc/sys/kernel/osrelease": "6.1.0\n",
	}, map[string]string{"git rev-parse": "abc123\n", "git status": " M x.go\n", "docker version": "27.0 (linux/amd64)\n", "docker info": "8 8589934592\n"})
	d := &dockerTarget{memory: "256m", cpus: "1"}
	e := pr.environment(context.Background(), "/does/not/exist", d, true)
	for got, want := range map[string]string{
		e.CPUModel: "Test CPU @ 3GHz", e.HostMemory: "16.0 GiB", e.Kernel: "6.1.0", e.Commit: "abc123",
		e.Mode: "container", e.Docker: "27.0 (linux/amd64)", e.DockerCPUs: "8", e.DockerMemory: "8.0 GiB", e.MemoryLimit: "256m (docker --memory, no swap)",
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if !e.CommitModified || e.HarnessGo == "" {
		t.Errorf("env = %+v", e)
	}
	// A Go binary carries its Go version: use the test binary itself.
	self, _ := os.Executable()
	if e := pr.environment(context.Background(), self, d, false); e.AngeliaGo == "" || e.Mode != "process" {
		t.Errorf("from buildinfo: %+v", e)
	}
}

// fakeTarget stands in for angelia: it serves /readyz and reports a fixed
// sample.
type fakeTarget struct {
	srv     *httptest.Server
	started []string
}

func (f *fakeTarget) Start(_ context.Context, cfg string, args []string) error {
	f.started = append(append(f.started, cfg), args...)
	return nil
}
func (f *fakeTarget) HealthURL(context.Context) (string, error) { return f.srv.URL, nil }
func (f *fakeTarget) Sample(context.Context) (Sample, error) {
	return Sample{At: time.Now(), MemoryBytes: 25 << 20, CPUPercent: 1}, nil
}
func (f *fakeTarget) Stop(context.Context) error { return nil }
func (f *fakeTarget) Limits() (string, string)   { return "1 CPU", "256m" }

// End to end with a fake angelia: the harness writes the watch file and the
// instance config, starts angelia with both, and writes both reports with
// the environment. The fake angelia checks nothing, so no release is seen
// and lag fails.
func TestRunWritesReport(t *testing.T) {
	ft := &fakeTarget{srv: httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))}
	defer ft.srv.Close()
	out := t.TempDir()
	cfgPath := filepath.Join(out, "fakereg.yaml")
	if err := os.WriteFile(cfgPath, []byte("defaults: {latency: 0s, latency_jitter: 0s}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := options{
		n: 50, duration: 30 * time.Millisecond, interval: 10 * time.Millisecond, fakeregConfig: cfgPath,
		bin: "bin/angelia", out: out, readyTimeout: 5 * time.Second, loader: watchFileLoader{interval: 5 * time.Minute},
		checkInterval: 5 * time.Millisecond, censor: 10 * time.Millisecond,
		newTarget: func(options, string) Target { return ft },
		probe:     fakeProbe(nil, map[string]string{"git rev-parse": "deadbeef"}),
	}
	rep, err := bench(context.Background(), o, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict == "pass" || len(rep.Samples) == 0 || rep.Params.PerEcosystem["crates"] != 12 || rep.Params.Loader != "watch-file" {
		t.Errorf("report = %+v", rep)
	}
	if want := []string{filepath.Join(out, "angelia.yaml"), "--watches", filepath.Join(out, "watches.yaml")}; strings.Join(ft.started, " ") != strings.Join(want, " ") {
		t.Errorf("started with %v, want %v", ft.started, want)
	}
	if b, err := os.ReadFile(filepath.Join(out, "angelia.yaml")); err != nil || !bytes.Contains(b, []byte("allow_networks:\n        - 127.0.0.1/32")) { //nolint:gosec // test temp dir
		t.Errorf("angelia.yaml: %v\n%s", err, b)
	}
	if _, err := os.Stat(filepath.Join(out, watchFileName)); err != nil {
		t.Error(err)
	}
	var back Report
	data, err := os.ReadFile(filepath.Join(out, "report.json")) //nolint:gosec // test temp dir
	if err != nil || json.Unmarshal(data, &back) != nil || back.Environment.Commit != "deadbeef" {
		t.Errorf("report.json: %v %s", err, data)
	}
	md, _ := os.ReadFile(filepath.Join(out, "report.md")) //nolint:gosec // test temp dir
	if w := rep.Registry.Window; !w.From.After(rep.Params.ReadyAt) || !w.To.After(w.From) {
		t.Errorf("lag window %+v, ready at %v", w, rep.Params.ReadyAt)
	}
	for _, want := range []string{
		"| Commit | deadbeef", "| Memory limit | 256m", "| Go (harness) |", "oci: not measured",
		"RSS (VmRSS) max", "Lag counts releases published from", "watch loader `watch-file`",
	} {
		if !bytes.Contains(md, []byte(want)) {
			t.Errorf("report.md lacks %q:\n%s", want, md)
		}
	}
}

func TestRunFlags(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"-n", "0"}, &bytes.Buffer{}, &stderr); code != 2 {
		t.Errorf("-n 0: exit %d", code)
	}
	if code := run(context.Background(), []string{"-duration", "2m"}, &bytes.Buffer{}, &stderr); code != 2 ||
		!strings.Contains(stderr.String(), "warm-up") {
		t.Errorf("-duration shorter than warm-up + censoring: exit %d, %s", code, stderr.String())
	}
	if code := run(context.Background(), []string{"-check-interval", "1m"}, &bytes.Buffer{}, &stderr); code != 2 ||
		!strings.Contains(stderr.String(), "docs/watch-file.md") {
		t.Errorf("-check-interval below the watch file minimum: exit %d, %s", code, stderr.String())
	}
	if code := run(context.Background(), []string{"-bogus"}, &bytes.Buffer{}, &stderr); code != 2 {
		t.Errorf("unknown flag: exit %d", code)
	}
	if code := run(context.Background(), []string{"-n", "5", "-container=false", "-fakereg-config", "missing.yaml", "-out", t.TempDir()}, &bytes.Buffer{}, &stderr); code != 2 {
		t.Errorf("missing config: exit %d", code)
	}
}

func TestBindHost(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	ctx := context.Background()
	runner := func(outputs map[string]string) runFunc {
		return fakeProbe(nil, outputs).run
	}
	if got := bindHost(ctx, runner(nil), "darwin", log); got != "127.0.0.1" {
		t.Errorf("Docker Desktop on macOS: %s", got)
	}
	if got := bindHost(ctx, runner(map[string]string{"docker info": "Docker Desktop\n"}), "linux", log); got != "127.0.0.1" {
		t.Errorf("Docker Desktop on Linux: %s", got)
	}
	if got := bindHost(ctx, runner(map[string]string{"docker network": "fd00::1 172.17.0.1 \n"}), "linux", log); got != "172.17.0.1" {
		t.Errorf("Linux bridge: %s", got)
	}
	if got := bindHost(ctx, runner(nil), "linux", log); got != "0.0.0.0" {
		t.Errorf("fallback: %s", got)
	}
}

func TestCheckMode(t *testing.T) {
	if checkMode(false, "darwin") == nil || checkMode(false, "linux") != nil || checkMode(true, "darwin") != nil {
		t.Error("process mode must need Linux, container mode must not")
	}
}
