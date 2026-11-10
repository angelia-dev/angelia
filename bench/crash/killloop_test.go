package main

import (
	"context"
	"database/sql"
	"errors"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

// seeded returns a deterministic source for the kill times.
func seeded() *rand.Rand { return rand.New(rand.NewPCG(1, 2)) } //nolint:gosec // test timing

func sqlOpenRW(path string) (*sql.DB, error) { return sql.Open("sqlite", path) }

// The test binary doubles as a stand-in for angelia: with CRASH_HELPER set
// it serves /readyz on CRASH_HELPER_LISTEN and exits 0 on SIGTERM, or, with
// CRASH_HELPER=exit, exits 3 at once.
func TestMain(m *testing.M) {
	switch os.Getenv("CRASH_HELPER") {
	case "serve":
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM)
		mux := http.NewServeMux()
		mux.HandleFunc("/readyz", func(http.ResponseWriter, *http.Request) {})
		srv := &http.Server{Addr: os.Getenv("CRASH_HELPER_LISTEN"), Handler: mux, ReadHeaderTimeout: time.Second}
		go func() { _ = srv.ListenAndServe() }()
		<-term
		os.Exit(0)
	case "exit":
		os.Exit(3)
	}
	os.Exit(m.Run())
}

func helperLauncher(t *testing.T, mode string) launcher {
	t.Helper()
	listen, err := freeAddr()
	if err != nil {
		t.Fatal(err)
	}
	return launcher{
		bin: os.Args[0], cfg: "unused.yaml", listen: listen, log: filepath.Join(t.TempDir(), "angelia.log"),
		env: childEnv(os.Environ(), []string{"CRASH_HELPER=" + mode, "CRASH_HELPER_LISTEN=" + listen}),
	}
}

// fakeInstance records what the loop does to it.
type fakeInstance struct {
	killed  bool
	killErr error
}

func (f *fakeInstance) waitReady(context.Context, time.Duration) error { return nil }
func (f *fakeInstance) kill() error {
	f.killed = true
	return f.killErr
}

// fakeState is a database that reports a fixed backlog and one more
// check and two more events at every read.
func fakeState(pending int, reads *int) func(time.Time) (State, error) {
	return func(time.Time) (State, error) {
		*reads++
		return State{Backlog: Backlog{Pending: pending, InFlight: 3}, MaxSeq: int64(2 * *reads), Checks: 1}, nil
	}
}

// Acceptance criterion 1: the loop kills the requested number of
// processes, each after an uptime drawn in [MinUp, MaxUp) after /readyz or,
// for about FromStart of them, in [0, FromStartMax) after the start;
// it restarts after each kill and records the receiver, the backlog and
// detection at each kill.
func TestKillLoopKillsAndRestarts(t *testing.T) {
	var started []*fakeInstance
	var slept []time.Duration
	received, reads := 0, 0
	cfg := loopConfig{
		Kills: 100, MinUp: 500 * time.Millisecond, MaxUp: 4 * time.Second,
		FromStart: 0.1, FromStartMax: 300 * time.Millisecond, MinPending: 5,
		Received: func() int { received += 7; return received },
		Open:     func() int { return 2 },
		State:    fakeState(10, &reads),
		Start: func() (instance, error) {
			f := &fakeInstance{}
			started = append(started, f)
			return f, nil
		},
		Sleep: func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		},
		Now: time.Now,
	}
	first := &fakeInstance{}
	kills, err := killLoop(context.Background(), cfg, seeded(), first)
	if err != nil {
		t.Fatal(err)
	}
	if len(kills) != 100 || len(started) != 99 || reads != 101 || !first.killed {
		t.Fatalf("kills %d, starts %d, state reads %d, first killed %v", len(kills), len(started), reads, first.killed)
	}
	for i, f := range started {
		if !f.killed {
			t.Errorf("process %d not killed", i+2)
		}
	}
	early := 0
	for i, k := range kills {
		if k.FromStart {
			early++
			if k.Index == 1 || k.Uptime >= cfg.FromStartMax || k.Startup != 0 {
				t.Errorf("kill from the start %+v", k)
			}
		} else if k.Uptime < cfg.MinUp || k.Uptime >= cfg.MaxUp {
			t.Errorf("kill %+v", k)
		}
		if k.Index != i+1 || k.Received != 7 || k.Open != 2 || k.Backlog.Pending != 10 || k.Checks != 1 || k.Events != 2 {
			t.Fatalf("kill %+v", k)
		}
	}
	if early < 3 || early > 25 {
		t.Errorf("%d kills from the start, want about 10", early)
	}
	if slices.Min(slept) == slices.Max(slept) {
		t.Error("uptimes are not random")
	}
}

// A kill from the start is drawn only when the previous kill left enough
// work, so that it lands during dispatch too.
func TestKillLoopFromStartNeedsBacklog(t *testing.T) {
	reads := 0
	cfg := loopConfig{
		Kills: 50, MinUp: time.Millisecond, MaxUp: 2 * time.Millisecond,
		FromStart: 1, FromStartMax: time.Millisecond, MinPending: 500,
		Received: func() int { return 0 }, Open: func() int { return 0 },
		State: fakeState(10, &reads),
		Start: func() (instance, error) { return &fakeInstance{}, nil },
		Sleep: func(context.Context, time.Duration) error { return nil },
		Now:   time.Now,
	}
	kills, err := killLoop(context.Background(), cfg, seeded(), &fakeInstance{})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range kills {
		if k.FromStart {
			t.Fatalf("kill %d from the start with 10 pending", k.Index)
		}
	}
}

// The loop waits for work before each kill and stops when none comes.
func TestKillLoopWaitsForDispatch(t *testing.T) {
	waits, reads := 0, 0
	cfg := loopConfig{
		Kills: 5, MinUp: time.Millisecond, MaxUp: time.Millisecond,
		Received: func() int { return 0 }, Open: func() int { return 0 },
		State: fakeState(1, &reads),
		Start: func() (instance, error) { return &fakeInstance{}, nil },
		Sleep: func(context.Context, time.Duration) error { return nil },
		Now:   time.Now,
		WaitBusy: func(context.Context) error {
			if waits++; waits == 4 {
				return context.DeadlineExceeded
			}
			return nil
		},
	}
	kills, err := killLoop(context.Background(), cfg, seeded(), &fakeInstance{})
	if !errors.Is(err, context.DeadlineExceeded) || len(kills) != 3 {
		t.Fatalf("kills %d, err %v", len(kills), err)
	}
}

func TestKillLoopStopsWhenAKillFails(t *testing.T) {
	reads := 0
	cfg := loopConfig{
		Kills: 5, MinUp: time.Millisecond, MaxUp: time.Millisecond,
		Received: func() int { return 0 }, Open: func() int { return 0 },
		State: fakeState(0, &reads),
		Start: func() (instance, error) { return &fakeInstance{killErr: errExited}, nil },
		Sleep: func(context.Context, time.Duration) error { return nil },
		Now:   time.Now,
	}
	kills, err := killLoop(context.Background(), cfg, seeded(), &fakeInstance{})
	if !errors.Is(err, errExited) || len(kills) != 1 {
		t.Fatalf("kills %d, err %v", len(kills), err)
	}
}

// Acceptance criterion 1 with real processes: SIGKILL ends each one, from
// the start or after /readyz, and the loop restarts it.
func TestKillLoopRealProcesses(t *testing.T) {
	l := helperLauncher(t, "serve")
	ctx := context.Background()
	var procs []*proc
	start := func() (instance, error) {
		p, err := l.start()
		procs = append(procs, p)
		return p, err
	}
	first, err := l.start()
	if err != nil {
		t.Fatal(err)
	}
	if err := first.waitReady(ctx, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	reads := 0
	cfg := loopConfig{
		Kills: 6, MinUp: 10 * time.Millisecond, MaxUp: 50 * time.Millisecond, ReadyTimeout: 10 * time.Second,
		FromStart: 0.5, FromStartMax: 20 * time.Millisecond, MinPending: 1,
		Received: func() int { return 0 }, Open: func() int { return 0 },
		State: fakeState(1, &reads),
		Start: start, Sleep: sleepCtx, Now: time.Now,
	}
	kills, err := killLoop(ctx, cfg, seeded(), first)
	if err != nil {
		t.Fatal(err)
	}
	if len(kills) != 6 || !first.exited() {
		t.Fatalf("kills %d, first exited %v", len(kills), first.exited())
	}
	for _, p := range procs {
		if !p.exited() {
			t.Error("a process is still running after the loop")
		}
	}
	early := 0
	for _, k := range kills {
		if k.FromStart {
			early++
		} else if k.Index > 1 && k.Startup <= 0 {
			t.Errorf("kill %d: no startup time", k.Index)
		}
	}
	if early == 0 || early == 6 {
		t.Errorf("%d of 6 kills from the start with share 0.5 (seeded)", early)
	}
	// stop drains a process with SIGTERM.
	p, err := l.start()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.waitReady(ctx, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := p.stop(10 * time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestProcReportsAnEarlyExit(t *testing.T) {
	l := helperLauncher(t, "exit")
	p, err := l.start()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.waitReady(context.Background(), 10*time.Second); !errors.Is(err, errExited) {
		t.Fatalf("waitReady: %v", err)
	}
	if err := p.kill(); !errors.Is(err, errExited) {
		t.Fatalf("kill: %v", err)
	}
	if err := p.stop(time.Second); !errors.Is(err, errExited) {
		t.Fatalf("stop: %v", err)
	}
}

func TestChildEnvDropsProxies(t *testing.T) {
	got := childEnv([]string{"PATH=/bin", "HTTPS_PROXY=http://p:1", "https_proxy=x", "NO_PROXY=y", "HOME=/h"}, []string{"A=1"})
	if !slices.Equal(got, []string{"PATH=/bin", "HOME=/h", "A=1"}) {
		t.Errorf("childEnv = %v", got)
	}
}
