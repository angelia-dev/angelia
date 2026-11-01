// Package lexical is the fallback version scheme: any non-blank string is a
// version, ordered by natural string order (version.Natural), so "1.10"
// sorts after "1.9". It is used for sources without a known scheme and for
// versions that the watch's scheme cannot parse.
package lexical

import (
	"strings"
	"unicode"

	"github.com/angelia-dev/angelia/internal/version"
)

// Name is the scheme's identifier.
const Name = "lexical"

// Version is a version string, trimmed of surrounding white space.
type Version struct {
	s string
}

// String returns the trimmed version string.
func (v Version) String() string { return v.s }

// Scheme returns Name.
func (Version) Scheme() string { return Name }

// Parse accepts any string that is not empty or only white space.
func Parse(raw string) (Version, error) {
	s := strings.TrimFunc(raw, unicode.IsSpace)
	if s == "" {
		return Version{}, &version.ParseError{Scheme: Name, Raw: raw, Reason: "empty"}
	}
	return Version{s: s}, nil
}

// Scheme implements version.Scheme for lexical versions. The zero value is ready to use.
type Scheme struct{}

var _ version.Scheme = Scheme{}

// Name returns "lexical".
func (Scheme) Name() string { return Name }

// Parse implements version.Scheme.
func (Scheme) Parse(raw string) (version.Version, error) {
	v, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// Compare orders any two versions by version.Natural on their String form;
// a nil version sorts first.
func (Scheme) Compare(a, b version.Version) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	return version.Natural(a.String(), b.String())
}

// isPrereleaseWord reports whether a lower-case run of letters marks a
// pre-release.
func isPrereleaseWord(w string) bool {
	switch w {
	case "a", "alpha", "b", "beta", "c", "rc", "cr", "pre", "preview", "dev",
		"snapshot", "nightly", "canary", "next", "m", "milestone", "ea":
		return true
	}
	return false
}

// IsPrerelease is a heuristic: it reports whether a run of ASCII letters in
// v's string is a common pre-release marker such as "alpha", "beta", "rc",
// "dev", "snapshot" or "nightly" ("1.0-rc1", "2.0b3", "3.1-SNAPSHOT").
// A single letter counts only next to a digit ("2.0b3", "5.0-M1", not
// "debian-b").
func (Scheme) IsPrerelease(v version.Version) bool {
	if v == nil {
		return false
	}
	s := strings.ToLower(v.String())
	for i := 0; i < len(s); {
		if !isLetter(s[i]) {
			i++
			continue
		}
		j := i
		for j < len(s) && isLetter(s[j]) {
			j++
		}
		word := s[i:j]
		if isPrereleaseWord(word) {
			nearDigit := i > 0 && isDigit(s[i-1]) || j < len(s) && isDigit(s[j])
			if len(word) > 1 || nearDigit {
				return true
			}
		}
		i = j
	}
	return false
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// Bump always returns version.BumpOther: a free-form string carries no
// major/minor/patch structure.
func (Scheme) Bump(_, _ version.Version) version.Bump { return version.BumpOther }
