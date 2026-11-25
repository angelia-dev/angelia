package maven

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/version"
	"github.com/angelia-dev/angelia/internal/version/lexical"
)

func mustParse(t testing.TB, s string) Version {
	t.Helper()
	v, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return v
}

// checkOrder asserts that list is in strictly increasing order, every pair.
func checkOrder(t *testing.T, list ...string) {
	t.Helper()
	for i := range list {
		for j := range list {
			want := cmpInt(i, j)
			if got := Compare(mustParse(t, list[i]), mustParse(t, list[j])); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", list[i], list[j], got, want)
			}
		}
	}
}

func checkEqual(t *testing.T, a, b string) {
	t.Helper()
	va, vb := mustParse(t, a), mustParse(t, b)
	if Compare(va, vb) != 0 || Compare(vb, va) != 0 {
		t.Errorf("expected %s == %s", a, b)
	}
}

// The vectors below are ported from Maven's ComparableVersionTest
// (maven-artifact, tag maven-3.9.16, Apache-2.0); see testdata/NOTICE.

func TestMavenVersionsQualifier(t *testing.T) {
	checkOrder(t,
		"1-alpha2snapshot", "1-alpha2", "1-alpha-123", "1-beta-2", "1-beta123", "1-m2", "1-m11", "1-rc", "1-cr2",
		"1-rc123", "1-SNAPSHOT", "1", "1-sp", "1-sp2", "1-sp123", "1-abc", "1-def", "1-pom-1", "1-1-snapshot",
		"1-1", "1-2", "1-123")
}

func TestMavenVersionsNumber(t *testing.T) {
	checkOrder(t,
		"2.0", "2.0.a", "2-1", "2.0.2", "2.0.123", "2.1.0", "2.1-a", "2.1b", "2.1-c", "2.1-1", "2.1.0.1", "2.2",
		"2.123", "11.a2", "11.a11", "11.b2", "11.b11", "11.m2", "11.m11", "11", "11.a", "11b", "11c", "11m")
}

func TestMavenVersionsEqual(t *testing.T) {
	for _, p := range [][2]string{
		{"1", "1"},
		{"1", "1.0"},
		{"1", "1.0.0"},
		{"1.0", "1.0.0"},
		{"1", "1-0"},
		{"1", "1.0-0"},
		{"1.0", "1.0-0"},
		// no separator between number and character
		{"1a", "1-a"},
		{"1a", "1.0-a"},
		{"1a", "1.0.0-a"},
		{"1.0a", "1-a"},
		{"1.0.0a", "1-a"},
		{"1x", "1-x"},
		{"1x", "1.0-x"},
		{"1x", "1.0.0-x"},
		{"1.0x", "1-x"},
		{"1.0.0x", "1-x"},
		// aliases
		{"1ga", "1"},
		{"1release", "1"},
		{"1final", "1"},
		{"1cr", "1rc"},
		// special "aliases" a, b and m for alpha, beta and milestone
		{"1a1", "1-alpha-1"},
		{"1b2", "1-beta-2"},
		{"1m3", "1-milestone-3"},
		// case insensitive
		{"1X", "1x"},
		{"1A", "1a"},
		{"1B", "1b"},
		{"1M", "1m"},
		{"1Ga", "1"},
		{"1GA", "1"},
		{"1RELEASE", "1"},
		{"1release", "1"},
		{"1RELeaSE", "1"},
		{"1Final", "1"},
		{"1FinaL", "1"},
		{"1FINAL", "1"},
		{"1Cr", "1Rc"},
		{"1cR", "1rC"},
		{"1m3", "1Milestone3"},
		{"1m3", "1MileStone3"},
		{"1m3", "1MILESTONE3"},
		// testLocaleIndependent
		{"1-abcdefghijklmnopqrstuvwxyz", "1-ABCDEFGHIJKLMNOPQRSTUVWXYZ"},
	} {
		checkEqual(t, p[0], p[1])
	}
}

func TestMavenVersionComparing(t *testing.T) {
	for _, p := range [][2]string{
		{"1", "2"},
		{"1.5", "2"},
		{"1", "2.5"},
		{"1.0", "1.1"},
		{"1.1", "1.2"},
		{"1.0.0", "1.1"},
		{"1.0.1", "1.1"},
		{"1.1", "1.2.0"},
		{"1.0-alpha-1", "1.0"},
		{"1.0-alpha-1", "1.0-alpha-2"},
		{"1.0-alpha-1", "1.0-beta-1"},
		{"1.0-beta-1", "1.0-SNAPSHOT"},
		{"1.0-SNAPSHOT", "1.0"},
		{"1.0-alpha-1-SNAPSHOT", "1.0-alpha-1"},
		{"1.0", "1.0-1"},
		{"1.0-1", "1.0-2"},
		{"1.0.0", "1.0-1"},
		{"2.0-1", "2.0.1"},
		{"2.0.1-klm", "2.0.1-lmn"},
		{"2.0.1", "2.0.1-xyz"},
		{"2.0.1", "2.0.1-123"},
		{"2.0.1-xyz", "2.0.1-123"},
		// MNG-5568
		{"6.1.0rc3", "6.1.0"},
		{"6.1.0rc3", "6.1H.5-beta"},
		{"6.1.0", "6.1H.5-beta"},
		// MNG-6572
		{"20190126.230843", "1234567890.12345"},
		{"1234567890.12345", "123456789012345.1H.5-beta"},
		{"20190126.230843", "123456789012345.1H.5-beta"},
		{"123456789012345.1H.5-beta", "12345678901234567890.1H.5-beta"},
		{"1234567890.12345", "12345678901234567890.1H.5-beta"},
		{"20190126.230843", "12345678901234567890.1H.5-beta"},
		// MNG-6964
		{"1-0.alpha", "1"},
		{"1-0.beta", "1"},
		{"1-0.alpha", "1-0.beta"},
	} {
		checkOrder(t, p[0], p[1])
	}
}

func TestMavenLeadingZeroes(t *testing.T) {
	for n := 1; n <= 19; n++ {
		checkEqual(t, strings.Repeat("0", n-1)+"1", "1")
		checkEqual(t, strings.Repeat("0", n), "0")
	}
}

func TestMavenMng7644(t *testing.T) {
	for _, x := range []string{"abc", "alpha", "a", "beta", "b", "def", "milestone", "m", "RC"} {
		checkOrder(t, "1.0.0."+x+"1", "1.0.0-"+x+"2")
		checkEqual(t, "2-"+x, "2.0."+x)
		checkEqual(t, "2-"+x, "2.0.0."+x)
		checkEqual(t, "2.0."+x, "2.0.0."+x)
	}
}

// The story's headline order, and other cases checked against maven-artifact 3.9.11.
func TestOrdering(t *testing.T) {
	checkOrder(t, "1", "1.1")
	checkOrder(t, "1-snapshot", "1", "1-sp")
	checkOrder(t, "1-foo2", "1-foo10")
	checkOrder(t, "1.0-alpha1", "1.0-beta1", "1.0-milestone1", "1.0-rc1", "1.0-SNAPSHOT", "1.0", "1.0-sp1", "1.0-jre")
	checkOrder(t, "1.0-alpha-1", "1.0-beta", "1.0-rc1", "1.0-SNAPSHOT", "1.0", "1.0-sp1", "1.0-jre")
	checkOrder(t, "33.8.0-android", "33.8.0-jre")
	checkOrder(t, "2.0.20", "2.1.0-alpha0", "2.1.0-alpha1")
	// Numbers of any length, and Maven's Int/Long/BigInteger kinds of zero.
	checkOrder(t, "1.18446744073709551615", "1.18446744073709551616", "1.99999999999999999999999")
	checkOrder(t, "0.1", "0000000000.1", "0000000000000000000.1")
	// Unicode decimal digits are digits, as in Java's Character.isDigit.
	checkEqual(t, "1.\u0663", "1.3")
	// Unknown qualifiers compare by UTF-16 code units, like String.compareTo.
	checkOrder(t, "1-\U0001F600", "1-\uffff") // 0xD83D < 0xFFFF
	for _, e := range [][2]string{{"1.0", "1-ga"}, {"1.0", "1-final"}, {"1.0", "1.0.0"}, {"1.0-rc1", "1.0-cr1"}, {"1.0-a1", "1.0-alpha-1"}} {
		checkEqual(t, e[0], e[1])
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // G304: fixed testdata names.
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// TestRealWorldSets sorts the Guava and slf4j version lists as Maven does;
// see testdata/NOTICE for how the expected order was produced.
func TestRealWorldSets(t *testing.T) {
	for _, f := range []string{"testdata/guava.sorted.txt", "testdata/slf4j-api.sorted.txt"} {
		t.Run(f, func(t *testing.T) {
			want := readLines(t, f)
			if len(want) < 100 {
				t.Fatalf("%s has %d versions", f, len(want))
			}
			got := slices.Clone(want)
			slices.Reverse(got)
			slices.SortFunc(got, func(a, b string) int {
				if c := Compare(mustParse(t, a), mustParse(t, b)); c != 0 {
					return c
				}
				return strings.Compare(a, b)
			})
			if !slices.Equal(got, want) {
				for i := range got {
					if got[i] != want[i] {
						t.Fatalf("position %d: got %s, want %s", i, got[i], want[i])
					}
				}
			}
		})
	}
}

func TestParse(t *testing.T) {
	for in, want := range map[string]string{
		"1.0":          "1.0",
		" 1.0-RC1\n":   "1.0-RC1",
		"r09":          "r09",
		"1.0-SNAPSHOT": "1.0-SNAPSHOT",
		"latest":       "latest",
		"-":            "-",
		"é.1":          "é.1",
	} {
		v := mustParse(t, in)
		if v.String() != want || v.Scheme() != Name {
			t.Errorf("Parse(%q) = %q (%s), want %q", in, v.String(), v.Scheme(), want)
		}
	}
	for _, in := range []string{"", "   ", "1.0 rc1", "1.0\trc1", "1.0\x00", "1.0\x7f", "1.0\u0085x", "\xff1.0", strings.Repeat("1", MaxLen+1)} {
		_, err := Parse(in)
		var pe *version.ParseError
		if !errors.Is(err, version.ErrInvalid) || !errors.As(err, &pe) || pe.Scheme != Name {
			t.Errorf("Parse(%q) error = %v, want a maven ParseError", in, err)
		}
	}
	if _, err := Parse(strings.Repeat("1", MaxLen)); err != nil {
		t.Errorf("Parse of %d bytes: %v", MaxLen, err)
	}
}

func TestIsPrerelease(t *testing.T) {
	for in, want := range map[string]bool{
		"1.0-a1": true, "1.0.M2": true, "1.0-RC1": true, "1.0-SNAPSHOT": true, "1.0-alpha": true,
		"1.0-beta-2": true, "1.0-cr1": true, "1.0.0.RC1": true, "2.1.0-alpha0": true, "1.0-1-SNAPSHOT": true,
		"1.0-alpha-1-SNAPSHOT": true, "SNAPSHOT": true,
		"1.0": false, "1.0-sp1": false, "33.8.0-jre": false, "33.8.0-android": false, "r09": false,
		"1.0-1": false, "1.0.Final": false, "1-ga": false, "1.0-RELEASE": false, "6.1H.5-beta": false,
	} {
		if got := mustParse(t, in).IsPrerelease(); got != want {
			t.Errorf("IsPrerelease(%q) = %v, want %v", in, got, want)
		}
	}
	if (Version{}).IsPrerelease() {
		t.Error("zero Version is a pre-release")
	}
}

func TestBump(t *testing.T) {
	tests := []struct {
		prev, next string
		want       version.Bump
	}{
		{"1.2.3", "2.0", version.BumpMajor},
		{"1.2.3", "1.3", version.BumpMinor},
		{"1.2", "1.2.1", version.BumpPatch},
		{"1.2.3", "1.2.4", version.BumpPatch},
		{"1.2.3.4", "1.2.3.5", version.BumpPatch},
		{"1.2.3", "2.0-rc1", version.BumpMajor},
		{"1.2.3", "1.3.0-beta-1", version.BumpMinor},
		{"r09", "10.0", version.BumpMajor},
		{"1.0-alpha-1", "1.0-alpha-2", version.BumpPrerelease},
		{"1.0-alpha-2", "1.0-beta-1", version.BumpPrerelease},
		{"1.0", "1.0-1-SNAPSHOT", version.BumpPrerelease},
		{"1.0-rc1", "1.0", version.BumpMajor},
		{"2.0.0-RC1", "2.0.0", version.BumpMajor},
		{"2.1-rc1", "2.1", version.BumpMinor},
		{"2.1.0-M2", "2.1.0", version.BumpMinor},
		{"2.1.1-SNAPSHOT", "2.1.1", version.BumpPatch},
		{"1.0-rc1", "1.0-jre", version.BumpMajor},
		{"1.0", "1.0-sp1", version.BumpPatch},
		{"1.0-sp1", "1.0-sp2", version.BumpPatch},
		{"1.0", "1.0-1", version.BumpPatch},
		{"1.0-1", "1.0-2", version.BumpPatch},
		{"33.8.0-android", "33.8.0-jre", version.BumpOther},
		{"1.0", "1.0-jre", version.BumpOther},
		{"1.0-sp1", "1.0-jre", version.BumpOther},
		{"1.0", "1.0.0", version.BumpOther},
		{"1.0", "1-ga", version.BumpOther},
		// Backports (ADR 0007 §5): prev is the greatest known version below next.
		{"1.9.3", "1.9.4", version.BumpPatch},
		{"2.0", "1.9.4", version.BumpOther},
		{"1.0", "1.0-rc1", version.BumpOther},
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
	v := mustParse(t, "0.0.1-SNAPSHOT")
	junk, _ := lexical.Parse("nightly build")
	junk2, _ := lexical.Parse("stable")
	if s.Compare(v, junk) != 1 || s.Compare(junk, v) != -1 || s.Compare(junk, junk2) != -1 || s.Compare(nil, nil) != 0 {
		t.Error("foreign versions must rank below Maven versions and order lexically")
	}
	if s.Bump(junk, v) != version.BumpOther || s.Bump(v, junk) != version.BumpOther {
		t.Error("Bump with a foreign version should be other")
	}
	if !s.IsPrerelease(v) || !s.IsPrerelease(junk) || s.IsPrerelease(junk2) {
		t.Error("IsPrerelease")
	}
	if _, err := s.Parse("french toast"); !errors.Is(err, version.ErrInvalid) {
		t.Errorf("Scheme.Parse error = %v", err)
	}
	if Compare(Version{}, v) != -1 || Compare(v, Version{}) != 1 || Compare(Version{}, Version{}) != 0 {
		t.Error("zero Version ordering")
	}
}

var fuzzSeeds = []string{
	"1-alpha2snapshot", "1.0-SNAPSHOT", "1.0", "1-sp", "1-0.alpha", "2.0.0.RC1", "6.1H.5-beta",
	"12345678901234567890.1H.5-beta", "0000000000.1", "33.8.0-jre", "r09", "1.\u0663", "-", ".", "1--1", "a1b2c3",
}

func FuzzParse(f *testing.F) {
	for _, s := range fuzzSeeds {
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
		w, err := Parse(v.String())
		if err != nil || w.String() != v.String() || Compare(v, w) != 0 || Compare(v, v) != 0 {
			t.Fatalf("%q: String() %q does not round-trip: %v", raw, v.String(), err)
		}
		_ = v.IsPrerelease()
		if Diff(v, v) != version.BumpOther {
			t.Fatalf("Diff(%q, itself) is not other", raw)
		}
	})
}

// irregular reports whether a list of l starts with a sub-list, as
// "1-0.alpha" ([1, [[alpha]]]) and "-1" ([[1]]) do once Maven has removed
// their zeros, or holds a qualifier after its first item, as "1.A."
// ([1, a]) and "1-a.b.c" ([1, [a, b, [c]]]) do. Maven compares a list
// with a qualifier at the same position by type, which breaks transitivity.
func irregular(l *item) bool {
	for i, it := range l.list {
		switch {
		case it.kind == kindList && (i == 0 || irregular(it)):
			return true
		case it.kind == kindString && i > 0:
			return true
		}
	}
	return false
}

// transitiveDomain reports whether the versions are outside the set where
// Maven's ordering is not transitive: Maven versions with an irregular list.
func transitiveDomain(vs ...version.Version) bool {
	for _, v := range vs {
		if mv, ok := v.(Version); ok && irregular(mv.items) {
			return false
		}
	}
	return true
}

// TestMavenIsNotTransitive records that ComparableVersion 3.9.16 is not
// transitive for irregular versions (checked against
// maven-artifact): the port keeps Maven's results rather than repairing them.
func TestMavenIsNotTransitive(t *testing.T) {
	for _, c := range [][3]string{{"1-0.alpha", "1", "1-sp"}, {"0", "A", "0A0"}, {"1.rC", "1", "1.A."}} {
		checkOrder(t, c[0], c[1])
		checkOrder(t, c[1], c[2])
		checkOrder(t, c[2], c[0])
		if transitiveDomain(mustParse(t, c[0]), mustParse(t, c[1]), mustParse(t, c[2])) {
			t.Errorf("%v is in the transitive domain", c)
		}
	}
	for _, s := range []string{"1.0-rc1", "1-alpha2snapshot", "33.8.0-jre", "r09", "1.0-1-SNAPSHOT", "1-0", "1.0-RC.1", "2.0.0.RC1"} {
		if irregular(mustParse(t, s).items) {
			t.Errorf("%s is irregular", s)
		}
	}
}

// FuzzCompare checks that Compare, through the Scheme with foreign (lexical)
// versions mixed in, is antisymmetric on random pairs and transitive on
// random triples outside the inputs where Maven itself is not.
func FuzzCompare(f *testing.F) {
	f.Add("1.0-rc1", "1.0", "1.0-jre")
	f.Add("1-0.alpha", "1-0.beta", "1")
	f.Add("6.1.0rc3", "6.1.0", "6.1H.5-beta")
	f.Add("1.0 x", "1.0", "latest build")
	s := Scheme{}
	parse := func(raw string) (version.Version, bool) {
		if v, err := s.Parse(raw); err == nil {
			return v, true
		}
		v, err := lexical.Parse(raw)
		return v, err == nil
	}
	f.Fuzz(func(t *testing.T, a, b, c string) {
		va, okA := parse(a)
		vb, okB := parse(b)
		vc, okC := parse(c)
		if !okA || !okB || !okC {
			return
		}
		ab, bc, ac := s.Compare(va, vb), s.Compare(vb, vc), s.Compare(va, vc)
		if ab != -s.Compare(vb, va) || ab < -1 || ab > 1 {
			t.Fatalf("Compare(%q, %q) not antisymmetric", a, b)
		}
		if !transitiveDomain(va, vb, vc) {
			return // Maven's own order is not transitive there; see TestMavenIsNotTransitive
		}
		if ab <= 0 && bc <= 0 && ac > 0 {
			t.Fatalf("%q <= %q <= %q but Compare(a, c) = %d", a, b, c, ac)
		}
		if ab >= 0 && bc >= 0 && ac < 0 {
			t.Fatalf("%q >= %q >= %q but Compare(a, c) = %d", a, b, c, ac)
		}
		if bump := s.Bump(va, vb); ab >= 0 && bump != version.BumpOther {
			t.Fatalf("Bump(%q, %q) = %s for a non-upgrade", a, b, bump)
		}
	})
}
