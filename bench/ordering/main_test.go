package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/version/lexical"
)

var committed = []string{
	"-snapshots", "testdata/pypi.jsonl.gz,testdata/npm.jsonl.gz",
	"-known", "known-mismatches.json",
}

// AC "Result: 0 mismatches, or each mismatch has an explanation and an
// issue": the benchmark itself, on the committed snapshots. CI also runs
// it as its own job (go run ./bench/ordering).
func TestBenchmarkOnCommittedSnapshots(t *testing.T) {
	if testing.Short() {
		t.Skip("full benchmark run; skipped with -short (CI runs it in the bench-ordering job and the test job)")
	}
	var out, errb bytes.Buffer
	code := run(committed, &out, &errb)
	t.Log("\n" + out.String())
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errb.String())
	}
	if !strings.HasSuffix(out.String(), "RESULT: PASS\n") {
		t.Errorf("output does not end with PASS")
	}
}

// The harness can fail: a wrong comparator on real data gives mismatches.
func TestBenchmarkDetectsWrongComparator(t *testing.T) {
	s, err := ReadSnapshotFile("testdata/npm.jsonl.gz")
	if err != nil {
		t.Fatal(err)
	}
	bad := 0
	for _, p := range s.Packages[:50] {
		bad += len(ComparePackage("npm", lexical.Scheme{}, p).Mismatches)
	}
	if bad == 0 {
		t.Error("lexical order matched npm everywhere; the harness cannot detect mismatches")
	}
}

// AC "Report lists every mismatch with package, expected and actual order".
func TestRunReportsMismatches(t *testing.T) {
	snap := writeSnapshot(t, "npm",
		Package{Name: "ok", Order: groups("1.0.0", "1.0.1")},
		Package{Name: "bad", Order: groups("1.0", "1.0.0"), Invalid: []string{"2.0.0"}},
	)
	jsonPath := filepath.Join(t.TempDir(), "report.json")
	var out, errb bytes.Buffer
	code := run(small("-snapshots", snap, "-known", "", "-json", jsonPath), &out, &errb)
	if code != 1 {
		t.Fatalf("exit %d, want 1; stderr %s", code, errb.String())
	}
	for _, want := range []string{
		"npm: 2 packages, 5 versions",
		"mismatches: 2 (known 0, unexplained 2)",
		`MISMATCH npm bad accepted: "2.0.0"`,
		`MISMATCH npm bad rejected: "1.0"`,
		"RESULT: FAIL (2 unexplained mismatches, 0 stale known entries)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	var rep Report
	b, err := os.ReadFile(jsonPath) //nolint:gosec // test temp file
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Unexplained != 2 || len(rep.Registries) != 1 || len(rep.Registries[0].Packages) != 1 {
		t.Errorf("json report = %+v", rep)
	}
}

func TestRunPrintsOrders(t *testing.T) {
	// A pypi snapshot where the registry's tie 1.0 == 1.0.0 is broken: the
	// pep440 scheme agrees, so build the disagreement with a fake order.
	snap := writeSnapshot(t, "pypi", Package{Name: "demo", Order: groups("1.0.0", "1.0", "0.9")})
	var out bytes.Buffer
	if code := run(small("-snapshots", snap, "-known", "", "-window", "1"), &out, &out); code != 1 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	for _, want := range []string{
		`MISMATCH pypi demo order: "1.0.0" < "1.0" expected, got =`,
		`MISMATCH pypi demo order: "1.0" < "0.9" expected, got >`,
		"expected: 1.0.0 < 1.0 < ...",
		"actual:   0.9 < 1.0 = 1.0.0",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestRunKnownAndStale(t *testing.T) {
	snap := writeSnapshot(t, "npm", Package{Name: "bad", Order: groups("1.0")})
	entry := Known{Registry: "npm", Package: "bad", Kind: KindRejected, Versions: []string{"1.0"}, Explanation: "npm accepted it once", Issue: "angelia-dev/angelia#999"}

	var out bytes.Buffer
	if code := run(small("-snapshots", snap, "-known", writeJSON(t, []Known{entry})), &out, &out); code != 0 {
		t.Fatalf("known: exit %d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "npm accepted it once (angelia-dev/angelia#999)") {
		t.Errorf("explanation not printed:\n%s", out.String())
	}

	staleEntry := entry
	staleEntry.Package = "gone"
	out.Reset()
	if code := run(small("-snapshots", snap, "-known", writeJSON(t, []Known{entry, staleEntry})), &out, &out); code != 1 {
		t.Fatalf("stale: exit %d, want 1:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "STALE known entry npm gone") {
		t.Errorf("stale entry not reported:\n%s", out.String())
	}
}

func TestRunUsageErrors(t *testing.T) {
	dir := t.TempDir()
	tests := map[string][]string{
		"bad flag":       {"-nope"},
		"extra argument": {"x"},
		"missing snap":   {"-known", "", "-snapshots", filepath.Join(dir, "missing.gz")},
		"bad known":      {"-known", filepath.Join(dir, "missing.json")},
		"no registry":    {"-known", "", "-snapshots", writeSnapshot(t, "")},
		"other registry": small("-known", "", "-snapshots", writeSnapshot(t, "maven", Package{Name: "a", Order: groups("1.0.0")})),
		"unwritable": small("-known", "", "-snapshots", writeSnapshot(t, "npm", Package{Name: "a", Order: groups("1.0.0")}),
			"-json", filepath.Join(dir, "no", "such", "dir.json")),
		// A hollow snapshot must not pass: default floors, and listed packages the registry lacked.
		"hollow": {"-known", "", "-snapshots", writeSnapshot(t, "npm", Package{Name: "a", Order: groups("1.0.0")})},
		"missing packages": small("-known", "", "-snapshots",
			writeSnapshotMeta(t, Meta{Registry: "npm", Missing: []string{"gone"}}, Package{Name: "a", Order: groups("1.0.0")})),
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if code := run(args, &out, &out); code != 2 {
				t.Errorf("exit %d, want 2:\n%s", code, out.String())
			}
		})
	}
}

func TestExcerpt(t *testing.T) {
	order := []string{"a", "b", "c", "d", "e"}
	if got := excerpt(order, 2, 1); got != "... < b < c < d < ..." {
		t.Errorf("got %q", got)
	}
	if got := excerpt(order, 0, 1); got != "a < b < ..." {
		t.Errorf("got %q", got)
	}
}
