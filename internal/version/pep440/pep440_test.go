package pep440

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/version"
	"github.com/angelia-dev/angelia/internal/version/lexical"
)

// packagingVectors are the test vectors of the Python packaging library;
// see testdata/NOTICE for their origin and licence.
type packagingVectors struct {
	Ordered      []string    `json:"ordered"`
	Invalid      []string    `json:"invalid"`
	Normalized   [][2]string `json:"normalized"`
	Str          [][2]string `json:"str"`
	IsPrerelease [][2]any    `json:"is_prerelease"`
}

func loadVectors(t *testing.T) packagingVectors {
	t.Helper()
	data, err := os.ReadFile("testdata/packaging_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v packagingVectors
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Ordered) == 0 || len(v.Invalid) == 0 || len(v.Normalized) == 0 || len(v.Str) == 0 || len(v.IsPrerelease) == 0 {
		t.Fatal("testdata/packaging_vectors.json is incomplete")
	}
	return v
}

func mustParse(t *testing.T, s string) Version {
	t.Helper()
	v, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return v
}

func sign(c int) int {
	switch {
	case c < 0:
		return -1
	case c > 0:
		return 1
	}
	return 0
}

// TestPackagingOrdering ports test_comparison_true/false: every pair of the
// VERSIONS list, which "must be in the correct sorting order".
func TestPackagingOrdering(t *testing.T) {
	vec := loadVectors(t)
	vs := make([]Version, len(vec.Ordered))
	for i, s := range vec.Ordered {
		vs[i] = mustParse(t, s)
	}
	for i := range vs {
		for j := range vs {
			want := sign(i - j)
			if got := Compare(vs[i], vs[j]); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", vec.Ordered[i], vec.Ordered[j], got, want)
			}
		}
	}
}

func TestPackagingInvalid(t *testing.T) {
	for _, in := range loadVectors(t).Invalid {
		if v, err := Parse(in); !errors.Is(err, version.ErrInvalid) {
			t.Errorf("Parse(%q) = %v, %v; want ErrInvalid", in, v, err)
		}
	}
}

func TestPackagingNormalized(t *testing.T) {
	vec := loadVectors(t)
	for _, tt := range append(vec.Normalized, vec.Str...) {
		if got := mustParse(t, tt[0]).String(); got != tt[1] {
			t.Errorf("Parse(%q).String() = %q, want %q", tt[0], got, tt[1])
		}
	}
}

func TestPackagingIsPrerelease(t *testing.T) {
	for _, tt := range loadVectors(t).IsPrerelease {
		in, _ := tt[0].(string)
		want, _ := tt[1].(bool)
		if got := mustParse(t, in).IsPrerelease(); got != want {
			t.Errorf("IsPrerelease(%q) = %v, want %v", in, got, want)
		}
	}
}

// test_version_rc_and_c_equals.
func TestRCEqualsC(t *testing.T) {
	if Compare(mustParse(t, "1.0rc1"), mustParse(t, "1.0c1")) != 0 {
		t.Error("1.0rc1 != 1.0c1")
	}
}

func TestParseEdgeCases(t *testing.T) {
	tests := []struct{ in, want string }{
		// Epochs.
		{"1!1.0", "1!1.0"},
		{"0!1.0", "1.0"},
		{"00!1.2", "1.2"},
		{"2!0", "2!0"},
		// Release segment length and zeros are kept in String.
		{"1", "1"},
		{"1.0.0.0", "1.0.0.0"},
		{"01.02.003", "1.2.3"},
		// Pre-release spellings and separators.
		{"1.0alpha", "1.0a0"},
		{"1.0.preview1", "1.0rc1"},
		{"1.0pre", "1.0rc0"},
		{"1.0.c", "1.0rc0"},
		{"1.0-a1", "1.0a1"},
		{"1.0a.1", "1.0a1"},
		{"1.0_b_2", "1.0b2"},
		// Separator after a label without a number is part of the segment.
		{"1.0a.", "1.0a0"},
		{"1.0a..dev", "1.0a0.dev0"},
		{"1.0.post-", "1.0.post0"},
		// Post-releases.
		{"1.0-1", "1.0.post1"},
		{"1.0-r-1", "1.0.post1"},
		{"1.0_post_1", "1.0.post1"},
		{"1.0a1-2", "1.0a1.post2"},
		{"1.0a1post", "1.0a1.post0"},
		{"1.0rc1-1", "1.0rc1.post1"},
		// Dev-releases.
		{"1.0-1-dev", "1.0.post1.dev0"},
		{"1.0dev-1", "1.0.dev1"},
		{"1.0.0-1.dev", "1.0.0.post1.dev0"},
		// Full combination.
		{"V3!1.2.3-RC.4_POST.5-DEV.6+Ubuntu-1_A.007", "3!1.2.3rc4.post5.dev6+ubuntu.1.a.7"},
		// Local versions: lower-cased, separators to ".", numeric parts trimmed.
		{"1.2+01", "1.2+1"},
		{"1.2+0a", "1.2+0a"},
		{"1.2+a-B_c", "1.2+a.b.c"},
		// Python's str.strip() white space, including U+001C..U+001F.
		{"\x1c1.0\x1f", "1.0"},
		{"　1.0 ", "1.0"},
		// Numbers beyond 64 bits.
		{"18446744073709551616.0", "18446744073709551616.0"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			v := mustParse(t, tt.in)
			if got := v.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
			if v.Scheme() != Name {
				t.Errorf("Scheme() = %q", v.Scheme())
			}
		})
	}
}

func TestParseInvalidEdgeCases(t *testing.T) {
	tests := []string{
		"", "   ", "v", "V", "!1.0", "1!", "1!!1", "1.0.", "1..0", ".1", "1_0", "1.0-", "1.0+", "1.0+a_",
		"1.0++a", "1.0a1.2", "1.0..post1", "vv1.0", "1.0 rc1", "1.0+ab cd", "1.0+K", "1.0a1a2",
		"1.0.dev1.dev2", "1.0.post1.post2", "1.0.dev1a1", "1.0+abc+def", "latest", "1.0\x00",
	}
	for _, in := range tests {
		t.Run(in, func(t *testing.T) {
			_, err := Parse(in)
			if !errors.Is(err, version.ErrInvalid) {
				t.Fatalf("Parse(%q) error = %v, want ErrInvalid", in, err)
			}
			var pe *version.ParseError
			if !errors.As(err, &pe) || pe.Scheme != Name {
				t.Fatalf("error %v is not a pep440 ParseError", err)
			}
		})
	}
}

func TestOrderingEdgeCases(t *testing.T) {
	tests := []struct {
		name string
		list []string
	}{
		{"epoch beats release", []string{"2013.10", "2014.1", "1!0.1", "1!1.0", "2!0"}},
		{"numeric release", []string{"1.9", "1.10", "1.11", "2", "10"}},
		{"dev of post below post", []string{"1.0", "1.0.post1.dev1", "1.0.post1", "1.0.post2"}},
		{"phases", []string{"1.0.dev1", "1.0a1.dev1", "1.0a1", "1.0a1.post1", "1.0b1", "1.0rc1", "1.0", "1.0.post1", "1.0.1.dev1"}},
		{"local after public", []string{"1.0", "1.0+abc", "1.0+abc.1", "1.0+1", "1.0+2", "1.0+10"}},
		{"huge numbers", []string{"1.18446744073709551615", "1.18446744073709551616", "1.99999999999999999999999"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i := range tt.list {
				for j := range tt.list {
					a, b := mustParse(t, tt.list[i]), mustParse(t, tt.list[j])
					if got, want := Compare(a, b), sign(i-j); got != want {
						t.Errorf("Compare(%s, %s) = %d, want %d", a, b, got, want)
					}
				}
			}
		})
	}
	equal := [][2]string{
		{"1.0", "1.0.0"},
		{"1.0", "1.0.0.0.0"},
		{"1.0a", "1.0a0"},
		{"1.0+01", "1.0+1"},
		{"0!1.0", "1.0"},
		{"1.0-1", "1.0.post1"},
	}
	for _, e := range equal {
		if Compare(mustParse(t, e[0]), mustParse(t, e[1])) != 0 {
			t.Errorf("%s should equal %s", e[0], e[1])
		}
	}
}

func TestPublic(t *testing.T) {
	if got := mustParse(t, "1!1.0rc1.post2+Local.7").Public(); got != "1!1.0rc1.post2" {
		t.Errorf("Public() = %q", got)
	}
}

func TestBump(t *testing.T) {
	tests := []struct {
		prev, next string
		want       version.Bump
	}{
		{"1.2.3", "2.0", version.BumpMajor},
		{"1.0", "1!0.1", version.BumpMajor}, // epoch
		{"1.2.3", "1.3", version.BumpMinor},
		{"1.2", "1.2.1", version.BumpPatch},
		{"1.2.3", "1.2.4", version.BumpPatch},
		{"1.2.3.4", "1.2.3.5", version.BumpPatch}, // fourth segment and beyond count as patch
		{"2", "2.1", version.BumpMinor},
		{"1.2.3", "2.0rc1", version.BumpMajor},
		{"1.2.3", "1.3.0b1", version.BumpMinor},
		{"1.0a1", "1.0a2", version.BumpPrerelease},
		{"1.0a2", "1.0b1", version.BumpPrerelease},
		{"1.0.dev1", "1.0.dev2", version.BumpPrerelease},
		{"1.0", "1.0.post1.dev1", version.BumpPrerelease},
		{"1.0rc1", "1.0", version.BumpMajor},
		{"2.0.0rc1", "2.0.0", version.BumpMajor},
		{"2.1rc1", "2.1", version.BumpMinor},
		{"2.1.0rc1", "2.1.0", version.BumpMinor},
		{"2.1.1.dev1", "2.1.1", version.BumpPatch},
		{"1.0rc1.post1", "1.0", version.BumpMajor},
		{"1.0rc1", "1.0.post1", version.BumpMajor},
		{"1.0", "1.0.post1", version.BumpPatch},
		{"1.0.post1", "1.0.post2", version.BumpPatch},
		{"1.0.post1.dev1", "1.0.post1", version.BumpPatch},
		{"1.0", "1.0+local", version.BumpOther},
		{"1.0", "1.0.0", version.BumpOther},
		{"2.0", "1.9.4", version.BumpOther},
		{"1.0", "1.0rc1", version.BumpOther},
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
	v, _ := s.Parse("0.0.1.dev0")
	junk, _ := lexical.Parse("nightly")
	junk2, _ := lexical.Parse("stable")
	if s.Compare(v, junk) != 1 || s.Compare(junk, v) != -1 || s.Compare(junk, junk2) != -1 || s.Compare(nil, nil) != 0 {
		t.Error("foreign versions must rank below PEP 440 versions and order lexically")
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
}

func FuzzParse(f *testing.F) {
	vec := packagingVectors{}
	if data, err := os.ReadFile("testdata/packaging_vectors.json"); err == nil {
		_ = json.Unmarshal(data, &vec)
	}
	for _, s := range append(vec.Ordered, vec.Invalid...) {
		f.Add(s)
	}
	f.Add("V3!1.2.3-RC.4_POST.5-DEV.6+Ubuntu-1_A.007")
	f.Add("1.0a..dev")
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
		if strings.ContainsAny(s, "ABCDEFGHIJKLMNOPQRSTUVWXYZ_- ") {
			t.Fatalf("normalized form %q is not canonical", s)
		}
	})
}

func FuzzCompare(f *testing.F) {
	f.Add("1.0.dev0", "1.0a0")
	f.Add("1.0.post1.dev1", "1.0.post1")
	f.Add("1.2+1.abc", "1.2+abc.1")
	f.Add("1!1.0", "2.0")
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
