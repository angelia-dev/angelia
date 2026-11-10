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
	// FromStart is true for a kill timed from the start of the process,
	// without waiting for /readyz: it lands in the startup (migrations, the
	// watch file, lease recovery).
	FromStart bool `json:"from_start"`
	// Startup is the time from the start of the process to /readyz; zero
	// for a kill from the start and for the first process.
	Startup time.Duration `json:"startup_ns"`
	// Uptime is the time to the kill, drawn uniformly: from the start of the
	// process for a kill from the start, otherwise from /readyz or, if fewer
	// than the wanted deliveries were pending then, from the moment they were.
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
	// Checks is the number of targets whose check was committed since the
	// previous kill, and Events the number of events committed since then:
	// detection was running in this process.
	Checks int `json:"checks"`
	Events int `json:"events"`
}

// loopConfig shapes the kill loop.
type loopConfig struct {
	Kills        int
	MinUp, MaxUp time.Duration // the uptime after /readyz is uniform in [MinUp, MaxUp)
	// FromStart is the share of kills timed from the start of the process,
	// uniformly in [0, FromStartMax). A kill is timed so only when the
	// previous kill left at least MinPending deliveries pending, so that it
	// too lands during dispatch.
	FromStart    float64
	FromStartMax time.Duration
	MinPending   int
	ReadyTimeout time.Duration
	// Received and Open are the receiver's counters.
	Received, Open func() int
	// State reads the database after each kill; since is the previous kill.
	State func(since time.Time) (State, error)
	// Start launches the next process.
	Start func() (instance, error)
	// WaitBusy, if set, returns once the dispatcher has work. It is called
	// before the uptime of every kill timed from /readyz.
	WaitBusy func(context.Context) error
	Progress func(k Kill) // called after each kill; may be nil
	Sleep    func(context.Context, time.Duration) error
	Now      func() time.Time
}

// killLoop kills cur (running and ready) after a random uptime, starts a
// new process, and so on until it has killed cfg.Kills processes. No
// process runs when it returns.
func killLoop(ctx context.Context, cfg loopConfig, rng *rand.Rand, cur instance) ([]Kill, error) {
	kills := make([]Kill, 0, cfg.Kills)
	prev := cfg.Now()
	st, err := cfg.State(prev)
	if err != nil {
		return nil, fmt.Errorf("read the database: %w", err)
	}
	prevSeq, pending := st.MaxSeq, st.Pending
	for i := range cfg.Kills {
		k := Kill{Index: i + 1}
		// Drawn in a fixed order, so that a seed repeats the kill times.
		early := rng.Float64() < cfg.FromStart
		k.Uptime = cfg.MinUp
		if span := cfg.MaxUp - cfg.MinUp; span > 0 {
			k.Uptime += time.Duration(rng.Int64N(int64(span)))
		}
		startUp := time.Duration(0)
		if cfg.FromStartMax > 0 {
			startUp = time.Duration(rng.Int64N(int64(cfg.FromStartMax)))
		}
		var before int
		if cur == nil {
			t0 := cfg.Now()
			if cur, err = cfg.Start(); err != nil {
				return kills, fmt.Errorf("restart before kill %d: %w", k.Index, err)
			}
			if early && pending >= cfg.MinPending {
				k.FromStart, k.Uptime = true, startUp
				before = cfg.Received()
				if err := cfg.Sleep(ctx, k.Uptime-cfg.Now().Sub(t0)); err != nil {
					return kills, err
				}
			} else if err := cur.waitReady(ctx, cfg.ReadyTimeout); err != nil {
				_ = cur.kill()
				return kills, fmt.Errorf("restart before kill %d: %w", k.Index, err)
			} else {
				k.Startup = cfg.Now().Sub(t0)
			}
		}
		if !k.FromStart {
			if cfg.WaitBusy != nil {
				if err := cfg.WaitBusy(ctx); err != nil {
					_ = cur.kill()
					return kills, fmt.Errorf("kill %d: %w", k.Index, err)
				}
			}
			before = cfg.Received()
			if err := cfg.Sleep(ctx, k.Uptime); err != nil {
				_ = cur.kill()
				return kills, err
			}
		}
		k.Open = cfg.Open()
		err := cur.kill()
		cur = nil
		if err != nil {
			return kills, fmt.Errorf("kill %d: %w", k.Index, err)
		}
		at := cfg.Now()
		k.Received = cfg.Received() - before
		st, err := cfg.State(prev)
		if err != nil {
			return kills, fmt.Errorf("kill %d: read the database: %w", k.Index, err)
		}
		k.Backlog = Backlog{Pending: st.Pending, InFlight: st.InFlight}
		k.Checks, k.Events = st.Checks, int(st.MaxSeq-prevSeq)
		prev, prevSeq, pending = at, st.MaxSeq, st.Pending
		kills = append(kills, k)
		if cfg.Progress != nil {
			cfg.Progress(k)
		}
	}
	return kills, nil
}

// sleepCtx waits for d or the end of ctx. A d of zero or less returns at
// once.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
