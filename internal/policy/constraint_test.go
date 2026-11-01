package policy

import (
	"errors"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/version"
	"github.com/angelia-dev/angelia/internal/version/calver"
	"github.com/angelia-dev/angelia/internal/version/lexical"
	"github.com/angelia-dev/angelia/internal/version/pep440"
	"github.com/angelia-dev/angelia/internal/version/schemes"
	"github.com/angelia-dev/angelia/internal/version/semver"
)

func TestParseConstraint(t *testing.T) {
	for in, want := range map[string]string{
		"":                    "",
		"   ":                 "",
		">=2 <3":              ">=2 <3",
		">=2,<3":              ">=2 <3",
		">= 2 , < 3":          ">=2 <3",
		"1.4.2":               "==1.4.2",
		"=1.4.2":              "==1.4.2",
		"!=1.5 >1 <=2":        "!=1.5 >1 <=2",
		">=1 <2 || >=3":       ">=1 <2 || >=3",
		"\t>=1.0rc1\t":        ">=1.0rc1",
		"<2026.10 || ==24.04": "<2026.10 || ==24.04",
	} {
		c, err := ParseConstraint(in)
		if err != nil {
			t.Errorf("ParseConstraint(%q): %v", in, err)
			continue
		}
		if got := c.String(); got != want {
			t.Errorf("ParseConstraint(%q) = %q, want %q", in, got, want)
		}
		if c.IsZero() != (want == "") {
			t.Errorf("ParseConstraint(%q).IsZero() = %v", in, c.IsZero())
		}
	}
}

func TestParseConstraintInvalid(t *testing.T) {
	for _, in := range []string{
		">=", ">=2 <", ">=2 ||", "|| >=2", ">=2 || || <1", ">>2", ">=<2", "=>2", "1|2",
		">=1\n<2", strings.Repeat("x", MaxConstraintLen+1),
		strings.TrimSpace(strings.Repeat(">=1 ", MaxComparators+1)),
	} {
		if _, err := ParseConstraint(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseConstraint(%q) = %v, want ErrInvalid", in, err)
		}
	}
	if _, err := ParseConstraint(strings.TrimSpace(strings.Repeat(">=1 ", MaxComparators))); err != nil {
		t.Errorf("%d comparators rejected: %v", MaxComparators, err)
	}
}

func TestRangeMatch(t *testing.T) {
	semverFB := schemes.WithFallback(semver.Scheme{}, nil)
	for _, tc := range []struct {
		scheme     version.Scheme
		constraint string
		in, out    []string
	}{
		{semverFB, ">=2 <3", []string{"2.0.0", "2.9.9", "v2.1.0", "3.0.0-rc.1"}, []string{"1.9.9", "3.0.0", "10.0.0", "2.0.0-rc.1", "latest", "1.2.3.4"}},
		{semverFB, ">2.0.0", []string{"2.0.1"}, []string{"2.0.0", "2.0.0+build"}},
		{semverFB, "!=2.0.0 >=1", []string{"1.0.0", "2.0.1"}, []string{"2.0.0", "2.0.0+meta", "0.9.0"}},
		{semverFB, "<=1.2", []string{"1.2.0", "0.1.0"}, []string{"1.2.1"}},
		{semverFB, "1.2.3", []string{"1.2.3", "v1.2.3"}, []string{"1.2.4"}},
		{semverFB, "<1 || >=2 <3", []string{"0.5.0", "2.5.0"}, []string{"1.0.0", "3.0.0"}},
		{schemes.WithFallback(semver.Scheme{Coerce: true}, nil), ">=3.12", []string{"3.12", "v4", "3.12.1"}, []string{"3.11", "latest"}},
		{schemes.WithFallback(pep440.Scheme{}, nil), ">=2,<3", []string{"2", "2.0.0", "2.31.0", "3.0rc1", "2.0.post1"}, []string{"1.9", "3.0", "3", "2.0rc1"}},
		{schemes.WithFallback(pep440.Scheme{}, nil), "==1.0", []string{"1.0", "1.0.0", "1"}, []string{"1.0.1"}},
		{schemes.WithFallback(calver.Scheme{}, nil), ">=2026 <2027", []string{"2026.1", "2026.10.15", "v2026.3"}, []string{"2025.12", "2027.1", "24.04"}},
		{schemes.WithFallback(calver.Scheme{}, nil), ">=24.04", []string{"24.04", "24.10", "2026.1"}, []string{"23.10"}},
		{lexical.Scheme{}, ">=1.9 <1.11", []string{"1.9", "1.10"}, []string{"1.8", "1.11", "1.100"}},
		{semverFB, "", []string{"1.0.0", "latest"}, nil},
	} {
		c, err := ParseConstraint(tc.constraint)
		if err != nil {
			t.Fatal(err)
		}
		r, err := c.Bind(tc.scheme)
		if err != nil {
			t.Fatalf("%s Bind(%q): %v", tc.scheme.Name(), tc.constraint, err)
		}
		for _, raws := range []struct {
			raws []string
			want bool
		}{{tc.in, true}, {tc.out, false}} {
			for _, raw := range raws.raws {
				v, err := tc.scheme.Parse(raw)
				if err != nil {
					t.Fatal(err)
				}
				if got := r.Match(v); got != raws.want {
					t.Errorf("%s %q matches %q = %v, want %v", tc.scheme.Name(), tc.constraint, raw, got, raws.want)
				}
			}
		}
	}
	var zero Range
	if !zero.Match(nil) {
		t.Error("zero Range does not match")
	}
	c, _ := ParseConstraint(">=1")
	r, _ := c.Bind(semver.Scheme{})
	if r.Match(nil) {
		t.Error("nil version matched")
	}
}

func TestBindInvalidOperand(t *testing.T) {
	for _, tc := range []struct {
		scheme     version.Scheme
		constraint string
	}{
		{schemes.WithFallback(semver.Scheme{}, nil), ">=latest"},
		{schemes.WithFallback(semver.Scheme{}, nil), ">=1.2.3.4.5"},
		{schemes.WithFallback(pep440.Scheme{}, nil), "<abc"},
		{schemes.WithFallback(calver.Scheme{}, nil), ">=1.2.3"},
		{semver.Scheme{}, ">=x"},
	} {
		c, err := ParseConstraint(tc.constraint)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Bind(tc.scheme); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s Bind(%q) = %v, want ErrInvalid", tc.scheme.Name(), tc.constraint, err)
		}
	}
}
