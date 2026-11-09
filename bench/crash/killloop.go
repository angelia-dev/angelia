package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"
)

// instance is a running angelia process, as the kill loop sees it.
type instance interface {
	waitReady(ctx context.Context, timeout time.Duration) error
	kill() error
}

// Kill is one SIGKILL of the loop.
type Kill struct {
	Index int `json:"index"`
	// Startup is the time from the start of the process to /readyz.
	Startup time.Duration `json:"startup_ns"`
	// Uptime is the time to the kill, drawn uniformly, from /readyz or,
	// if no delivery was pending then, from the moment one was.
	Uptime time.Duration `json:"uptime_ns"`
	// Received is the number of requests the receiver got from the start
	// of the uptime to the kill.
	Received int `json:"received"`
	// Open is the number of requests the receiver was answering at the
	// kill: deliveries cut off in the middle of the sink call.
	Open int `json:"open"`
	// Backlog is read from the database right after the kill: the
	// deliveries the dead process left pending or in flight.
	Backlog Backlog `json:"backlog"`
}

// loopConfig shapes the kill loop.
type loopConfig struct {
	Kills          int
	MinUp, MaxUp   time.Duration // the uptime after /readyz is uniform in [MinUp, MaxUp)
	ReadyTimeout   time.Duration
	Received, Open func() int               // the receiver's counters
	Backlog        func() (Backlog, error)  // read after each kill
	Start          func() (instance, error) // launches the next process
	Progress       func(k Kill)             // called after each kill; may be nil
	// WaitBusy, if set, returns once the dispatcher has work, before each
	// uptime starts.
	WaitBusy func(context.Context) error
	Sleep    func(context.Context, time.Duration) error
}

// killLoop kills cur after a random uptime, starts a new process, and so on
// until it has killed cfg.Kills processes. cur must be ready. The process
// started after the last kill is returned, ready, for the drain.
func killLoop(ctx context.Context, cfg loopConfig, rng *rand.Rand, cur instance) ([]Kill, instance, error) {
	kills := make([]Kill, 0, cfg.Kills)
	var startup time.Duration
	for i := range cfg.Kills {
		k := Kill{Index: i + 1, Startup: startup, Uptime: cfg.MinUp}
		if span := cfg.MaxUp - cfg.MinUp; span > 0 {
			k.Uptime += time.Duration(rng.Int64N(int64(span)))
		}
		if cfg.WaitBusy != nil {
			if err := cfg.WaitBusy(ctx); err != nil {
				return kills, cur, fmt.Errorf("kill %d: %w", k.Index, err)
			}
		}
		before := cfg.Received()
		if err := cfg.Sleep(ctx, k.Uptime); err != nil {
			return kills, cur, err
		}
		k.Open = cfg.Open()
		if err := cur.kill(); err != nil {
			return kills, nil, fmt.Errorf("kill %d: %w", k.Index, err)
		}
		k.Received = cfg.Received() - before
		b, err := cfg.Backlog()
		if err != nil {
			return kills, nil, fmt.Errorf("kill %d: read backlog: %w", k.Index, err)
		}
		k.Backlog = b
		kills = append(kills, k)
		if cfg.Progress != nil {
			cfg.Progress(k)
		}
		t0 := time.Now()
		if cur, err = cfg.Start(); err != nil {
			return kills, nil, fmt.Errorf("restart after kill %d: %w", k.Index, err)
		}
		if err := cur.waitReady(ctx, cfg.ReadyTimeout); err != nil {
			return kills, cur, fmt.Errorf("restart after kill %d: %w", k.Index, err)
		}
		startup = time.Since(t0)
	}
	return kills, cur, nil
}

// sleepCtx waits for d or the end of ctx.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
