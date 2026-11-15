package main

import (
	"context"
	"errors"
	"math/rand/v2"
	"testing"
	"time"
)

// fakeMember is a replica process that records what the cluster does to it.
type fakeMember struct {
	id                       int
	ready, killed, stopped   bool
	readyErr, killErr, stopE error
}

func (f *fakeMember) waitReady(context.Context, time.Duration) error {
	f.ready = f.readyErr == nil
	return f.readyErr
}
func (f *fakeMember) kill() error  { f.killed = true; return f.killErr }
func (f *fakeMember) exited() bool { return f.killed || f.stopped }
func (f *fakeMember) stop(time.Duration) error {
	f.stopped = true
	return f.stopE
}

// testCluster builds a cluster of n fake replicas; started lists every
// member it started, in order, with its replica.
func testCluster(n int, seed uint64) (*cluster, *[]*fakeMember) {
	var started []*fakeMember
	c := newCluster(n, func(i int) (member, error) {
		m := &fakeMember{id: i}
		started = append(started, m)
		return m, nil
	}, rand.New(rand.NewPCG(seed, 1))) //nolint:gosec // test
	return c, &started
}

func mustNil(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// With one replica the cluster is that process: it is always the victim and
// the random stream is not drawn from, so a seed repeats its kill times.
func TestClusterOneReplicaIsAlwaysTheVictim(t *testing.T) {
	c, started := testCluster(1, 1)
	mustNil(t, c.startAll())
	mustNil(t, c.waitReady(t.Context(), time.Second))
	for range 5 {
		mustNil(t, c.kill())
		if got := c.lastKilled(); got != 0 {
			t.Fatalf("lastKilled with one replica = %d, want 0 (left out of the report)", got)
		}
		if _, err := c.restart(); err != nil {
			t.Fatal(err)
		}
		mustNil(t, c.waitReady(t.Context(), time.Second))
	}
	if len(*started) != 6 {
		t.Errorf("started %d processes, want 1 + 5 restarts", len(*started))
	}
	if want, got := rand.New(rand.NewPCG(1, 1)).Uint64(), c.rng.Uint64(); want != got { //nolint:gosec // test
		t.Error("a one-replica cluster drew from its random stream")
	}
}

// Each kill takes down exactly one replica, chosen at random among those
// running, and the next restart brings that one back.
func TestClusterKillsOneRandomReplicaAndRestartsIt(t *testing.T) {
	c, started := testCluster(3, 7)
	mustNil(t, c.startAll())
	mustNil(t, c.waitReady(t.Context(), time.Second))
	seen := map[int]int{}
	for i := range 90 {
		running := 0
		for _, m := range c.members {
			if m != nil {
				running++
			}
		}
		if running != 3 {
			t.Fatalf("kill %d: %d replicas running, want 3", i+1, running)
		}
		before := len(*started)
		mustNil(t, c.kill())
		down := c.lastKilled()
		seen[down]++
		for j, m := range c.members {
			if (m == nil) != (j == down-1) {
				t.Fatalf("kill %d: replica %d down = %v, want only replica %d down", i+1, j+1, m == nil, down)
			}
		}
		if _, err := c.restart(); err != nil {
			t.Fatal(err)
		}
		if len(*started) != before+1 || (*started)[before].id != down-1 {
			t.Fatalf("kill %d: restarted %v, want replica %d", i+1, *started, down)
		}
		mustNil(t, c.waitReady(t.Context(), time.Second))
	}
	for r := 1; r <= 3; r++ {
		if seen[r] < 10 {
			t.Errorf("replica %d was killed %d times in 90 kills: the choice is not random enough (%v)", r, seen[r], seen)
		}
	}
}

// A kill timed from the start of the process (the replica was restarted and
// is not ready) takes down that replica, however many are running.
func TestClusterEarlyKillTakesTheRestartedReplica(t *testing.T) {
	c, _ := testCluster(4, 3)
	mustNil(t, c.startAll())
	mustNil(t, c.waitReady(t.Context(), time.Second))
	for range 20 {
		mustNil(t, c.kill())
		first := c.lastKilled()
		if _, err := c.restart(); err != nil {
			t.Fatal(err)
		}
		// no waitReady: the kill lands in its startup
		mustNil(t, c.kill())
		if got := c.lastKilled(); got != first {
			t.Fatalf("early kill took replica %d, want the restarted replica %d", got, first)
		}
		if _, err := c.restart(); err != nil {
			t.Fatal(err)
		}
		mustNil(t, c.waitReady(t.Context(), time.Second))
	}
}

// The kill loop sees the cluster as one instance: waitReady waits for the
// restarted replica, and exited reports a replica that died on its own.
func TestClusterWaitReadyAndExited(t *testing.T) {
	c, started := testCluster(2, 1)
	mustNil(t, c.startAll())
	if c.exited() {
		t.Fatal("exited right after the start")
	}
	if err := c.waitReady(t.Context(), time.Second); err != nil {
		t.Fatal(err)
	}
	for _, m := range *started {
		if !m.ready {
			t.Errorf("replica %d not waited for", m.id+1)
		}
	}
	(*started)[1].killed = true // died without a kill from the harness
	if !c.exited() {
		t.Error("a replica that ended on its own is not reported")
	}
	(*started)[1].killed = false
	(*started)[0].readyErr = errors.New("never ready")
	mustNil(t, c.kill())
	if _, err := c.restart(); err != nil {
		t.Fatal(err)
	}
	(*started)[len(*started)-1].readyErr = errors.New("never ready")
	if err := c.waitReady(t.Context(), time.Second); err == nil {
		t.Error("waitReady passed a replica that is not ready")
	}
}

func TestClusterStopAllAndKillAll(t *testing.T) {
	c, started := testCluster(3, 1)
	mustNil(t, c.startAll())
	mustNil(t, c.waitReady(t.Context(), time.Second))
	mustNil(t, c.kill())
	if err := c.stopAll(time.Second); err != nil {
		t.Fatal(err)
	}
	stopped := 0
	for _, m := range *started {
		if m.stopped {
			stopped++
		}
	}
	if stopped != 2 {
		t.Errorf("stopAll stopped %d replicas, want the 2 that were running", stopped)
	}
	// the next startAll brings every replica back, killed or stopped
	mustNil(t, c.startAll())
	if len(*started) != 6 {
		t.Errorf("started %d processes, want 3 + 3", len(*started))
	}
	c.killAll()
	for _, m := range (*started)[3:] {
		if !m.killed {
			t.Errorf("replica %d survived killAll", m.id+1)
		}
	}
	if c.victim() != -1 {
		t.Error("a victim after killAll")
	}
	// a failing stop is reported with its replica
	c, started = testCluster(2, 1)
	mustNil(t, c.startAll())
	(*started)[1].stopE = errors.New("did not drain")
	if err := c.stopAll(time.Second); err == nil || err.Error() != "replica 2: did not drain" {
		t.Errorf("stopAll error = %v", err)
	}
}

// The loop records which replica each kill took down.
func TestKillLoopRecordsTheReplica(t *testing.T) {
	c, _ := testCluster(2, 5)
	mustNil(t, c.startAll())
	mustNil(t, c.waitReady(t.Context(), time.Second))
	reads := 0
	cfg := loopConfig{
		Kills: 12, MinUp: time.Millisecond, MaxUp: 2 * time.Millisecond, ReadyTimeout: time.Second,
		Received: func() int { return 0 }, Open: func() int { return 0 },
		State: fakeState(0, &reads), Start: c.restart,
		Sleep: func(context.Context, time.Duration) error { return nil }, Now: time.Now,
	}
	kills, err := killLoop(t.Context(), cfg, seeded(), instance(c))
	if err != nil {
		t.Fatal(err)
	}
	n := map[int]int{}
	for _, k := range kills {
		if k.Replica != 1 && k.Replica != 2 {
			t.Fatalf("kill %d: replica %d", k.Index, k.Replica)
		}
		n[k.Replica]++
	}
	if n[1] == 0 || n[2] == 0 {
		t.Errorf("12 kills hit replicas %v: want both", n)
	}
}
