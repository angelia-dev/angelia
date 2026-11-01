package main

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/version"
	"github.com/angelia-dev/angelia/internal/version/lexical"
	"github.com/angelia-dev/angelia/internal/version/schemes"
)

func selectScheme(t *testing.T, kind string) version.Scheme {
	t.Helper()
	s, err := schemes.Select(kind, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestComparePackageAgrees(t *testing.T) {
	tests := []struct {
		registry string
		pkg      Package
	}{
		{"pypi", Package{
			Name: "demo",
			Order: [][]string{
				{"1.0.dev1"},
				{"1.0a1"},
				{"1.0b2"},
				{"1.0rc1"},
				{"1.0", "1.0.0"},
				{"1.0+local"},
				{"1.0.post1"},
				{"1.1"},
				{"1!0.1"},
			},
			Invalid: []string{"0.1-bulbasaur", "1.0beta5prerelease"},
		}},
		{"npm", Package{
			Name: "@scope/demo",
			Order: append(groups("0.0.0-experimental-abc", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta",
				"1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1"),
				[]string{"1.0.0", "1.0.0+build.1"}, []string{"10.0.0"}),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.registry, func(t *testing.T) {
			res := ComparePackage(tt.registry, selectScheme(t, tt.registry), tt.pkg)
			if len(res.Mismatches) != 0 || res.Expected != nil {
				t.Fatalf("want no mismatch, got %+v", res)
			}
			want := len(tt.pkg.Invalid)
			for _, g := range tt.pkg.Order {
				want += len(g)
			}
			if res.Versions != want {
				t.Errorf("Versions = %d, want %d", res.Versions, want)
			}
		})
	}
}

// AC "Report lists every mismatch with package, expected and actual order".
func TestComparePackageOrderMismatch(t *testing.T) {
	// Lexical order puts "1.0.0" before its pre-release; the registry does not.
	p := Package{Name: "demo", Order: groups("0.9.0", "1.0.0-rc.1", "1.0.0", "2.0.0")}
	res := ComparePackage("npm", lexical.Scheme{}, p)

	want := []Mismatch{{
		Registry: "npm", Package: "demo", Kind: KindOrder,
		Versions: []string{"1.0.0-rc.1", "1.0.0"}, Expected: "<", Actual: ">",
	}}
	if !reflect.DeepEqual(res.Mismatches, want) {
		t.Errorf("Mismatches = %+v, want %+v", res.Mismatches, want)
	}
	if exp := []string{"0.9.0", "1.0.0-rc.1", "1.0.0", "2.0.0"}; !slices.Equal(res.Expected, exp) {
		t.Errorf("Expected = %q, want %q", res.Expected, exp)
	}
	if act := []string{"0.9.0", "1.0.0", "1.0.0-rc.1", "2.0.0"}; !slices.Equal(res.Actual, act) {
		t.Errorf("Actual = %q, want %q", res.Actual, act)
	}
}

func TestComparePackageTieMismatch(t *testing.T) {
	// The registry says 1.0 == 1.0.0; lexical order does not.
	p := Package{Name: "demo", Order: [][]string{{"1.0", "1.0.0"}}}
	res := ComparePackage("pypi", lexical.Scheme{}, p)
	if len(res.Mismatches) != 1 {
		t.Fatalf("want 1 mismatch, got %+v", res.Mismatches)
	}
	m := res.Mismatches[0]
	if m.Kind != KindOrder || m.Expected != "=" || m.Actual != "<" {
		t.Errorf("got %+v, want order = vs <", m)
	}
	if !slices.Equal(res.Expected, []string{"1.0 = 1.0.0"}) || !slices.Equal(res.Actual, []string{"1.0", "1.0.0"}) {
		t.Errorf("Expected %q, Actual %q", res.Expected, res.Actual)
	}
}

func TestComparePackageRejectedAndAccepted(t *testing.T) {
	// Strict SemVer rejects "1.0" (registry-valid here by construction) and
	// accepts "2.0.0" (registry-invalid here by construction).
	p := Package{Name: "demo", Order: groups("1.0", "1.0.0", "1.1.0"), Invalid: []string{"2.0.0", "junk"}}
	res := ComparePackage("npm", selectScheme(t, "npm"), p)
	var got []string
	for _, m := range res.Mismatches {
		got = append(got, string(m.Kind)+":"+strings.Join(m.Versions, ","))
	}
	want := []string{"accepted:2.0.0", "rejected:1.0"}
	if !slices.Equal(got, want) {
		t.Errorf("mismatches = %q, want %q", got, want)
	}
	if res.Expected != nil {
		t.Errorf("order of the versions both accept should agree, got %q vs %q", res.Expected, res.Actual)
	}
}

// cyclic orders "a" < "b" < "c" < "a": every adjacent pair of a..c agrees,
// but no sort can reproduce the registry's order reliably.
type cyclic struct{ lexical.Scheme }

func (cyclic) Compare(a, b version.Version) int {
	switch x, y := a.String(), b.String(); {
	case x == "c" && y == "a":
		return -1
	case x == "a" && y == "c":
		return 1
	default:
		return strings.Compare(x, y)
	}
}

func TestComparePackageNonTransitive(t *testing.T) {
	// Find a package name whose shuffle exposes the cycle; the shuffle is
	// deterministic, so this is stable.
	for _, name := range []string{"p0", "p1", "p2", "p3", "p4", "p5", "p6", "p7"} {
		res := ComparePackage("npm", cyclic{}, Package{Name: name, Order: groups("a", "b", "c")})
		if res.Expected == nil {
			continue
		}
		if len(res.Mismatches) != 1 || res.Mismatches[0].Kind != KindOrder {
			t.Fatalf("want one order mismatch, got %+v", res.Mismatches)
		}
		return
	}
	t.Fatal("no shuffle exposed the non-transitive comparator")
}

func TestComparePackageDeterministic(t *testing.T) {
	p := Package{Name: "demo", Order: groups("a", "b", "c", "d", "e")}
	a := ComparePackage("npm", cyclic{}, p)
	b := ComparePackage("npm", cyclic{}, p)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("two runs differ: %+v vs %+v", a, b)
	}
}

func TestFirstDiff(t *testing.T) {
	if got := firstDiff([]string{"a", "b"}, []string{"a", "c"}); got != 1 {
		t.Errorf("got %d, want 1", got)
	}
	if got := firstDiff([]string{"a"}, []string{"a", "b"}); got != 0 {
		t.Errorf("prefix: got %d, want 0", got)
	}
}
