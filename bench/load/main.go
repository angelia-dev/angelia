// Command load is the Phase 0 scale benchmark (architecture.md section 6,
// benchmark 1): it starts the fake registry, generates N watches over five
// ecosystems, runs `angelia serve` (in a CPU and memory limited container,
// or as a plain process with -container=false), samples memory and CPU,
// reads check lag from the fake registry and writes report.json and
// report.md.
//
//	make bench-scale BENCH_N=10000                  # container, needs Docker
//	BENCH_CONTAINER=0 make bench-scale BENCH_N=100  # plain process, Linux only
//
// Exit status: 0 pass, 1 fail, 2 error, 3 incomplete (a target could not be
// measured, for example because angelia cannot be given watches yet).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/angelia-dev/angelia/bench/fakereg"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

type options struct {
	n                       int
	duration, interval      time.Duration
	fakeregConfig, bin, out string
	container               bool
	memory, cpus, image     string
	readyTimeout            time.Duration
	// checkInterval is the per-watch check interval the loader asks for;
	// lag counts only releases published one interval (plus jitter) after
	// readiness. censor drops the releases published in the last censor of
	// the run, so every unseen release counts with at least that lag.
	checkInterval, censor time.Duration
	loader                WatchLoader
	newTarget             func(o options, workdir string) Target // tests replace it
	probe                 probe
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("load", flag.ContinueOnError)
	fs.SetOutput(stderr)
	o := options{loader: missingLoader{}, newTarget: newTarget, probe: probe{readFile: os.ReadFile, run: runCmd, goos: runtime.GOOS}}
	fs.IntVar(&o.n, "n", 10000, "number of watches, spread over 5 ecosystems")
	fs.DurationVar(&o.duration, "duration", 15*time.Minute, "how long to measure after angelia is ready")
	fs.DurationVar(&o.interval, "sample-interval", 5*time.Second, "time between memory/CPU samples")
	fs.StringVar(&o.fakeregConfig, "fakereg-config", "bench/fakereg/fakereg.yaml", "fake registry behaviour YAML")
	fs.StringVar(&o.bin, "angelia", "bin/angelia", "angelia binary (linux build for -container)")
	fs.StringVar(&o.out, "out", "bin/bench/scale", "output directory for the report and logs")
	fs.BoolVar(&o.container, "container", true, "run angelia in a limited Docker container")
	fs.StringVar(&o.memory, "memory", "256m", "container memory limit")
	fs.StringVar(&o.cpus, "cpus", "1", "container CPU limit")
	fs.StringVar(&o.image, "image", "gcr.io/distroless/static-debian12:nonroot", "base image the binary is mounted into")
	fs.DurationVar(&o.readyTimeout, "ready-timeout", time.Minute, "how long to wait for /readyz")
	fs.DurationVar(&o.checkInterval, "check-interval", time.Minute, "per-watch check interval; the warm-up is 1.1 times this")
	o.censor = targetLagP95
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if o.n < 1 || o.duration <= 0 || o.interval <= 0 || o.checkInterval <= 0 || fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "load: -n, -duration, -sample-interval and -check-interval must be positive; no arguments")
		return 2
	}
	if least := warmup(o.checkInterval) + o.censor; o.duration <= least {
		_, _ = fmt.Fprintf(stderr, "load: -duration must exceed the warm-up plus %v of censoring: more than %v\n", o.censor, least)
		return 2
	}
	if err := checkMode(o.container, runtime.GOOS); err != nil {
		_, _ = fmt.Fprintln(stderr, "load:", err)
		return 2
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	rep, err := bench(ctx, o, log)
	if err != nil {
		log.Error("benchmark failed", "err", err)
		return 2
	}
	_, _ = fmt.Fprintf(stdout, "verdict: %s; report in %s\n", rep.Verdict, filepath.Join(o.out, "report.md"))
	return map[string]int{"pass": 0, "fail": 1, "incomplete": 3}[rep.Verdict]
}

func newTarget(o options, workdir string) Target {
	if o.container {
		return &dockerTarget{
			run: runCmd, name: fmt.Sprintf("%s-%d", benchLabel, os.Getpid()), image: o.image, bin: o.bin,
			workdir: workdir, memory: o.memory, cpus: o.cpus, logPath: filepath.Join(workdir, "angelia.log"),
		}
	}
	return newProcessTarget(o.bin, freeAddr(), filepath.Join(workdir, "angelia.log"))
}

func bench(ctx context.Context, o options, log *slog.Logger) (*Report, error) {
	var err error
	if o.bin, err = filepath.Abs(o.bin); err != nil {
		return nil, err
	}
	workdir, err := filepath.Abs(o.out)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(workdir, 0o750); err != nil {
		return nil, err
	}
	cfg, err := fakereg.LoadConfig(o.fakeregConfig)
	if err != nil {
		return nil, err
	}
	watches, perEco := GenerateWatches(o.n)
	SizeRegistry(cfg, perEco)

	host := "127.0.0.1"
	if o.container {
		host = bindHost(ctx, o.probe.run, o.probe.goos, log)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return nil, err
	}
	reg := fakereg.New(cfg)
	srv := &http.Server{Handler: reg, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port
	registry, listen, angeliaDir := fmt.Sprintf("http://127.0.0.1:%d", port), "", workdir
	if o.container {
		registry, listen, angeliaDir = fmt.Sprintf("http://host.docker.internal:%d", port), ":8080", "/work"
	}

	t := o.newTarget(o, workdir)
	if pt, ok := t.(*processTarget); ok {
		listen = pt.listen
	}
	serveArgs, loaderErr := o.loader.Prepare(ctx, watches, workdir, angeliaDir)
	if loaderErr != nil {
		if !errors.Is(loaderErr, ErrNoWatchLoader) {
			return nil, loaderErr
		}
		log.Warn("running without watches", "reason", loaderErr)
	}
	if err := WriteAngeliaConfig(filepath.Join(workdir, "angelia.yaml"), listen, registry, angeliaDir); err != nil {
		return nil, err
	}

	rep := &Report{
		Benchmark: "Phase 0 benchmark 1: scale",
		Params: Params{
			Watches: o.n, PerEcosystem: perEco, Loader: o.loader.Name(), Duration: o.duration.String(),
			SampleInterval: o.interval.String(), FakeregConfig: o.fakeregConfig, StartedAt: time.Now().UTC(),
		},
	}
	rep.Environment = o.probe.environment(ctx, o.bin, t, o.container)
	log.Info("starting angelia", "mode", rep.Environment.Mode, "watches", o.n, "duration", o.duration)
	if err := t.Start(ctx, filepath.Join(angeliaDir, "angelia.yaml"), serveArgs); err != nil {
		return nil, err
	}
	defer func() { _ = t.Stop(context.WithoutCancel(ctx)) }()
	if err := waitReady(ctx, t, o.readyTimeout); err != nil {
		return nil, err
	}
	readyAt := time.Now()
	end := readyAt.Add(o.duration)
	window := fakereg.Window{From: readyAt.Add(warmup(o.checkInterval)), To: end.Add(-o.censor)}
	rep.Params.ReadyAt, rep.Params.CheckInterval = readyAt.UTC(), o.checkInterval.String()

	tick := time.NewTicker(o.interval)
	defer tick.Stop()
	for {
		s, err := t.Sample(ctx)
		if err != nil {
			return nil, fmt.Errorf("sample: %w", err)
		}
		rep.Samples = append(rep.Samples, s)
		if !time.Now().Before(end) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
	rep.Registry = reg.StatsIn(window)
	rep.Resources = summarizeSamples(rep.Samples)
	rep.Resources.MemorySource = "RSS (VmRSS)"
	if o.container {
		rep.Resources.MemorySource = "container memory (docker stats)"
	}
	rep.evaluate(loaderErr)
	return rep, rep.Write(workdir)
}

func waitReady(ctx context.Context, t Target, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		base, err := t.HealthURL(ctx)
		if err == nil {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/readyz", nil)
			if resp, err := http.DefaultClient.Do(req); err == nil { //nolint:gosec // loopback URL of the instance under test
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("angelia not ready after %v (see angelia.log): %w", timeout, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// checkMode fails fast where process mode cannot sample.
func checkMode(container bool, goos string) error {
	if !container && goos != "linux" {
		return errors.New("process mode (BENCH_CONTAINER=0) reads /proc and needs Linux; use the container mode")
	}
	return nil
}

// warmup is how long after readiness every watch has been checked once:
// one check interval plus the scheduler's 10% jitter.
func warmup(interval time.Duration) time.Duration { return interval + interval/10 }

// bindHost picks the address the fake registry listens on in container
// mode, where angelia reaches it as host.docker.internal. Docker Desktop
// forwards that name to the host's loopback; on Linux it is the bridge
// gateway (--add-host host-gateway). All interfaces only as a last resort.
func bindHost(ctx context.Context, run runFunc, goos string, log *slog.Logger) string {
	if goos != "linux" {
		return "127.0.0.1"
	}
	if info, err := run(ctx, "docker", "info", "--format", "{{.OperatingSystem}}"); err == nil && strings.Contains(string(info), "Docker Desktop") {
		return "127.0.0.1"
	}
	out, err := run(ctx, "docker", "network", "inspect", "bridge", "--format", "{{range .IPAM.Config}}{{.Gateway}} {{end}}")
	if err == nil {
		for _, f := range strings.Fields(string(out)) {
			if ip := net.ParseIP(f); ip != nil && ip.To4() != nil {
				return f
			}
		}
	}
	log.Warn("cannot find the docker bridge gateway; the fake registry listens on all interfaces for this run", "err", err)
	return "0.0.0.0"
}

// freeAddr returns a loopback address with a port that was free a moment ago.
func freeAddr() string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "127.0.0.1:18080"
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}
