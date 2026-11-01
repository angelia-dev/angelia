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
)

func TestGenerateWatches(t *testing.T) {
	w, per := GenerateWatches(7)
	if len(w) != 7 || per != 2 {
		t.Fatalf("got %d watches, %d per ecosystem; want 7 and 2", len(w), per)
	}
	kinds := map[string]int{}
	for _, x := range w {
		kinds[x.Kind]++
	}
	want := map[string]int{"pypi": 2, "npm": 2, "crates": 1, "github-release": 1, "oci": 1}
	for k, n := range want {
		if kinds[k] != n {
			t.Errorf("%s: %d watches, want %d (all: %v)", k, kinds[k], n, kinds)
		}
	}
	if w[3].Coordinates["repo"] != "bench/pkg-00000" || w[5].Coordinates["name"] != "pkg-00001" {
		t.Errorf("coordinates: %+v / %+v", w[3], w[5])
	}
	c := fakereg.DefaultConfig()
	SizeRegistry(c, per)
	if c.Ecosystems["oci"].Packages != 2 {
		t.Error("SizeRegistry did not apply")
	}
}

// The harness writes the existing instance configuration format; angelia's
// own loader must accept it.
func TestAngeliaConfigIsValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "angelia.yaml")
	if err := WriteAngeliaConfig(path, "127.0.0.1:9", "http://127.0.0.1:5000", dir); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(context.Background(), config.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Sources["github-release"].BaseURL; got != "http://127.0.0.1:5000/github" {
		t.Errorf("github-release base_url = %q", got)
	}
	if len(cfg.Sources) != 5 {
		t.Errorf("sources = %v", cfg.Sources)
	}
}

func TestMissingLoader(t *testing.T) {
	var l WatchLoader = missingLoader{}
	if _, err := l.Prepare(context.Background(), nil, "", ""); !errors.Is(err, ErrNoWatchLoader) || l.Name() != "none" {
		t.Errorf("err = %v", err)
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
	if s.RSSBytes != 20<<20 || s.CPUPercent != 0 {
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
			}
			return nil, nil
		},
	}
	ctx := context.Background()
	if err := d.Start(ctx, "/work/angelia.yaml", []string{"--extra"}); err != nil {
		t.Fatal(err)
	}
	run := strings.Join(calls[1], " ")
	for _, want := range []string{"--memory 256m", "--memory-swap 256m", "--cpus 1", "/abs/angelia:/angelia:ro", "img /angelia serve --config /work/angelia.yaml --extra"} {
		if !strings.Contains(run, want) {
			t.Errorf("docker run %q lacks %q", run, want)
		}
	}
	if u, _ := d.HealthURL(ctx); u != "http://127.0.0.1:49153" {
		t.Errorf("health URL = %q", u)
	}
	s, err := d.Sample(ctx)
	if err != nil || s.RSSBytes != 37.5*(1<<20) || s.CPUPercent != 12.25 {
		t.Errorf("sample = %+v, %v", s, err)
	}
	if err := d.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(d.logPath); string(b) != "log line\n" {
		t.Errorf("logs = %q", b)
	}
	if cpu, mem := d.Limits(); !strings.Contains(cpu, "1 CPU") || !strings.Contains(mem, "256m") {
		t.Errorf("limits = %q %q", cpu, mem)
	}
}

func TestEvaluate(t *testing.T) {
	samples := []Sample{{RSSBytes: 30 << 20}, {RSSBytes: 40 << 20, CPUPercent: 10}}
	reg := fakereg.Stats{Total: fakereg.EcoStats{Published: 10, Lag: fakereg.LagStats{Samples: 10, P95Seconds: 30}}}
	for name, tc := range map[string]struct {
		loaderErr error
		rss       int64
		lag       float64
		want      string
	}{
		"pass":       {nil, 40 << 20, 30, "pass"},
		"rss fails":  {nil, 200 << 20, 30, "fail"},
		"lag fails":  {nil, 40 << 20, 300, "fail"},
		"no watches": {ErrNoWatchLoader, 40 << 20, 30, "incomplete"},
	} {
		t.Run(name, func(t *testing.T) {
			s := append([]Sample(nil), samples...)
			s[1].RSSBytes = tc.rss
			r := &Report{Samples: s, Registry: reg}
			r.Registry.Total.Lag.P95Seconds = tc.lag
			r.Resources = summarizeSamples(s)
			r.evaluate(tc.loaderErr)
			if r.Verdict != tc.want {
				t.Errorf("verdict = %s, want %s (checks %+v)", r.Verdict, tc.want, r.Checks)
			}
		})
	}
	r := &Report{Samples: samples, Resources: summarizeSamples(samples)}
	r.evaluate(nil)
	if r.Verdict != "incomplete" || len(r.Notes) == 0 {
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
	}, map[string]string{"git rev-parse": "abc123\n", "git status": " M x.go\n", "docker version": "27.0 (linux/amd64)\n"})
	d := &dockerTarget{memory: "256m", cpus: "1"}
	e := pr.environment(context.Background(), "/does/not/exist", d, true)
	for got, want := range map[string]string{
		e.CPUModel: "Test CPU @ 3GHz", e.HostMemory: "16.0 GiB", e.Kernel: "6.1.0", e.Commit: "abc123",
		e.Mode: "container", e.Docker: "27.0 (linux/amd64)", e.MemoryLimit: "256m (docker --memory, no swap)",
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

func (f *fakeTarget) Start(_ context.Context, cfg string, _ []string) error {
	f.started = append(f.started, cfg)
	return nil
}
func (f *fakeTarget) HealthURL(context.Context) (string, error) { return f.srv.URL, nil }
func (f *fakeTarget) Sample(context.Context) (Sample, error) {
	return Sample{At: time.Now(), RSSBytes: 25 << 20, CPUPercent: 1}, nil
}
func (f *fakeTarget) Stop(context.Context) error { return nil }
func (f *fakeTarget) Limits() (string, string)   { return "1 CPU", "256m" }

// End to end with a fake angelia: the harness writes both reports, with
// the environment, and says the run is incomplete without a watch loader.
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
		bin: "bin/angelia", out: out, readyTimeout: 5 * time.Second, loader: missingLoader{},
		newTarget: func(options, string) Target { return ft },
		probe:     fakeProbe(nil, map[string]string{"git rev-parse": "deadbeef"}),
	}
	rep, err := bench(context.Background(), o, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != "incomplete" || len(rep.Samples) < 2 || rep.Params.PerEcosystem != 10 {
		t.Errorf("report = %+v", rep)
	}
	if len(ft.started) != 1 || ft.started[0] != filepath.Join(out, "angelia.yaml") {
		t.Errorf("started with %v", ft.started)
	}
	var back Report
	data, err := os.ReadFile(filepath.Join(out, "report.json")) //nolint:gosec // test temp dir
	if err != nil || json.Unmarshal(data, &back) != nil || back.Environment.Commit != "deadbeef" {
		t.Errorf("report.json: %v %s", err, data)
	}
	md, _ := os.ReadFile(filepath.Join(out, "report.md")) //nolint:gosec // test temp dir
	for _, want := range []string{"Verdict: incomplete", "| Commit | deadbeef", "| Memory limit | 256m", "| Go (harness) |", "no watch loader"} {
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
	if code := run(context.Background(), []string{"-bogus"}, &bytes.Buffer{}, &stderr); code != 2 {
		t.Errorf("unknown flag: exit %d", code)
	}
	if code := run(context.Background(), []string{"-n", "5", "-fakereg-config", "missing.yaml", "-out", t.TempDir()}, &bytes.Buffer{}, &stderr); code != 2 {
		t.Errorf("missing config: exit %d", code)
	}
}
