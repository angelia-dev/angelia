package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

// member is one replica process as the cluster sees it.
type member interface {
	instance
	exited() bool
	stop(timeout time.Duration) error
}

// cluster is the set of replicas of one run: N `angelia serve` processes
// with every role, on one database, each with its own holder. It is the
// instance of the kill loop: a kill takes down one replica, and the next
// restart brings that replica back while the others keep running. With one
// replica it is that one process, as before.
type cluster struct {
	start func(replica int) (member, error)
	rng   *rand.Rand // picks the replica to kill, only when several are up

	members []member // nil: the replica is down
	unready []int    // replicas started and not yet waited for
	down    []int    // replicas killed and not yet restarted
	last    int      // the replica of the latest kill, counted from 1
}

func newCluster(replicas int, start func(replica int) (member, error), rng *rand.Rand) *cluster {
	return &cluster{start: start, rng: rng, members: make([]member, replicas)}
}

// startAll starts every replica that is down, as the warm-up and the drain
// do. A replica that fails to start stops the others.
func (c *cluster) startAll() error {
	for i := range c.members {
		if c.members[i] != nil {
			continue
		}
		m, err := c.start(i)
		if err != nil {
			c.killAll()
			return fmt.Errorf("replica %d: %w", i+1, err)
		}
		c.members[i] = m
		c.unready = append(c.unready, i)
	}
	c.down = nil
	return nil
}

// restart starts the replicas the kills took down and returns the cluster
// as the next instance of the kill loop. The restarted replica is the one a
// kill timed from the start of the process takes down again.
func (c *cluster) restart() (instance, error) {
	down := c.down
	c.down = nil
	for _, i := range down {
		m, err := c.start(i)
		if err != nil {
			c.down = append(c.down, i)
			return nil, fmt.Errorf("replica %d: %w", i+1, err)
		}
		c.members[i] = m
		c.unready = append(c.unready, i)
	}
	return c, nil
}

// waitReady waits for every replica started since the last wait.
func (c *cluster) waitReady(ctx context.Context, timeout time.Duration) error {
	for len(c.unready) > 0 {
		i := c.unready[0]
		if err := c.members[i].waitReady(ctx, timeout); err != nil {
			return fmt.Errorf("replica %d: %w", i+1, err)
		}
		c.unready = c.unready[1:]
	}
	return nil
}

// kill sends SIGKILL to one replica. If a replica was just restarted and is
// not ready (a kill timed from the start of the process), that one dies:
// the point of such a kill is to land in its startup. Otherwise it is a
// random one of the running replicas.
func (c *cluster) kill() error {
	v := c.victim()
	if v < 0 {
		return errors.New("no replica is running")
	}
	err := c.members[v].kill()
	c.last = v + 1
	c.members[v] = nil
	c.unready = removeInt(c.unready, v)
	c.down = append(c.down, v)
	if err != nil {
		return fmt.Errorf("replica %d: %w", v+1, err)
	}
	return nil
}

// lastKilled returns the replica of the latest kill, counted from 1; with a
// single replica, 0, as the report leaves it out.
func (c *cluster) lastKilled() int {
	if len(c.members) == 1 {
		return 0
	}
	return c.last
}

// victim returns the replica the next kill takes down, or -1.
func (c *cluster) victim() int {
	if len(c.unready) > 0 {
		return c.unready[len(c.unready)-1]
	}
	var up []int
	for i, m := range c.members {
		if m != nil {
			up = append(up, i)
		}
	}
	switch len(up) {
	case 0:
		return -1
	case 1:
		return up[0] // no draw: a one-replica run keeps its seed's kill times
	}
	return up[c.rng.IntN(len(up))]
}

// exited reports whether any replica that should be running has ended.
func (c *cluster) exited() bool {
	for _, m := range c.members {
		if m != nil && m.exited() {
			return true
		}
	}
	return false
}

// stopAll sends SIGTERM to every running replica at once and waits for each
// to drain.
func (c *cluster) stopAll(timeout time.Duration) error {
	errs := make([]error, len(c.members))
	var wg sync.WaitGroup
	for i, m := range c.members {
		if m == nil {
			continue
		}
		wg.Go(func() {
			if err := m.stop(timeout); err != nil {
				errs[i] = fmt.Errorf("replica %d: %w", i+1, err)
			}
		})
	}
	wg.Wait()
	for i := range c.members {
		c.members[i] = nil
	}
	c.unready = nil
	return errors.Join(errs...)
}

// killAll is the cleanup of a failed run: no replica may outlive it.
func (c *cluster) killAll() {
	for i, m := range c.members {
		if m != nil && !m.exited() {
			_ = m.kill()
		}
		c.members[i] = nil
	}
	c.unready = nil
}

func removeInt(s []int, v int) []int {
	out := s[:0:0]
	for _, x := range s {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}
