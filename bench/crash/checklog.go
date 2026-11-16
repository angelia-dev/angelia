package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"
)

// CheckRun is one check of a target by one holder, from the scheduler's two
// debug lines "check started" and "check finished" (internal/scheduler).
type CheckRun struct {
	Target, Holder string
	// Replica is the log the check came from, counted from 1 (0: unknown).
	// Failure details name it, not the holder, which holds the hostname.
	Replica int
	Start   time.Time
	// End is when the "check finished" line was written. A holder killed
	// during its check never writes it: Finished is then false, and End is
	// the lease expiry named by the start line, the last moment the lease
	// could protect the check (a check this short never renews it).
	End      time.Time
	Finished bool
}

// logLine is the part of a scheduler log line that parseCheckLog reads.
type logLine struct {
	Time       time.Time `json:"time"`
	Msg        string    `json:"msg"`
	Role       string    `json:"role"`
	Holder     string    `json:"holder"`
	Target     string    `json:"target"`
	LeaseUntil time.Time `json:"lease_until"`
}

// parseCheckLog reads the JSON log of one replica (every process it ran,
// appended) and returns its checks, in the order they started. Lines that
// are not JSON (a panic's stack, a line cut off by SIGKILL) and lines of
// other messages are skipped. A finish line with no start line is an error.
// A holder that starts a target twice before the first finish line leaves
// the first run unfinished, which checkOverlaps then reports.
func parseCheckLog(r io.Reader) ([]CheckRun, error) {
	var runs []CheckRun
	open := map[[2]string]int{} // (holder, target) -> index into runs of the unfinished check
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var l logLine
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.Role != "scheduler" || l.Target == "" {
			continue
		}
		k := [2]string{l.Holder, l.Target}
		switch l.Msg {
		case "check started":
			if l.Holder == "" || l.Time.IsZero() || l.LeaseUntil.IsZero() {
				return nil, fmt.Errorf("check started for %s lacks the holder, time or lease_until: %s", l.Target, sc.Bytes())
			}
			open[k] = len(runs)
			runs = append(runs, CheckRun{Target: l.Target, Holder: l.Holder, Start: l.Time, End: l.LeaseUntil.Truncate(time.Millisecond)}) // the store keeps milliseconds
		case "check finished":
			i, ok := open[k]
			if !ok {
				return nil, fmt.Errorf("holder %s finished target %s at %s without a start line", l.Holder, l.Target, l.Time.Format(time.RFC3339Nano))
			}
			runs[i].End, runs[i].Finished = l.Time, true
			delete(open, k)
		}
	}
	return runs, sc.Err()
}

// readCheckLogs parses the logs at paths and returns the checks of each,
// in the order of paths; Replica is the position of the log, from 1.
func readCheckLogs(paths []string) ([][]CheckRun, error) {
	var all [][]CheckRun
	for i, p := range paths {
		f, err := os.Open(p) //nolint:gosec // the run's own log files
		if err != nil {
			return nil, err
		}
		runs, err := parseCheckLog(f)
		err = errors.Join(err, f.Close())
		if err != nil {
			return nil, fmt.Errorf("replica %d log: %w", i+1, err)
		}
		for j := range runs {
			runs[j].Replica = i + 1
		}
		all = append(all, runs)
	}
	return all, nil
}

// noOverlapCheck is the check of the report named so.
const noOverlapCheck = "no target checked by two holders at once (from the scheduler logs)"

// checkOverlaps fails when two checks of one target overlap in time, by
// two holders or by one. A check lasts from its start line to its finish
// line, or, for a holder killed in the middle of it, to the expiry of its
// lease; another holder can only take the target over after that expiry.
// The logs are those of every replica, one slice each: the holders of the
// replicas and of all their restarts are distinct (each process draws its
// own).
//
// It also fails when the logs cannot support the verdict: a replica's log
// holds no check, a target the database holds was never checked in any log,
// or a finished check lasted longer than a third of the lease, when the
// scheduler renews it and the end of a check cut off by a kill (the lease
// expiry the start line names) would be too early. Failure details name the
// replica, never the holder, which holds the hostname.
func checkOverlaps(logs [][]CheckRun, leaseTTL time.Duration, targets []string) Check {
	byTarget := map[string][]CheckRun{}
	holders := map[string]bool{}
	var unfinished, total int
	var bad []string
	for i, runs := range logs {
		if len(runs) == 0 {
			bad = append(bad, fmt.Sprintf("replica %d: its log holds no check: the debug lines are missing, or it never ran a scheduler", i+1))
		}
		for _, r := range runs {
			total++
			byTarget[r.Target] = append(byTarget[r.Target], r)
			holders[r.Holder] = true
			if !r.Finished {
				unfinished++
			} else if d := r.End.Sub(r.Start); d > leaseTTL/3 {
				bad = append(bad, fmt.Sprintf("%s: replica %d: a check of %s outlived a third of the lease, so its lease was renewed and the overlap check cannot judge it", r.Target, r.Replica, d))
			}
		}
	}
	for _, t := range targets {
		if len(byTarget[t]) == 0 {
			bad = append(bad, t+": in the database, but no log holds a check of it")
		}
	}
	for target, rs := range byTarget {
		slices.SortFunc(rs, func(a, b CheckRun) int { return a.Start.Compare(b.Start) })
		for i, b := range rs {
			for _, a := range rs[:i] {
				if b.Start.Before(a.End) {
					bad = append(bad, fmt.Sprintf("%s: replica %d ran %s to %s (finished: %t), replica %d started at %s",
						target, a.Replica, a.Start.Format("15:04:05.000"), a.End.Format("15:04:05.000"), a.Finished, b.Replica, b.Start.Format("15:04:05.000")))
				}
			}
		}
	}
	if total == 0 {
		bad = append(bad, "the logs hold no check: the scheduler's debug lines are missing")
	}
	var c checker
	c.add(noOverlapCheck, fmt.Sprintf("%d checks of %d targets in %d logs by %d holders (%d cut off by a kill), none overlapping, every database target checked",
		total, len(byTarget), len(logs), len(holders), unfinished), bad)
	return c.checks[0]
}
