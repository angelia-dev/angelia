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

// Acceptance criterion 1: the loop kills the requested number of
// processes, each after an uptime drawn in [MinUp, MaxUp), restarting
// after each kill, and records the receiver and the backlog at each kill.
func TestKillLoopKillsAndRestarts(t *testing.T) {
	var started []*fakeInstance
	var slept []time.Duration
	received, backlogReads := 0, 0
	cfg := loopConfig{
		Kills: 100, MinUp: 500 * time.Millisecond, MaxUp: 4 * time.Second,
		Received: func() int { received += 7; return received },
		Open:     func() int { return 2 },
		Backlog: func() (Backlog, error) {
			backlogReads++
			return Backlog{Pending: 10, InFlight: 3}, nil
		},
		Start: func() (instance, error) {
			f := &fakeInstance{}
			started = append(started, f)
			return f, nil
		},
		Sleep: func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		},
	}
	first := &fakeInstance{}
	kills, last, err := killLoop(context.Background(), cfg, seeded(), first)
	if err != nil {
		t.Fatal(err)
	}
	if len(kills) != 100 || len(started) != 100 || backlogReads != 100 || !first.killed {
		t.Fatalf("kills %d, starts %d, backlog reads %d, first killed %v", len(kills), len(started), backlogReads, first.killed)
	}
	for i, f := range started[:99] {
		if !f.killed {
			t.Errorf("process %d not killed", i+1)
		}
	}
	if last != started[99] || started[99].killed {
		t.Error("the process started after the last kill should be returned alive")
	}
	for i, k := range kills {
		if k.Index != i+1 || k.Uptime < cfg.MinUp || k.Uptime >= cfg.MaxUp || k.Uptime != slept[i] ||
			k.Received != 7 || k.Open != 2 || k.Backlog.Pending != 10 {
			t.Fatalf("kill %+v", k)
		}
	}
	if slices.Min(slept) == slices.Max(slept) {
		t.Error("uptimes are not random")
	}
}

// The loop waits for work before each kill and stops when none comes.
func TestKillLoopWaitsForDispatch(t *testing.T) {
	waits := 0
	cfg := loopConfig{
		Kills: 5, MinUp: time.Millisecond, MaxUp: time.Millisecond,
		Received: func() int { return 0 }, Open: func() int { return 0 },
		Backlog: func() (Backlog, error) { return Backlog{Pending: 1}, nil },
		Start:   func() (instance, error) { return &fakeInstance{}, nil },
		Sleep:   func(context.Context, time.Duration) error { return nil },
		WaitBusy: func(context.Context) error {
			if waits++; waits == 4 {
				return context.DeadlineExceeded
			}
			return nil
		},
	}
	kills, _, err := killLoop(context.Background(), cfg, seeded(), &fakeInstance{})
	if !errors.Is(err, context.DeadlineExceeded) || len(kills) != 3 {
		t.Fatalf("kills %d, err %v", len(kills), err)
	}
}

func TestKillLoopStopsWhenAKillFails(t *testing.T) {
	cfg := loopConfig{
		Kills: 5, MinUp: time.Millisecond, MaxUp: time.Millisecond,
		Received: func() int { return 0 }, Open: func() int { return 0 },
		Backlog: func() (Backlog, error) { return Backlog{}, nil },
		Start:   func() (instance, error) { return &fakeInstance{killErr: errExited}, nil },
		Sleep:   func(context.Context, time.Duration) error { return nil },
	}
	kills, _, err := killLoop(context.Background(), cfg, seeded(), &fakeInstance{})
	if !errors.Is(err, errExited) || len(kills) != 1 {
		t.Fatalf("kills %d, err %v", len(kills), err)
	}
}

// Acceptance criterion 1 with real processes: SIGKILL ends each one, the
// loop restarts it, and stop drains the last one with SIGTERM.
func TestKillLoopRealProcesses(t *testing.T) {
	l := helperLauncher(t, "serve")
	ctx := context.Background()
	start := func() (instance, error) { return l.start() }
	first, err := l.start()
	if err != nil {
		t.Fatal(err)
	}
	if err := first.waitReady(ctx, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	cfg := loopConfig{
		Kills: 3, MinUp: 10 * time.Millisecond, MaxUp: 50 * time.Millisecond, ReadyTimeout: 10 * time.Second,
		Received: func() int { return 0 }, Open: func() int { return 0 },
		Backlog: func() (Backlog, error) { return Backlog{}, nil },
		Start:   start, Sleep: sleepCtx,
	}
	kills, last, err := killLoop(ctx, cfg, seeded(), first)
	if err != nil {
		t.Fatal(err)
	}
	if len(kills) != 3 || !first.exited() {
		t.Fatalf("kills %d, first exited %v", len(kills), first.exited())
	}
	for _, k := range kills[1:] {
		if k.Startup <= 0 {
			t.Errorf("kill %d: no startup time", k.Index)
		}
	}
	if err := last.(*proc).stop(10 * time.Second); err != nil {
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
