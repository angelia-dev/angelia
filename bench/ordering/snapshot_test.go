package main

import (
	"bytes"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReadSnapshot(t *testing.T) {
	data := gzLines(t,
		map[string]Meta{"meta": {Registry: "pypi", Oracle: "packaging 26.3", Packages: 1, Versions: 3}},
		Package{Name: "demo", Order: [][]string{{"1.0", "1.0.0"}}, Invalid: []string{"x"}},
	)
	s, err := ReadSnapshot(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	want := &Snapshot{
		Meta:     Meta{Registry: "pypi", Oracle: "packaging 26.3", Packages: 1, Versions: 3},
		Packages: []Package{{Name: "demo", Order: [][]string{{"1.0", "1.0.0"}}, Invalid: []string{"x"}}},
	}
	if !reflect.DeepEqual(s, want) {
		t.Errorf("got %+v, want %+v", s, want)
	}
}

func TestReadSnapshotErrors(t *testing.T) {
	meta := map[string]Meta{"meta": {Registry: "npm", Packages: 1}}
	tests := map[string]struct {
		data []byte
		want string
	}{
		"not gzip":      {[]byte("plain"), "gzip"},
		"empty":         {gzLines(t), "empty snapshot"},
		"no meta":       {gzLines(t, Package{Name: "x"}), "missing meta.registry"},
		"bad meta":      {gzLines(t, "{"), "line 1"},
		"bad package":   {gzLines(t, meta, "[1]"), "line 2"},
		"missing name":  {gzLines(t, meta, Package{}), "missing name"},
		"count differs": {gzLines(t, meta), "meta says 1 packages, file has 0"},
		"versions differ": {
			gzLines(t, map[string]Meta{"meta": {Registry: "npm", Packages: 1, Versions: 5}}, Package{Name: "x", Order: groups("1.0.0")}),
			"meta says 5 versions, file has 1",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ReadSnapshot(bytes.NewReader(tt.data))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestReadSnapshotFileMissing(t *testing.T) {
	if _, err := ReadSnapshotFile(filepath.Join(t.TempDir(), "nope.jsonl.gz")); err == nil {
		t.Error("want an error for a missing file")
	}
}

// AC "Snapshot is reproducible and committed (compressed)": the committed
// snapshots parse, cover both registries and match their own metadata.
func TestCommittedSnapshots(t *testing.T) {
	for _, reg := range []string{"pypi", "npm"} {
		s, err := ReadSnapshotFile(filepath.Join("testdata", reg+".jsonl.gz"))
		if err != nil {
			t.Fatal(err)
		}
		if s.Meta.Registry != reg || len(s.Packages) < 400 || s.Meta.Oracle == "" {
			t.Errorf("%s: meta %+v, %d packages", reg, s.Meta, len(s.Packages))
		}
		n := 0
		for i, p := range s.Packages {
			if i > 0 && s.Packages[i-1].Name >= p.Name {
				t.Errorf("%s: packages not sorted by name at %q", reg, p.Name)
			}
			n += len(p.Invalid)
			for _, g := range p.Order {
				n += len(g)
			}
		}
		if n != s.Meta.Versions {
			t.Errorf("%s: %d versions, meta says %d", reg, n, s.Meta.Versions)
		}
	}
}

func TestSnapshotValidate(t *testing.T) {
	two := []Package{{Name: "a", Order: groups("1.0.0", "1.1.0")}, {Name: "b", Invalid: []string{"x"}}}
	tests := map[string]struct {
		snap       Snapshot
		minP, minV int
		wantErr    string
	}{
		"ok":            {Snapshot{Meta: Meta{Registry: "npm"}, Packages: two}, 2, 3, ""},
		"few packages":  {Snapshot{Meta: Meta{Registry: "npm"}, Packages: two}, 3, 1, "2 packages, want at least 3"},
		"few versions":  {Snapshot{Meta: Meta{Registry: "npm"}, Packages: two}, 1, 4, "3 versions, want at least 4"},
		"empty":         {Snapshot{Meta: Meta{Registry: "npm"}}, 1, 1, "0 packages"},
		"missing names": {Snapshot{Meta: Meta{Registry: "pypi", Missing: []string{"gone"}}, Packages: two}, 1, 1, `missing from the registry ["gone"]`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := tt.snap.Validate(tt.minP, tt.minV)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
