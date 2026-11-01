// Command ordering is Phase 0 benchmark 6: it checks that our version
// schemes order real PyPI and npm version lists exactly as the registries'
// own tooling does (packaging for PyPI, node-semver for npm).
//
// It reads committed snapshots (see snapshot/ and README.md), never the
// network, so CI can run it offline:
//
//	go run ./bench/ordering
//
// A snapshot below -min-packages or -min-versions, or with packages the
// registry did not return, is an error: a hollow snapshot must not pass.
//
// Exit status: 0 when every mismatch is listed in the known-mismatch file
// (and no entry there is stale), 1 otherwise, 2 on usage or I/O errors.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/angelia-dev/angelia/internal/version/schemes"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// Report is the machine-readable result, written with -json.
type Report struct {
	Registries []RegistryReport `json:"registries"`
	// Unexplained counts the mismatches without a known-mismatch entry.
	Unexplained int `json:"unexplained"`
}

// RegistryReport is the result for one snapshot.
type RegistryReport struct {
	Meta       Meta            `json:"meta"`
	Versions   int             `json:"versions"`
	Mismatches int             `json:"mismatches"`
	Known      int             `json:"known"`
	Stale      []Known         `json:"stale,omitempty"`
	Packages   []PackageResult `json:"packages"`
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ordering", flag.ContinueOnError)
	fs.SetOutput(stderr)
	snaps := fs.String("snapshots", "bench/ordering/testdata/pypi.jsonl.gz,bench/ordering/testdata/npm.jsonl.gz",
		"comma-separated snapshot files")
	knownPath := fs.String("known", "bench/ordering/known-mismatches.json", "known-mismatch file (empty: none)")
	jsonOut := fs.String("json", "", "also write the full report as JSON to this file")
	minPackages := fs.Int("min-packages", 100, "fail when a snapshot has fewer packages")
	minVersions := fs.Int("min-versions", 10000, "fail when a snapshot has fewer versions")
	window := fs.Int("window", 3, "versions of context shown around the first difference of an order")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "ordering: unexpected arguments %q\n", fs.Args())
		return 2
	}
	known, err := LoadKnown(*knownPath)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "ordering:", err)
		return 2
	}

	var rep Report
	stale := 0
	for _, path := range strings.Split(*snaps, ",") {
		snap, err := ReadSnapshotFile(strings.TrimSpace(path))
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "ordering:", err)
			return 2
		}
		if err := snap.Validate(*minPackages, *minVersions); err != nil {
			_, _ = fmt.Fprintln(stderr, "ordering:", err)
			return 2
		}
		rr, err := checkSnapshot(snap, known)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "ordering:", err)
			return 2
		}
		printRegistry(stdout, rr, *window)
		for _, p := range rr.Packages {
			for _, m := range p.Mismatches {
				if m.Known == nil {
					rep.Unexplained++
				}
			}
		}
		stale += len(rr.Stale)
		rep.Registries = append(rep.Registries, rr)
	}

	if *jsonOut != "" {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err == nil {
			err = os.WriteFile(*jsonOut, append(b, '\n'), 0o600)
		}
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "ordering: write report:", err)
			return 2
		}
	}
	if rep.Unexplained > 0 || stale > 0 {
		_, _ = fmt.Fprintf(stdout, "RESULT: FAIL (%d unexplained mismatches, %d stale known entries)\n", rep.Unexplained, stale)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, "RESULT: PASS")
	return 0
}

// checkSnapshot compares every package of snap with the default scheme of
// its registry (the same selection, with lexical fallback, as a watch).
func checkSnapshot(snap *Snapshot, known []Known) (RegistryReport, error) {
	if r := snap.Meta.Registry; r != "pypi" && r != "npm" {
		return RegistryReport{}, fmt.Errorf("snapshot registry %q: want pypi or npm", r)
	}
	scheme, err := schemes.Select(snap.Meta.Registry, "", nil)
	if err != nil {
		return RegistryReport{}, fmt.Errorf("%s: %w", snap.Meta.Registry, err)
	}
	rr := RegistryReport{Meta: snap.Meta}
	results := make([]PackageResult, 0, len(snap.Packages))
	for _, p := range snap.Packages {
		res := ComparePackage(snap.Meta.Registry, scheme, p)
		rr.Versions += res.Versions
		results = append(results, res)
	}
	rr.Stale = ApplyKnown(snap.Meta.Registry, results, known)
	for _, res := range results {
		for _, m := range res.Mismatches {
			rr.Mismatches++
			if m.Known != nil {
				rr.Known++
			}
		}
		if len(res.Mismatches) > 0 {
			rr.Packages = append(rr.Packages, res)
		}
	}
	return rr, nil
}

func printRegistry(w io.Writer, rr RegistryReport, window int) {
	m := rr.Meta
	_, _ = fmt.Fprintf(w, "%s: %d packages, %d versions (oracle: %s; snapshot %s)\n",
		m.Registry, m.Packages, rr.Versions, m.Oracle, m.Generated)
	_, _ = fmt.Fprintf(w, "  mismatches: %d (known %d, unexplained %d)\n", rr.Mismatches, rr.Known, rr.Mismatches-rr.Known)
	for _, p := range rr.Packages {
		for _, mm := range p.Mismatches {
			tag := "MISMATCH"
			if mm.Known != nil {
				tag = "known   "
			}
			switch mm.Kind {
			case KindOrder:
				_, _ = fmt.Fprintf(w, "  %s %s %s order: %q %s %q expected, got %s\n",
					tag, m.Registry, p.Package, mm.Versions[0], mm.Expected, mm.Versions[1], mm.Actual)
			default:
				_, _ = fmt.Fprintf(w, "  %s %s %s %s: %q\n", tag, m.Registry, p.Package, mm.Kind, mm.Versions[0])
			}
			if mm.Known != nil {
				_, _ = fmt.Fprintf(w, "           %s (%s)\n", mm.Known.Explanation, mm.Known.Issue)
			}
		}
		if p.Expected != nil {
			i := firstDiff(p.Expected, p.Actual)
			_, _ = fmt.Fprintf(w, "    expected: %s\n", excerpt(p.Expected, i, window))
			_, _ = fmt.Fprintf(w, "    actual:   %s\n", excerpt(p.Actual, i, window))
		}
	}
	for _, k := range rr.Stale {
		_, _ = fmt.Fprintf(w, "  STALE known entry %s %s %s %q: no longer observed, remove it\n", k.Registry, k.Package, k.Kind, k.Versions)
	}
}

// excerpt shows order[i-window : i+window+1] with ellipses at cut ends.
func excerpt(order []string, i, window int) string {
	lo, hi := max(0, i-window), min(len(order), i+window+1)
	var b strings.Builder
	if lo > 0 {
		b.WriteString("... < ")
	}
	b.WriteString(strings.Join(order[lo:hi], " < "))
	if hi < len(order) {
		b.WriteString(" < ...")
	}
	return b.String()
}
