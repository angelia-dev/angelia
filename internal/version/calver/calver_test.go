package calver

import (
	"errors"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/version"
)

func TestParse(t *testing.T) {
	tests := []struct {
		in, str, suffix string
		parts           int
		pre             bool
	}{
		{"2026.10.1", "2026.10.1", "", 3, false},
		{"2026.10.15", "2026.10.15", "", 3, false},
		{"v2026.10.15", "2026.10.15", "", 3, false},
		{" 24.04 ", "24.04", "", 2, false},
		{"24.04.1", "24.04.1", "", 3, false},
		{"2026.3", "2026.3", "", 2, false},
		{"2026.10.1.2", "2026.10.1.2", "", 4, false},
		{"24.04-rc1", "24.04-rc1", "-rc1", 2, true},
		{"2026.10.1-beta.2", "2026.10.1-beta.2", "-beta.2", 3, true},
		{"2026.10b1", "2026.10b1", "b1", 2, true},
		{"2026.10.1.dev3", "2026.10.1.dev3", ".dev3", 3, true},
		{"2026.10.1-hotfix", "2026.10.1-hotfix", "-hotfix", 3, false},
		{"2026.10.1+build5", "2026.10.1+build5", "+build5", 3, false},
		{"24.04-alpine", "24.04-alpine", "-alpine", 2, false},
		{"2026.10b", "2026.10b", "b", 2, true},
		{"24.04a", "24.04a", "a", 2, true},
		{"2026.10.1+rc1", "2026.10.1+rc1", "+rc1", 3, false},
		{"2026.10.1-rc1+build", "2026.10.1-rc1+build", "-rc1+build", 3, true},
	}
	for _, tc := range tests {
		v, err := Parse(tc.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", tc.in, err)
			continue
		}
		if v.String() != tc.str || v.Suffix() != tc.suffix || len(v.Parts()) != tc.parts ||
			v.IsPrerelease() != tc.pre || v.Scheme() != Name || v.Year() != v.Parts()[0] {
			t.Errorf("Parse(%q) = %q suffix %q parts %d pre %v", tc.in, v, v.Suffix(), len(v.Parts()), v.IsPrerelease())
		}
	}
}

func TestParseInvalid(t *testing.T) {
	for _, in := range []string{
		"", " ", "v", "2026", "24", "1.2.3", "1.2", "123.4", "12345.1", "2026.", "2026.10.", "2026.10-",
		"2026.10.1.2.3", "2026..10", "2026.10 .1", "2026.10/1", "latest", "v-2026.10", "2026.10-é",
		"2026.10-rc 1", "20.", ".2026.10", "2026.10-.", "2026.10..", "2026.10+", "2026.10-+-", "2026.1.2.3.4",
	} {
		_, err := Parse(in)
		if err == nil {
			t.Errorf("Parse(%q) succeeded", in)
		} else if !errors.Is(err, version.ErrInvalid) {
			t.Errorf("Parse(%q) error %v does not wrap ErrInvalid", in, err)
		}
	}
}

func mustParse(t *testing.T, s string) Version {
	t.Helper()
	v, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return v
}

func TestParseTooManyComponents(t *testing.T) {
	_, err := Parse("2026.1.2.3.4")
	if err == nil || !strings.Contains(err.Error(), "too many") {
		t.Errorf("error = %v", err)
	}
}

func TestCompare(t *testing.T) {
	// Each row: a < b (strictly).
	less := [][2]string{
		// YYYY.MM.DD
		{"2026.10.1", "2026.10.2"},
		{"2026.9.30", "2026.10.1"},
		{"2026.09.30", "2026.10.01"},
		{"2025.12.31", "2026.01.01"},
		{"2026.10.9", "2026.10.15"},
		// YY.MM
		{"24.04", "24.10"},
		{"23.10", "24.04"},
		{"24.04", "24.04.1"},
		{"24.04.1", "24.04.2"},
		// YYYY.N
		{"2026.2", "2026.10"},
		{"2025.99", "2026.1"},
		// four components
		{"2026.10.1.1", "2026.10.1.2"},
		{"2026.10.1", "2026.10.1.1"},
		// huge numbers, no overflow
		{"2026.99999999999999999999", "2026.100000000000000000000"},
		// suffixes: pre-release < release < other suffix
		{"24.04-rc1", "24.04"},
		{"24.04-rc1", "24.04-rc2"},
		{"24.04-rc2", "24.04-rc10"},
		{"24.04-alpha", "24.04-beta"},
		{"24.04-beta", "24.04-rc1"},
		{"2026.10.1-rc1", "2026.10.1"},
		{"2026.10.1", "2026.10.1-hotfix"},
		{"2026.10.1-hotfix", "2026.10.2-rc1"},
		{"2026.10.1-hotfix1", "2026.10.1-hotfix2"},
		{"2026.10.1-dev", "2026.10.1-rc1"},
		{"2026.10b1", "2026.10"},
		{"24.04-RC1", "24.04-rc2"},
		// marker rank: dev < alpha < beta < rc
		{"24.04-dev9", "24.04-a1"},
		{"24.04-alpha9", "24.04-b1"},
		{"24.04-beta9", "24.04-rc1"},
		{"24.04-rc1", "24.04-rc1.1"},
		{"24.04a1", "24.04b1"},
		{"24.04b", "24.04"},
		{"24.04+build", "24.04-hotfix"},
	}
	for _, tc := range less {
		a, b := mustParse(t, tc[0]), mustParse(t, tc[1])
		if got := Compare(a, b); got != -1 {
			t.Errorf("Compare(%q, %q) = %d, want -1", tc[0], tc[1], got)
		}
		if got := Compare(b, a); got != 1 {
			t.Errorf("Compare(%q, %q) = %d, want 1", tc[1], tc[0], got)
		}
	}
	equal := [][2]string{
		{"24.04", "24.4"},
		{"24.04", "24.04.0"},
		{"v2026.10.1", "2026.10.1"},
		{"2026.010.001", "2026.10.1"},
		{"24.04-RC1", "24.04-rc1"},
		{"2026.10.1+build5", "2026.10.1+build10"},
		{"2026.10.1+a", "2026.10.1+b"},
		{"2026.10.1-rc1+a", "2026.10.1-rc1"},
	}
	for _, tc := range equal {
		if got := Compare(mustParse(t, tc[0]), mustParse(t, tc[1])); got != 0 {
			t.Errorf("Compare(%q, %q) = %d, want 0", tc[0], tc[1], got)
		}
	}
}

func TestDiff(t *testing.T) {
	tests := []struct {
		prev, next string
		want       version.Bump
	}{
		{"2025.12.1", "2026.1.1", version.BumpMajor},
		{"23.10", "24.04", version.BumpMajor},
		{"2026.9.1", "2026.10.1", version.BumpMinor},
		{"24.04", "24.10", version.BumpMinor},
		{"2026.10.1", "2026.10.2", version.BumpPatch},
		{"24.04", "24.04.1", version.BumpPatch},
		{"2026.10.1.1", "2026.10.1.2", version.BumpPatch},
		{"24.04-rc1", "24.04-rc2", version.BumpPrerelease},
		{"24.04-rc2", "24.04", version.BumpMinor},
		{"2026.10.1-rc1", "2026.10.1", version.BumpPatch},
		{"2026.0-rc1", "2026.0", version.BumpMajor},
		{"24.04-rc1", "24.04-hotfix", version.BumpMinor},
		{"24.04", "24.05-rc1", version.BumpMinor},
		{"24.04", "24.04+b5", version.BumpOther},
		{"24.04", "24.04-hotfix", version.BumpPatch},
		{"24.04.1", "24.04", version.BumpOther},
		{"24.04", "24.04", version.BumpOther},
		{"24.04", "24.4", version.BumpOther},
	}
	for _, tc := range tests {
		if got := Diff(mustParse(t, tc.prev), mustParse(t, tc.next)); got != tc.want {
			t.Errorf("Diff(%q, %q) = %s, want %s", tc.prev, tc.next, got, tc.want)
		}
	}
}

func TestScheme(t *testing.T) {
	var s Scheme
	if s.Name() != Name {
		t.Fatalf("Name() = %q", s.Name())
	}
	a, err := s.Parse("2026.10.1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.Parse("2026.10.2-rc1")
	if s.Compare(a, b) != -1 || s.Compare(b, a) != 1 || s.Compare(a, a) != 0 {
		t.Error("Compare through the interface")
	}
	if s.IsPrerelease(a) || !s.IsPrerelease(b) {
		t.Error("IsPrerelease through the interface")
	}
	if got := s.Bump(a, b); got != version.BumpPatch {
		t.Errorf("Bump = %s", got)
	}
	if _, err := s.Parse("1.2.3"); !errors.Is(err, version.ErrInvalid) {
		t.Errorf("Parse(1.2.3) error = %v", err)
	}
}

// foreign is a version of another scheme.
type foreign string

func (f foreign) String() string { return string(f) }
func (foreign) Scheme() string   { return "other" }

func TestSchemeForeignVersions(t *testing.T) {
	var s Scheme
	c := mustParse(t, "24.04")
	f1, f2 := foreign("latest"), foreign("zeta-rc1")
	if s.Compare(c, f1) != 1 || s.Compare(f1, c) != -1 {
		t.Error("calver must rank above foreign versions")
	}
	if s.Compare(f1, f2) != -s.Compare(f2, f1) || s.Compare(f1, f2) == 0 {
		t.Error("foreign versions must keep a total order")
	}
	if s.Bump(c, f1) != version.BumpOther || s.Bump(f1, c) != version.BumpOther {
		t.Error("Bump with a foreign version must be other")
	}
	if !s.IsPrerelease(f2) || s.IsPrerelease(f1) {
		t.Error("IsPrerelease of foreign versions uses the lexical heuristic")
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"2026.10.1", "v24.04", "24.04-rc1", "2026.10.1.2", "2026.10b1", "2026.10.1+build5",
		"99999999999999999999999.1", "1.2.3", "2026.", "2026.10-", "", " 24.04 ",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		v, err := Parse(raw)
		if err != nil {
			if !errors.Is(err, version.ErrInvalid) {
				t.Fatalf("Parse(%q) error %v does not wrap ErrInvalid", raw, err)
			}
			return
		}
		s := v.String()
		w, err := Parse(s)
		if err != nil {
			t.Fatalf("String() %q of %q does not parse: %v", s, raw, err)
		}
		if w.String() != s {
			t.Fatalf("String() not stable: %q -> %q", s, w.String())
		}
		if Compare(v, w) != 0 || Compare(v, v) != 0 {
			t.Fatalf("%q does not compare equal to its normalized form", raw)
		}
	})
}

func FuzzCompare(f *testing.F) {
	f.Add("2026.10.1", "2026.10.2", "24.04")
	f.Add("24.04-rc1", "24.04", "24.04-hotfix")
	f.Add("2026.10", "2026.10.0", "2026.010")
	f.Add("2026.10b1", "2026.10-B1", "2026.10.1")
	f.Fuzz(func(t *testing.T, a, b, c string) {
		va, errA := Parse(a)
		vb, errB := Parse(b)
		vc, errC := Parse(c)
		if errA != nil || errB != nil || errC != nil {
			return
		}
		ab, bc, ac := Compare(va, vb), Compare(vb, vc), Compare(va, vc)
		if ab != -Compare(vb, va) || ab < -1 || ab > 1 {
			t.Fatalf("Compare(%q, %q) = %d is not antisymmetric", a, b, ab)
		}
		if ab <= 0 && bc <= 0 && ac > 0 {
			t.Fatalf("%q <= %q <= %q but Compare(a, c) = %d", a, b, c, ac)
		}
		if ab >= 0 && bc >= 0 && ac < 0 {
			t.Fatalf("%q >= %q >= %q but Compare(a, c) = %d", a, b, c, ac)
		}
		if bump := Diff(va, vb); ab >= 0 && bump != version.BumpOther {
			t.Fatalf("Diff(%q, %q) = %s for a non-upgrade", a, b, bump)
		}
	})
}
