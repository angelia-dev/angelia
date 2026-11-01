package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadKnown(t *testing.T) {
	ok := Known{Registry: "npm", Package: "demo", Kind: KindOrder, Versions: []string{"a", "b"}, Explanation: "why", Issue: "#1"}
	ks, err := LoadKnown(writeJSON(t, []Known{ok}))
	if err != nil || len(ks) != 1 {
		t.Fatalf("LoadKnown = %v, %v", ks, err)
	}
	if ks, err := LoadKnown(""); err != nil || ks != nil {
		t.Errorf("empty path: %v, %v", ks, err)
	}

	noIssue := ok
	noIssue.Issue = " "
	noVersions := ok
	noVersions.Versions = nil
	tests := map[string]struct {
		v    any
		want string
	}{
		"no issue":    {[]Known{noIssue}, "explanation and issue are required"},
		"no versions": {[]Known{noVersions}, "are required"},
		"duplicate":   {[]Known{ok, ok}, "duplicate"},
		"not a list":  {map[string]int{}, "cannot unmarshal"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := LoadKnown(writeJSON(t, tt.v))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
	if _, err := LoadKnown(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("missing file: want an error")
	}
}

func TestApplyKnown(t *testing.T) {
	hit := Known{Registry: "npm", Package: "demo", Kind: KindRejected, Versions: []string{"1.0"}, Explanation: "e", Issue: "#1"}
	stale := Known{Registry: "npm", Package: "gone", Kind: KindRejected, Versions: []string{"1.0"}, Explanation: "e", Issue: "#2"}
	other := Known{Registry: "pypi", Package: "demo", Kind: KindRejected, Versions: []string{"1.0"}, Explanation: "e", Issue: "#3"}
	results := []PackageResult{{Package: "demo", Mismatches: []Mismatch{
		{Registry: "npm", Package: "demo", Kind: KindRejected, Versions: []string{"1.0"}},
		{Registry: "npm", Package: "demo", Kind: KindRejected, Versions: []string{"2.0"}},
	}}}
	got := ApplyKnown("npm", results, []Known{hit, stale, other})
	if len(got) != 1 || got[0].Package != "gone" {
		t.Errorf("stale = %+v, want only %q", got, "gone")
	}
	if k := results[0].Mismatches[0].Known; k == nil || k.Issue != "#1" {
		t.Errorf("first mismatch not marked known: %+v", k)
	}
	if results[0].Mismatches[1].Known != nil {
		t.Error("second mismatch must stay unexplained")
	}
}

// The committed known-mismatch file must load (every entry explained and
// linked to an issue).
func TestCommittedKnownFile(t *testing.T) {
	if _, err := os.Stat("known-mismatches.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKnown("known-mismatches.json"); err != nil {
		t.Fatal(err)
	}
}
