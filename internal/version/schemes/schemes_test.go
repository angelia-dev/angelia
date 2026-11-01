package schemes

import (
	"bytes"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/version"
	"github.com/angelia-dev/angelia/internal/version/lexical"
	"github.com/angelia-dev/angelia/internal/version/pep440"
	"github.com/angelia-dev/angelia/internal/version/semver"
)

func TestLookup(t *testing.T) {
	for _, name := range Names() {
		s, err := Lookup(name)
		if err != nil || s.Name() != name {
			t.Errorf("Lookup(%q) = %v, %v", name, s, err)
		}
	}
	if _, err := Lookup("maven"); err == nil || !strings.Contains(err.Error(), "maven") {
		t.Errorf("Lookup(maven) error = %v", err)
	}
}

func TestForSourceKind(t *testing.T) {
	tests := map[string]string{
		"pypi":           pep440.Name,
		"npm":            semver.Name,
		"crates":         semver.Name,
		"go-proxy":       semver.Name,
		"github-release": semver.Name,
		"github-tag":     semver.Name,
		"gitlab-release": semver.Name,
		"oci":            semver.Name,
		"maven":          lexical.Name, // own scheme in Phase 1
		"http-json":      lexical.Name,
		"http-regex":     lexical.Name,
		"":               lexical.Name,
		"unknown":        lexical.Name,
	}
	for kind, want := range tests {
		if got := ForSourceKind(kind).Name(); got != want {
			t.Errorf("ForSourceKind(%q) = %s, want %s", kind, got, want)
		}
	}
}

func TestSelect(t *testing.T) {
	tests := []struct {
		kind, override, want string
		wantErr              bool
	}{
		{"pypi", "", pep440.Name, false},
		{"npm", "", semver.Name, false},
		{"github-release", "pep440", pep440.Name, false}, // per-watch override
		{"pypi", "lexical", lexical.Name, false},
		{"http-json", "semver", semver.Name, false},
		{"npm", "calver", "", true},
	}
	for _, tt := range tests {
		s, err := Select(tt.kind, tt.override, nil)
		if tt.wantErr {
			if err == nil {
				t.Errorf("Select(%q, %q) should fail", tt.kind, tt.override)
			}
			continue
		}
		if err != nil || s.Name() != tt.want {
			t.Errorf("Select(%q, %q) = %v, %v; want %s", tt.kind, tt.override, s, err, tt.want)
		}
	}
}

func TestFallbackWarnsAndNeverFails(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	tests := []struct {
		scheme, raw, wantScheme string
	}{
		{"semver", "v1.2.3", semver.Name},
		{"semver", "1.2", lexical.Name},
		{"semver", "latest", lexical.Name},
		{"pep440", "1.0rc1", pep440.Name},
		{"pep440", "french toast", lexical.Name},
		{"pep440", "1.0+İ", lexical.Name},
		{"semver", "\x00\xff", lexical.Name},
	}
	for _, tt := range tests {
		buf.Reset()
		s, err := Select("", tt.scheme, logger)
		if err != nil {
			t.Fatal(err)
		}
		v, err := s.Parse(tt.raw)
		if err != nil {
			t.Fatalf("%s.Parse(%q): %v", tt.scheme, tt.raw, err)
		}
		if v.Scheme() != tt.wantScheme {
			t.Errorf("%s.Parse(%q) scheme = %s, want %s", tt.scheme, tt.raw, v.Scheme(), tt.wantScheme)
		}
		warned := strings.Contains(buf.String(), "level=WARN")
		if warned != (tt.wantScheme == lexical.Name) {
			t.Errorf("%s.Parse(%q): warning logged = %v, log %q", tt.scheme, tt.raw, warned, buf.String())
		}
		if warned && !strings.Contains(buf.String(), "scheme="+tt.scheme) {
			t.Errorf("warning does not name the scheme: %q", buf.String())
		}
	}
}

func TestFallbackBlankIsAnError(t *testing.T) {
	s := WithFallback(semver.Scheme{}, nil)
	_, err := s.Parse("   ")
	if !errors.Is(err, version.ErrInvalid) {
		t.Fatalf("Parse(blank) error = %v, want ErrInvalid", err)
	}
	if WithFallback(lexical.Scheme{}, nil) != (lexical.Scheme{}) {
		t.Error("WithFallback(lexical) should return lexical unchanged")
	}
}

// A watch whose upstream mixes valid and invalid versions still gets a
// total order: parsed versions above the fallback ones.
func TestMixedOrdering(t *testing.T) {
	s, err := Select("npm", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := []string{"2.0.0", "latest", "1.10.0", "v1.9.0", "1.2", "2.0.0-rc.1", "build-10", "build-9"}
	want := []string{"1.2", "build-9", "build-10", "latest", "1.9.0", "1.10.0", "2.0.0-rc.1", "2.0.0"}
	vs := make([]version.Version, 0, len(raw))
	for _, r := range raw {
		v, err := s.Parse(r)
		if err != nil {
			t.Fatal(err)
		}
		vs = append(vs, v)
	}
	slices.SortFunc(vs, s.Compare)
	got := make([]string, len(vs))
	for i, v := range vs {
		got[i] = v.String()
	}
	if !slices.Equal(got, want) {
		t.Errorf("sorted = %v, want %v", got, want)
	}
	if !s.IsPrerelease(vs[6]) || s.IsPrerelease(vs[7]) {
		t.Error("IsPrerelease through the fallback wrapper")
	}
	if s.Bump(vs[5], vs[7]) != version.BumpMajor || s.Bump(vs[3], vs[7]) != version.BumpOther {
		t.Error("Bump through the fallback wrapper")
	}
}

// FuzzFallbackParse checks that the selected schemes never panic and never
// fail on a non-blank string.
func FuzzFallbackParse(f *testing.F) {
	for _, s := range []string{"1.2.3", "v1.0.0-rc.1", "1.0.post1", "latest", "1!2.0", "", "\xff"} {
		f.Add(s)
	}
	schemes := []version.Scheme{WithFallback(semver.Scheme{}, nil), WithFallback(pep440.Scheme{}, nil)}
	f.Fuzz(func(t *testing.T, raw string) {
		for _, s := range schemes {
			v, err := s.Parse(raw)
			if err != nil {
				if strings.TrimSpace(raw) != "" {
					t.Fatalf("%s.Parse(%q): %v", s.Name(), raw, err)
				}
				continue
			}
			if s.Compare(v, v) != 0 {
				t.Fatalf("%s: %q does not compare equal to itself", s.Name(), raw)
			}
			_ = s.IsPrerelease(v)
			_ = s.Bump(v, v)
		}
	})
}
