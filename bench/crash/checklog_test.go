package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

var logT0 = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

// logEntry writes a scheduler log line: msg at offset (ms from logT0) for a
// holder and target; a start line carries its lease expiry.
func logEntry(msg, holder, target string, at, leaseMs int) string {
	until := ""
	if leaseMs > 0 {
		until = fmt.Sprintf(`,"lease_until":%q`, logT0.Add(time.Duration(leaseMs)*time.Millisecond).Format(time.RFC3339Nano))
	}
	return fmt.Sprintf(`{"time":%q,"level":"DEBUG","msg":%q,"role":"scheduler","holder":%q,"target":%q%s}`,
		logT0.Add(time.Duration(at)*time.Millisecond).Format(time.RFC3339Nano), msg, holder, target, until)
}

func TestParseCheckLog(t *testing.T) {
	log := strings.Join([]string{
		`not json at all`,
		`{"time":"2026-11-15T12:00:00Z","msg":"check started","role":"dispatcher","holder":"x","target":"t9"}`, // another role
		logEntry("check started", "h1", "t1", 0, 300000),
		logEntry("check finished", "h1", "t1", 8, 0),
		logEntry("check started", "h1", "t2", 10, 300010),     // killed in the middle: no finish
		`{"time":"2026-11-15T12:00:00.020Z","msg":"check fin`, // cut off by the SIGKILL
	}, "\n")
	runs, err := parseCheckLog(strings.NewReader(log))
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs %+v", runs)
	}
	if r := runs[0]; r.Target != "t1" || r.Holder != "h1" || !r.Finished || r.End.Sub(r.Start) != 8*time.Millisecond {
		t.Errorf("finished run %+v", r)
	}
	if r := runs[1]; r.Finished || !r.End.Equal(logT0.Add(300010*time.Millisecond)) {
		t.Errorf("a run cut off by a kill ends at its lease expiry: %+v", r)
	}
	for _, bad := range []string{
		logEntry("check finished", "h1", "t1", 8, 0),                                                         // no start line
		`{"time":"2026-11-15T12:00:00Z","msg":"check started","role":"scheduler","holder":"h","target":"t"}`, // no lease_until
	} {
		if _, err := parseCheckLog(strings.NewReader(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func chk(target, holder string, start, end int, finished bool) CheckRun {
	return CheckRun{
		Target: target, Holder: holder, Finished: finished,
		Start: logT0.Add(time.Duration(start) * time.Millisecond), End: logT0.Add(time.Duration(end) * time.Millisecond),
	}
}

func TestCheckOverlaps(t *testing.T) {
	ok := []CheckRun{
		chk("t1", "h1", 0, 10, true),
		chk("t1", "h2", 300000, 300010, true), // the next cycle, another holder
		chk("t2", "h1", 5, 15, true),          // another target, at the same time
		chk("t3", "h1", 0, 300000, false),     // h1 killed; its lease expired at 300000
		chk("t3", "h2", 300000, 300008, true), // h2 took it over exactly then
	}
	if c := checkOverlaps(ok); !c.OK {
		t.Errorf("clean runs failed: %+v", c)
	}
	for name, runs := range map[string][]CheckRun{
		"two holders at once":            {chk("t1", "h1", 0, 10, true), chk("t1", "h2", 5, 20, true)},
		"started inside a killed lease":  {chk("t1", "h1", 0, 300000, false), chk("t1", "h2", 120000, 120005, true)},
		"the same holder twice at once":  {chk("t1", "h1", 0, 10, true), chk("t1", "h1", 9, 20, true)},
		"started before the other ended": {chk("t1", "h2", 5, 20, true), chk("t1", "h1", 0, 6, true)},
		"no check logged":                nil,
	} {
		c := checkOverlaps(runs)
		if c.OK || len(c.Examples) == 0 {
			t.Errorf("%s: check passed: %+v", name, c)
		}
	}
	c := checkOverlaps(ok)
	if !strings.Contains(c.Detail, "5 checks of 3 targets by 2 holders (1 cut off by a kill)") {
		t.Errorf("detail %q", c.Detail)
	}
}
