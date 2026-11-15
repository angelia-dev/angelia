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
	Start          time.Time
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
			runs = append(runs, CheckRun{Target: l.Target, Holder: l.Holder, Start: l.Time, End: l.LeaseUntil})
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

// readCheckLogs parses the logs at paths and returns every check.
func readCheckLogs(paths []string) ([]CheckRun, error) {
	var all []CheckRun
	for _, p := range paths {
		f, err := os.Open(p) //nolint:gosec // the run's own log files
		if err != nil {
			return nil, err
		}
		runs, err := parseCheckLog(f)
		err = errors.Join(err, f.Close())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		all = append(all, runs...)
	}
	return all, nil
}

// noOverlapCheck is the check of the report named so.
const noOverlapCheck = "no target checked by two holders at once (from the scheduler logs)"

// checkOverlaps fails when two checks of one target overlap in time, by
// two holders or by one. A check lasts from its start line to its finish
// line, or, for a holder killed in the middle of it, to the expiry of its
// lease; another holder can only take the target over after that expiry. It
// reads the logs of every replica: the holders of the replicas and of all
// their restarts are distinct (each process draws its own).
func checkOverlaps(runs []CheckRun) Check {
	byTarget := map[string][]CheckRun{}
	holders := map[string]bool{}
	var unfinished int
	for _, r := range runs {
		byTarget[r.Target] = append(byTarget[r.Target], r)
		holders[r.Holder] = true
		if !r.Finished {
			unfinished++
		}
	}
	var bad []string
	for target, rs := range byTarget {
		slices.SortFunc(rs, func(a, b CheckRun) int { return a.Start.Compare(b.Start) })
		for i, b := range rs {
			for _, a := range rs[:i] {
				if b.Start.Before(a.End) {
					bad = append(bad, fmt.Sprintf("%s: holder %s ran %s to %s (finished: %t), holder %s started at %s",
						target, a.Holder, a.Start.Format("15:04:05.000"), a.End.Format("15:04:05.000"), a.Finished, b.Holder, b.Start.Format("15:04:05.000")))
				}
			}
		}
	}
	if len(runs) == 0 {
		// A run whose logs hold no check proves nothing.
		bad = append(bad, "the logs hold no check: the scheduler's debug lines are missing")
	}
	var c checker
	c.add(noOverlapCheck, fmt.Sprintf("%d checks of %d targets by %d holders (%d cut off by a kill), none overlapping", len(runs), len(byTarget), len(holders), unfinished), bad)
	return c.checks[0]
}
