package semver

import (
	"errors"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/version"
	"github.com/angelia-dev/angelia/internal/version/lexical"
)

// Valid and invalid strings from the semver.org FAQ regular expression test
// suite (https://regex101.com/r/Ly7O1x/3/), plus loose-prefix cases.
func TestParseValid(t *testing.T) {
	tests := []struct {
		in, want string
		pre      bool
	}{
		{"0.0.4", "0.0.4", false},
		{"1.2.3", "1.2.3", false},
		{"10.20.30", "10.20.30", false},
		{"1.1.2-prerelease+meta", "1.1.2-prerelease+meta", true},
		{"1.1.2+meta", "1.1.2+meta", false},
		{"1.1.2+meta-valid", "1.1.2+meta-valid", false},
		{"1.0.0-alpha", "1.0.0-alpha", true},
		{"1.0.0-beta", "1.0.0-beta", true},
		{"1.0.0-alpha.beta", "1.0.0-alpha.beta", true},
		{"1.0.0-alpha.beta.1", "1.0.0-alpha.beta.1", true},
		{"1.0.0-alpha.1", "1.0.0-alpha.1", true},
		{"1.0.0-alpha0.valid", "1.0.0-alpha0.valid", true},
		{"1.0.0-alpha.0valid", "1.0.0-alpha.0valid", true},
		{"1.0.0-alpha-a.b-c-somethinglong+build.1-aef.1-its-okay", "1.0.0-alpha-a.b-c-somethinglong+build.1-aef.1-its-okay", true},
		{"1.0.0-rc.1+build.1", "1.0.0-rc.1+build.1", true},
		{"2.0.0-rc.1+build.123", "2.0.0-rc.1+build.123", true},
		{"1.2.3-beta", "1.2.3-beta", true},
		{"10.2.3-DEV-SNAPSHOT", "10.2.3-DEV-SNAPSHOT", true},
		{"1.2.3-SNAPSHOT-123", "1.2.3-SNAPSHOT-123", true},
		{"1.0.0", "1.0.0", false},
		{"2.0.0", "2.0.0", false},
		{"1.1.7", "1.1.7", false},
		{"2.0.0+build.1848", "2.0.0+build.1848", false},
		{"2.0.1-alpha.1227", "2.0.1-alpha.1227", true},
		{"1.0.0-alpha+beta", "1.0.0-alpha+beta", true},
		{"1.2.3----RC-SNAPSHOT.12.9.1--.12+788", "1.2.3----RC-SNAPSHOT.12.9.1--.12+788", true},
		{"1.2.3----R-S.12.9.1--.12+meta", "1.2.3----R-S.12.9.1--.12+meta", true},
		{"1.2.3----RC-SNAPSHOT.12.9.1--.12", "1.2.3----RC-SNAPSHOT.12.9.1--.12", true},
		{"1.0.0+0.build.1-rc.10000aaa-kk-0.1", "1.0.0+0.build.1-rc.10000aaa-kk-0.1", false},
		{"99999999999999999999999.999999999999999999.99999999999999999", "99999999999999999999999.999999999999999999.99999999999999999", false},
		{"1.0.0-0A.is.legal", "1.0.0-0A.is.legal", true},
		// Build metadata may have leading zeros.
		{"1.0.0+001", "1.0.0+001", false},
		{"1.0.0-alpha+001", "1.0.0-alpha+001", true},
		{"1.0.0+20130313144700", "1.0.0+20130313144700", false},
		{"1.0.0-beta+exp.sha.5114f85", "1.0.0-beta+exp.sha.5114f85", true},
		{"1.0.0+21AF26D3----117B344092BD", "1.0.0+21AF26D3----117B344092BD", false},
		// npm-style loose prefix.
		{"v1.2.3", "1.2.3", false},
		{"V1.2.3", "1.2.3", false},
		{"=1.2.3", "1.2.3", false},
		{"=v1.2.3-rc.1", "1.2.3-rc.1", true},
		{"  v1.2.3\n", "1.2.3", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			v, err := Parse(tt.in)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.in, err)
			}
			if got := v.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
			if v.IsPrerelease() != tt.pre {
				t.Errorf("IsPrerelease() = %v, want %v", v.IsPrerelease(), tt.pre)
			}
			if v.Scheme() != Name {
				t.Errorf("Scheme() = %q", v.Scheme())
			}
		})
	}
}

func TestParseInvalid(t *testing.T) {
	tests := []string{
		"", " ", "v", "=", "1", "1.2", "1.2.3-0123", "1.2.3-0123.0123", "1.1.2+.123", "+invalid",
		"-invalid", "-invalid+invalid", "-invalid.01", "alpha", "alpha.beta", "alpha.beta.1",
		"alpha.1", "alpha+beta", "alpha_beta", "alpha.", "alpha..", "beta", "1.0.0-alpha_beta",
		"-alpha.", "1.0.0-alpha..", "1.0.0-alpha..1", "1.0.0-alpha...1", "1.0.0-alpha....1",
		"1.0.0-alpha.....1", "1.0.0-alpha......1", "1.0.0-alpha.......1", "01.1.1", "1.01.1",
		"1.1.01", "1.2.3.DEV", "1.2-SNAPSHOT", "1.2.31.2.3----RC-SNAPSHOT.12.09.1--..12+788",
		"1.2-RC-SNAPSHOT", "-1.0.3-gamma+b7718", "+justmeta", "9.8.7+meta+meta",
		"9.8.7-whatever+meta+meta",
		"99999999999999999999999.999999999999999999.99999999999999999----RC-SNAPSHOT.12.09.1--------------------------------..12",
		// Loose prefix is limited to "=" then "v".
		"vv1.2.3", "v=1.2.3", "==1.2.3", "1.2.3-", "1.2.3+", "1.2.3-é", "1.2.3.4", "1..3", "1.2.3 beta",
	}
	for _, in := range tests {
		t.Run(in, func(t *testing.T) {
			_, err := Parse(in)
			if !errors.Is(err, version.ErrInvalid) {
				t.Fatalf("Parse(%q) error = %v, want ErrInvalid", in, err)
			}
			var pe *version.ParseError
			if !errors.As(err, &pe) || pe.Scheme != Name || pe.Raw != in {
				t.Fatalf("error %v is not a ParseError for %q", err, in)
			}
		})
	}
}

// assertOrdered checks Compare on every pair of a strictly increasing list.
func assertOrdered(t *testing.T, list []string) {
	t.Helper()
	vs := make([]Version, len(list))
	for i, s := range list {
		v, err := Parse(s)
		if err != nil {
			t.Fatalf("Parse(%q): %v", s, err)
		}
		vs[i] = v
	}
	for i, a := range vs {
		for j, b := range vs {
			want := 0
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			if got := Compare(a, b); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", a, b, got, want)
			}
		}
	}
}

// The precedence examples of semver.org §11.
func TestPrecedenceSemverOrg(t *testing.T) {
	t.Run("core", func(t *testing.T) {
		assertOrdered(t, []string{"1.0.0", "2.0.0", "2.1.0", "2.1.1"})
	})
	t.Run("prerelease below release", func(t *testing.T) {
		assertOrdered(t, []string{"1.0.0-alpha", "1.0.0"})
	})
	t.Run("prerelease identifiers", func(t *testing.T) {
		assertOrdered(t, []string{
			"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta",
			"1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0",
		})
	})
}

func TestPrecedenceEdgeCases(t *testing.T) {
	tests := []struct {
		name string
		list []string
	}{
		{"numeric not lexical", []string{"1.9.0", "1.10.0", "1.11.0", "2.0.0", "10.0.0"}},
		{"huge numbers", []string{"1.0.0", "18446744073709551615.0.0", "18446744073709551616.0.0", "99999999999999999999999.0.0"}},
		{"numeric identifiers below alphanumeric", []string{"1.0.0-1", "1.0.0-2", "1.0.0-10", "1.0.0-a", "1.0.0-a.1"}},
		{"alphanumeric compare in ASCII order", []string{"1.0.0-ALPHA", "1.0.0-Alpha", "1.0.0-alpha", "1.0.0-alpha-1", "1.0.0-alpha0", "1.0.0-alphb"}},
		{"hyphen-led alphanumeric", []string{"1.0.0-0", "1.0.0--1", "1.0.0-0a"}},
		{"longer set wins", []string{"1.0.0-a", "1.0.0-a.0", "1.0.0-a.0.0"}},
		{"prerelease of next below release", []string{"1.0.0", "1.0.1-rc.1", "1.0.1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { assertOrdered(t, tt.list) })
	}
}

func TestBuildMetadataIgnoredInPrecedence(t *testing.T) {
	tests := [][2]string{
		{"1.0.0", "1.0.0+build"},
		{"1.0.0+a", "1.0.0+b"},
		{"1.0.0-rc.1+001", "1.0.0-rc.1+exp.sha.5114f85"},
		{"v1.0.0", "1.0.0"},
	}
	for _, tt := range tests {
		a, _ := Parse(tt[0])
		b, _ := Parse(tt[1])
		if Compare(a, b) != 0 || Compare(b, a) != 0 {
			t.Errorf("%s and %s should have equal precedence", tt[0], tt[1])
		}
	}
}

func TestAccessors(t *testing.T) {
	v, err := Parse("v1.22.333-rc.1+sha.abc")
	if err != nil {
		t.Fatal(err)
	}
	if v.Major() != "1" || v.Minor() != "22" || v.Patch() != "333" || v.Prerelease() != "rc.1" || v.Build() != "sha.abc" {
		t.Errorf("accessors = %s %s %s %s %s", v.Major(), v.Minor(), v.Patch(), v.Prerelease(), v.Build())
	}
}

func TestBump(t *testing.T) {
	tests := []struct {
		prev, next string
		want       version.Bump
	}{
		{"1.2.3", "2.0.0", version.BumpMajor},
		{"1.2.3", "2.5.1", version.BumpMajor},
		{"1.2.3", "1.3.0", version.BumpMinor},
		{"1.2.3", "1.3.7", version.BumpMinor},
		{"1.2.3", "1.2.4", version.BumpPatch},
		{"0.1.0", "0.2.0", version.BumpMinor}, // literal, even though 0.x minors may break
		{"1.2.3", "2.0.0-rc.1", version.BumpMajor},
		{"1.2.3", "1.3.0-beta", version.BumpMinor},
		{"1.2.3", "1.2.4-alpha", version.BumpPatch},
		{"1.0.0-alpha", "1.0.0-alpha.1", version.BumpPrerelease},
		{"1.0.0-rc.1", "1.0.0-rc.2", version.BumpPrerelease},
		{"2.0.0-rc.1", "2.0.0", version.BumpMajor},
		{"2.1.0-rc.1", "2.1.0", version.BumpMinor},
		{"2.1.1-rc.1", "2.1.1", version.BumpPatch},
		{"0.0.0-x", "0.0.0", version.BumpMajor},
		{"1.0.0", "1.0.0+build", version.BumpOther}, // build metadata only
		{"1.0.0", "1.0.0", version.BumpOther},
		{"2.0.0", "1.9.4", version.BumpOther}, // not an upgrade
		{"1.0.0", "1.0.0-rc.1", version.BumpOther},
		{"v1.2.3", "1.2.4", version.BumpPatch},
	}
	s := Scheme{}
	for _, tt := range tests {
		t.Run(tt.prev+"->"+tt.next, func(t *testing.T) {
			p, err := s.Parse(tt.prev)
			if err != nil {
				t.Fatal(err)
			}
			n, err := s.Parse(tt.next)
			if err != nil {
				t.Fatal(err)
			}
			if got := s.Bump(p, n); got != tt.want {
				t.Errorf("Bump(%s, %s) = %s, want %s", tt.prev, tt.next, got, tt.want)
			}
		})
	}
}

func TestSchemeWithForeignVersions(t *testing.T) {
	s := Scheme{}
	if s.Name() != Name {
		t.Fatalf("Name() = %q", s.Name())
	}
	v1, _ := s.Parse("1.0.0")
	pre, _ := s.Parse("0.0.1-rc.1")
	junk, _ := lexical.Parse("latest")
	junkRC, _ := lexical.Parse("nightly-2024")

	tests := []struct {
		name string
		a, b version.Version
		want int
	}{
		{"semver above lexical", v1, junk, 1},
		{"semver prerelease above lexical", pre, junk, 1},
		{"lexical below semver", junk, v1, -1},
		{"lexical among themselves", junk, junkRC, -1},
		{"nil below semver", nil, v1, -1},
		{"nil equals nil", nil, nil, 0},
	}
	for _, tt := range tests {
		if got := s.Compare(tt.a, tt.b); got != tt.want {
			t.Errorf("%s: Compare = %d, want %d", tt.name, got, tt.want)
		}
	}
	if s.Bump(junk, v1) != version.BumpOther || s.Bump(v1, junk) != version.BumpOther {
		t.Error("Bump with a foreign version should be other")
	}
	if !s.IsPrerelease(pre) || s.IsPrerelease(v1) {
		t.Error("IsPrerelease on semver versions")
	}
	if !s.IsPrerelease(junkRC) || s.IsPrerelease(junk) || s.IsPrerelease(nil) {
		t.Error("IsPrerelease on foreign versions should use the lexical heuristic")
	}
	if _, err := s.Parse("1.0"); !errors.Is(err, version.ErrInvalid) {
		t.Errorf("Scheme.Parse(1.0) error = %v", err)
	}
}

func TestParseErrorTruncatesLongInput(t *testing.T) {
	_, err := Parse(strings.Repeat("x", 1000))
	if err == nil || len(err.Error()) > 200 {
		t.Fatalf("error = %v", err)
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"1.2.3", "v1.2.3-rc.1+build.5", "=v0.0.0", "1.0.0-alpha.beta.1", "1.2.3----RC-SNAPSHOT.12.9.1--.12+788",
		"99999999999999999999999.0.0", "1.2", "01.1.1", "1.0.0-01", "", " v1.0.0 ",
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
	f.Add("1.0.0-alpha", "1.0.0-alpha.1")
	f.Add("1.0.0-beta.11", "1.0.0-beta.2")
	f.Add("2.0.0", "v2.0.0+build")
	f.Add("1.0.0-1", "1.0.0-a")
	f.Fuzz(func(t *testing.T, a, b string) {
		va, errA := Parse(a)
		vb, errB := Parse(b)
		if errA != nil || errB != nil {
			return
		}
		ab, ba := Compare(va, vb), Compare(vb, va)
		if ab != -ba || ab < -1 || ab > 1 {
			t.Fatalf("Compare(%q, %q) = %d but Compare(b, a) = %d", a, b, ab, ba)
		}
		if bump := Diff(va, vb); ab >= 0 && bump != version.BumpOther {
			t.Fatalf("Diff(%q, %q) = %s for a non-upgrade", a, b, bump)
		}
	})
}

func TestParseLooseCoercesShortCores(t *testing.T) {
	tests := []struct {
		in, str, core string
		coerced, pre  bool
	}{
		{"3.12", "3.12", "3.12.0", true, false},
		{"v2", "v2", "2.0.0", true, false},
		{" 1.2 ", "1.2", "1.2.0", true, false},
		{"3.12-alpine", "3.12-alpine", "3.12.0", true, true},
		{"v2-rc.1+b5", "v2-rc.1+b5", "2.0.0", true, true},
		{"v1.2.3", "1.2.3", "1.2.3", false, false}, // full versions are not coerced
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			v, err := ParseLoose(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			core := v.Major() + "." + v.Minor() + "." + v.Patch()
			if v.String() != tt.str || core != tt.core || v.Coerced() != tt.coerced || v.IsPrerelease() != tt.pre {
				t.Errorf("ParseLoose(%q) = %q core %s coerced %v pre %v", tt.in, v.String(), core, v.Coerced(), v.IsPrerelease())
			}
			if _, err := Parse(tt.in); tt.coerced && err == nil {
				t.Errorf("strict Parse(%q) should fail", tt.in)
			}
		})
	}
	for _, in := range []string{"", "v", "1.", ".1", "01.2", "1.02", "1..2", "1.2.3.4", "a.b", "1.2-", "-rc"} {
		if _, err := ParseLoose(in); !errors.Is(err, version.ErrInvalid) {
			t.Errorf("ParseLoose(%q) error = %v, want ErrInvalid", in, err)
		}
	}
}

func TestCoercedOrdering(t *testing.T) {
	s := Scheme{Coerce: true}
	lists := [][]string{
		{"1.2", "1.3.0", "1.10", "1.10.1"}, // pins 1.3.0 < 1.10
		{"1.2", "1.10.0"},
		{"1", "1.0.1", "1.1", "2-rc.1", "v2", "2.0.1"},
	}
	for _, list := range lists {
		vs := make([]version.Version, len(list))
		for i, raw := range list {
			v, err := s.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			vs[i] = v
		}
		for i, a := range vs {
			for j, b := range vs {
				want := 0
				switch {
				case i < j:
					want = -1
				case i > j:
					want = 1
				}
				if got := s.Compare(a, b); got != want {
					t.Errorf("Compare(%s, %s) = %d, want %d", a, b, got, want)
				}
			}
		}
	}
	a, b := mustLoose(t, "1.2"), mustLoose(t, "1.2.0")
	if Compare(a, b) != 0 {
		t.Error("1.2 and 1.2.0 must have equal precedence")
	}
	if got := s.Bump(a, mustLoose(t, "1.3")); got != version.BumpMinor {
		t.Errorf("Bump(1.2, 1.3) = %s", got)
	}
	if _, err := (Scheme{}).Parse("1.2"); err == nil {
		t.Error("the zero Scheme must stay strict")
	}
}

func mustLoose(t *testing.T, s string) Version {
	t.Helper()
	v, err := ParseLoose(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Bump must be given the greatest known version below next: a backport
// released after a newer major is a patch of its own line.
func TestBumpBackport(t *testing.T) {
	s := Scheme{}
	next := mustLoose(t, "1.9.4")
	var prev version.Version
	for _, k := range []string{"1.9.2", "2.0.0", "1.9.3"} {
		kv, err := s.Parse(k)
		if err != nil {
			t.Fatal(err)
		}
		if s.Compare(kv, next) < 0 && (prev == nil || s.Compare(kv, prev) > 0) {
			prev = kv
		}
	}
	if prev == nil || prev.String() != "1.9.3" {
		t.Fatalf("greatest known below 1.9.4 = %v", prev)
	}
	if got := s.Bump(prev, next); got != version.BumpPatch {
		t.Errorf("Bump(1.9.3, 1.9.4) = %s, want patch", got)
	}
	if got := s.Bump(mustLoose(t, "2.0.0"), next); got != version.BumpOther {
		t.Errorf("Bump(2.0.0, 1.9.4) = %s, want other", got)
	}
}

func FuzzParseLoose(f *testing.F) {
	for _, s := range []string{"3.12", "v2", "3.12-alpine", "1.2.3", "v2-rc.1+b5", "1.", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		v, err := ParseLoose(raw)
		if err != nil {
			if !errors.Is(err, version.ErrInvalid) {
				t.Fatalf("ParseLoose(%q) error %v does not wrap ErrInvalid", raw, err)
			}
			return
		}
		w, err := ParseLoose(v.String())
		if err != nil || w.String() != v.String() || Compare(v, w) != 0 {
			t.Fatalf("String() %q of %q does not round-trip: %v", v.String(), raw, err)
		}
	})
}
