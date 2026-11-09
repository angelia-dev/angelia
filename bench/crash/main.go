// Command crash is Phase 0 benchmark 5 (architecture.md section 6, story
// angelia-dev/angelia#109): it kills angelia with SIGKILL at random points
// during heavy dispatch to a recording webhook receiver, restarts it each
// time, and then checks that every delivery arrived at least once and that
// duplicates share their idempotency key. See README.md.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
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
	bin, out, receiverHost, envNote string
	w                               workload
	kills                           int
	minUp, maxUp, receiverDelay     time.Duration
	warmTimeout, drainTimeout       time.Duration
	busyTimeout                     time.Duration
	minPending                      int
	minFlow                         int
	seed                            uint64
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("crash", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	fs.StringVar(&o.bin, "angelia", "bin/angelia", "angelia binary (Linux)")
	fs.StringVar(&o.out, "out", "bin/bench/crash", "output directory: report, logs, configuration and database")
	fs.IntVar(&o.kills, "kills", 100, "number of SIGKILLs")
	fs.DurationVar(&o.minUp, "min-uptime", 200*time.Millisecond, "least time from /readyz to the kill")
	fs.DurationVar(&o.maxUp, "max-uptime", 3*time.Second, "the uptime is uniform in [min-uptime, max-uptime)")
	fs.IntVar(&o.w.Packages, "n", 300, "pypi watches against the fake registry")
	fs.IntVar(&o.w.Subscriptions, "subscriptions", 5, "webhook sinks, one subscription each; every event is delivered to each")
	fs.Float64Var(&o.w.ReleasesPerH, "releases-per-hour", 28800, "new releases per hour over all packages (with -n 300 and 5 subscriptions, 12,000 deliveries per 5-minute check cycle)")
	fs.IntVar(&o.w.History, "history", 3, "versions each package has at start (recorded as the baseline, without events)")
	fs.DurationVar(&o.receiverDelay, "receiver-delay", 250*time.Millisecond, "the receiver answers after a delay uniform in [0, this)")
	fs.StringVar(&o.receiverHost, "receiver-host", "127.0.0.2", "loopback address of the receiver; another host than the registry's 127.0.0.1, so each has its own rate limit")
	fs.IntVar(&o.minFlow, "min-flow", 200, "requests the receiver must have got before the first kill")
	fs.IntVar(&o.minPending, "min-pending", 500, "before each kill, wait until this many deliveries are pending; the receiver's host limit (100 rps, burst 200) cannot send 500 within -max-uptime (3s), so the kill lands while deliveries wait")
	fs.DurationVar(&o.busyTimeout, "busy-timeout", 6*time.Minute, "before each kill, how long to wait for a pending delivery (the next check cycle) before giving up")
	fs.DurationVar(&o.warmTimeout, "warmup-timeout", 12*time.Minute, "how long to wait for min-flow (the first events come one check interval, 5m, after the baseline)")
	fs.DurationVar(&o.drainTimeout, "drain-timeout", 10*time.Minute, "how long the final dispatcher-only run may take to finish every delivery")
	fs.Uint64Var(&o.seed, "seed", 0, "seed of the kill times; 0 picks one (the report records it)")
	fs.StringVar(&o.envNote, "env-note", "", "description of the machine, for the report")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if o.kills < 1 || o.minPending < 1 || o.minUp <= 0 || o.maxUp < o.minUp || o.w.Packages < 1 || o.w.Subscriptions < 1 ||
		o.w.ReleasesPerH <= 0 || o.w.History < 1 || o.receiverDelay < 0 || fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "crash: -kills, -n, -subscriptions, -releases-per-hour, -history and the uptimes must be positive, -max-uptime >= -min-uptime; no arguments")
		return 2
	}
	if runtime.GOOS != "linux" {
		_, _ = fmt.Fprintln(stderr, "crash: needs Linux (angelia runs as a child process on loopback addresses)")
		return 2
	}
	if o.seed == 0 {
		o.seed = mrand.Uint64() //nolint:gosec // kill timing, recorded for a rerun
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	rep, err := bench(ctx, o, log)
	if rep != nil {
		if werr := rep.Write(o.out); werr != nil {
			err = errors.Join(err, werr)
		} else {
			_, _ = fmt.Fprintf(stdout, "verdict: %s; report in %s\n", rep.Verdict, filepath.Join(o.out, "report.md"))
		}
	}
	if err != nil {
		log.Error("benchmark failed", "err", err)
		if rep == nil {
			return 2
		}
	}
	return map[string]int{"pass": 0, "fail": 1, "incomplete": 3}[rep.Verdict]
}

// newSecret returns a Standard Webhooks secret: whsec_ and 32 random bytes.
func newSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "whsec_" + base64.StdEncoding.EncodeToString(b), nil
}

// serve serves h on host:0 until ctx ends and returns "http://host:port".
func serve(ctx context.Context, host string, h http.Handler) (string, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return "", fmt.Errorf("listen on %s: %w", host, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	return "http://" + ln.Addr().String(), nil
}

// waitPending waits until at least n deliveries are pending, so that every
// kill lands during dispatch: on a machine that dispatches faster than the
// check cycles produce, the loop pauses until the next cycle.
func waitPending(ctx context.Context, dbPath string, n int, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		b, err := readBacklog(ctx, dbPath)
		if err == nil && b.Pending >= n {
			return nil
		}
		if err := sleepCtx(ctx, 200*time.Millisecond); err != nil {
			return fmt.Errorf("fewer than %d deliveries pending for %v: %w", n, timeout, err)
		}
	}
}

// freeAddr returns a loopback address whose port was free a moment ago.
func freeAddr() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String(), nil
}

// bench runs the benchmark. A non-nil report is written even with an
// error: its verdict is then incomplete, or fail if checks ran and failed.
func bench(ctx context.Context, o options, log *slog.Logger) (*Report, error) {
	t0 := time.Now()
	var err error
	if o.bin, err = filepath.Abs(o.bin); err != nil {
		return nil, err
	}
	if o.out, err = filepath.Abs(o.out); err != nil {
		return nil, err
	}
	dataDir := filepath.Join(o.out, "data")
	if err := os.RemoveAll(dataDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, err
	}
	secret, err := newSecret()
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx)) // servers outlive ctx until the report
	defer cancel()
	registry, err := serve(sctx, "127.0.0.1", fakereg.New(registryConfig(o.w)))
	if err != nil {
		return nil, err
	}
	recv := NewReceiver(secret, o.receiverDelay)
	receiver, err := serve(sctx, o.receiverHost, recv)
	if err != nil {
		return nil, err
	}
	listen, err := freeAddr()
	if err != nil {
		return nil, err
	}
	cfgPath, watches := filepath.Join(o.out, "angelia.yaml"), filepath.Join(o.out, "watches.yaml")
	if err := writeInstanceConfig(cfgPath, listen, dataDir, registry, receiver); err != nil {
		return nil, err
	}
	if err := writeWatchFile(watches, receiver, o.w); err != nil {
		return nil, err
	}
	l := launcher{
		bin: o.bin, cfg: cfgPath, listen: listen, log: filepath.Join(o.out, "angelia.log"),
		env: childEnv(os.Environ(), []string{secretEnv + "=" + secret}),
	}
	if err := os.Remove(l.log); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	dbPath := filepath.Join(dataDir, "angelia.db")

	rep := &Report{
		Benchmark: "kill-9", Started: t0, Environment: environment(ctx, o.bin, o.envNote),
		Params: Params{
			Workload: o.w, Kills: o.kills, MinUp: o.minUp, MaxUp: o.maxUp,
			Uptime: fmt.Sprintf("uniform in [%s, %s)", o.minUp, o.maxUp), ReceiverWait: o.receiverDelay, Seed: o.seed, MinFlow: o.minFlow,
		},
	}
	finish := func(err error, notes ...string) (*Report, error) {
		rep.Notes = append(rep.Notes, notes...)
		rep.Duration = time.Since(t0)
		if rep.Verdict == "" {
			rep.decide(nil, setupChecks(o.kills, rep.Kills, rep.Tally))
		}
		return rep, err
	}

	// Warm-up: the baseline check of every watch, then the first events
	// one interval later. No kill until deliveries flow.
	log.Info("starting angelia", "watches", o.w.Packages, "subscriptions", o.w.Subscriptions, "out", o.out)
	start := func() (*proc, error) { return l.start("--watches", watches) }
	p, err := start()
	if err != nil {
		return finish(err)
	}
	if err := p.waitReady(ctx, 2*time.Minute); err != nil {
		return finish(err)
	}
	wctx, wcancel := context.WithTimeout(ctx, o.warmTimeout)
	for recv.Count() < o.minFlow && wctx.Err() == nil && !p.exited() {
		_ = sleepCtx(wctx, time.Second)
		if s := int(time.Since(t0).Seconds()); s%30 == 0 {
			log.Info("waiting for dispatch to start", "received", recv.Count(), "want", o.minFlow)
		}
	}
	wcancel()
	rep.Phases.Warmup = time.Since(t0)
	if rep.Phases.ReceivedBeforeKills = recv.Count(); rep.Phases.ReceivedBeforeKills < o.minFlow {
		_ = p.kill()
		return finish(fmt.Errorf("only %d deliveries received after %v (exited: %v)", recv.Count(), o.warmTimeout, p.exited()))
	}

	// The kill loop.
	log.Info("dispatch is flowing; starting the kill loop", "received", recv.Count(), "kills", o.kills, "seed", o.seed)
	t1 := time.Now()
	cfg := loopConfig{
		Kills: o.kills, MinUp: o.minUp, MaxUp: o.maxUp, ReadyTimeout: 2 * time.Minute,
		Received: recv.Count, Open: recv.Open, Sleep: sleepCtx,
		WaitBusy: func(ctx context.Context) error { return waitPending(ctx, dbPath, o.minPending, o.busyTimeout) },
		Backlog:  func() (Backlog, error) { return readBacklog(ctx, dbPath) },
		Start:    func() (instance, error) { return start() },
		Progress: func(k Kill) {
			if k.Index%10 == 0 {
				log.Info("killed", "kills", k.Index, "received", recv.Count(), "pending", k.Backlog.Pending, "in_flight", k.Backlog.InFlight)
			}
		},
	}
	kills, last, err := killLoop(ctx, cfg, mrand.New(mrand.NewPCG(o.seed, 0x6b696c6c)), p) //nolint:gosec // kill timing
	rep.Kills, rep.Phases.KillLoop = kills, time.Since(t1)
	if err != nil {
		if lp, ok := last.(*proc); ok && lp != nil && !lp.exited() {
			_ = lp.kill()
		}
		return finish(err)
	}
	if err := last.(*proc).stop(40 * time.Second); err != nil {
		return finish(fmt.Errorf("stop the last process of the kill loop: %w", err))
	}

	// Drain: the dispatcher alone (no new events) until no delivery is
	// pending or in flight. In-flight rows of the last kills wait for their
	// lease (2 minutes) to expire.
	log.Info("draining with the dispatcher only", "received", recv.Count())
	t2 := time.Now()
	d, err := l.start("--roles=api,dispatcher")
	if err != nil {
		return finish(err)
	}
	if err := d.waitReady(ctx, 2*time.Minute); err != nil {
		return finish(err)
	}
	drained, last2 := false, Backlog{}
	dctx, dcancel := context.WithTimeout(ctx, o.drainTimeout)
	for dctx.Err() == nil && !d.exited() {
		if last2, err = readBacklog(dctx, dbPath); err == nil && last2.Pending+last2.InFlight == 0 {
			drained = true
			break
		}
		_ = sleepCtx(dctx, 2*time.Second)
	}
	dcancel()
	rep.Phases.Drain = time.Since(t2)
	if err := d.stop(40 * time.Second); err != nil {
		return finish(fmt.Errorf("stop the drain process: %w", err))
	}
	var notes []string
	if !drained {
		notes = append(notes, fmt.Sprintf("The drain did not finish within %v: %d deliveries pending and %d in flight at its end.", o.drainTimeout, last2.Pending, last2.InFlight))
	}

	snap, err := readSnapshot(ctx, dbPath)
	if err != nil {
		return finish(err, notes...)
	}
	verify, tally := Verify(snap, recv.Receipts())
	rep.Tally = tally
	rep.decide(verify, setupChecks(o.kills, kills, tally))
	log.Info("done", "verdict", rep.Verdict, "events", tally.Events, "deliveries", tally.Deliveries, "received", tally.Received, "duplicates", tally.Duplicates)
	return finish(nil, notes...)
}
