package lexical

import (
	"errors"
	"testing"

	"github.com/angelia-dev/angelia/internal/version"
)

func TestParse(t *testing.T) {
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"latest", "latest", true},
		{"  2024-01-15 \n", "2024-01-15", true},
		{"1.0", "1.0", true},
		{"ünïcode", "ünïcode", true},
		{"", "", false},
		{" \t\n", "", false},
	}
	for _, tt := range tests {
		v, err := Parse(tt.in)
		if tt.ok != (err == nil) {
			t.Errorf("Parse(%q) error = %v, want ok=%v", tt.in, err, tt.ok)
			continue
		}
		if !tt.ok {
			if !errors.Is(err, version.ErrInvalid) {
				t.Errorf("Parse(%q) error %v does not wrap ErrInvalid", tt.in, err)
			}
			if _, err := (Scheme{}).Parse(tt.in); err == nil {
				t.Errorf("Scheme.Parse(%q) should fail", tt.in)
			}
			continue
		}
		if v.String() != tt.want || v.Scheme() != Name {
			t.Errorf("Parse(%q) = %q (%s), want %q", tt.in, v.String(), v.Scheme(), tt.want)
		}
	}
}

func TestCompare(t *testing.T) {
	ordered := []string{"1.0", "1.0-rc1", "1.0.1", "1.2", "1.10", "2.0", "10.0", "build-7", "build-10", "latest", "stable"}
	s := Scheme{}
	if s.Name() != Name {
		t.Fatalf("Name() = %q", s.Name())
	}
	vs := make([]version.Version, len(ordered))
	for i, o := range ordered {
		v, err := s.Parse(o)
		if err != nil {
			t.Fatal(err)
		}
		vs[i] = v
	}
	for i := range vs {
		for j := range vs {
			want := 0
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			if got := s.Compare(vs[i], vs[j]); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", vs[i], vs[j], got, want)
			}
		}
	}
	if s.Compare(nil, vs[0]) != -1 || s.Compare(vs[0], nil) != 1 || s.Compare(nil, nil) != 0 {
		t.Error("nil must sort first")
	}
}

func TestIsPrerelease(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"1.0", false},
		{"1.0-rc1", true},
		{"1.0.0-RC.1", true},
		{"2.0b3", true},
		{"2.0a1", true},
		{"3.1-SNAPSHOT", true},
		{"nightly-2024-01-01", true},
		{"1.0.0-alpha", true},
		{"1.0.0-beta.2", true},
		{"5.0.0-M1", true},
		{"1.0.dev4", true},
		{"20.04-canary", true},
		{"latest", false},
		{"stable", false},
		{"debian-b", false},  // a lone letter must follow a digit
		{"release-1", false}, // "release" is not "rc"
		{"1.0-final", false},
		{"bookworm", false},
		// Known false positives of the heuristic, pinned so a change is deliberate.
		{"latest-next", true},
		{"1a2b", true},
	}
	s := Scheme{}
	for _, tt := range tests {
		v, err := s.Parse(tt.in)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.IsPrerelease(v); got != tt.want {
			t.Errorf("IsPrerelease(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
	if s.IsPrerelease(nil) {
		t.Error("IsPrerelease(nil) = true")
	}
}

func TestBumpIsAlwaysOther(t *testing.T) {
	s := Scheme{}
	a, _ := s.Parse("1.0")
	b, _ := s.Parse("2.0")
	if s.Bump(a, b) != version.BumpOther || s.Bump(nil, nil) != version.BumpOther {
		t.Error("lexical Bump must be other")
	}
}
