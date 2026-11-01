// Package version defines how Angelia parses, orders and classifies upstream
// versions. Each scheme lives in its own subpackage (semver, pep440, calver, lexical);
// package schemes picks one per source kind and adds the lexical fallback.
//
// Every function here is pure: no I/O, no shared state, safe for concurrent use.
package version

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Version is a parsed version. Each scheme has its own concrete type.
//
// Never compare two Versions with == or use them as map keys: concrete types
// hold slices, and equal precedence is not identity. Use Scheme.Compare to
// order them and String to display them. Compare returning 0 does not mean
// the same version ("1.0" and "1.0.0" in PEP 440, "1.0.0+a" and "1.0.0+b"
// in SemVer), so key known-version sets by the raw upstream string.
type Version interface {
	// String returns the normalized form, e.g. "1.0rc1" for the PEP 440 input "1.0-RC1".
	String() string
	// Scheme returns the name of the scheme that parsed the version.
	Scheme() string
}

// Scheme parses, orders and classifies the versions of one versioning scheme.
//
// Compare, IsPrerelease and Bump accept any Version, never panic, and keep a
// total order when a set mixes schemes (which happens when unparseable
// versions fall back to lexical): versions of the scheme itself rank above
// versions of any other scheme, and foreign versions are ordered among
// themselves with Natural on their String form.
type Scheme interface {
	// Name is the scheme's identifier as used in configuration, e.g. "pep440".
	Name() string
	// Parse parses raw. Errors wrap ErrInvalid.
	Parse(raw string) (Version, error)
	// Compare returns -1 if a < b, 0 if a and b have the same precedence, +1 if a > b.
	Compare(a, b Version) int
	// IsPrerelease reports whether v is a pre-release (alpha, beta, rc, dev, ...).
	IsPrerelease(v Version) bool
	// Bump classifies the change from prev to next. It returns BumpOther
	// unless next is strictly greater than prev and both belong to the scheme.
	//
	// prev must be the greatest known version below next, not the latest
	// known version: when 2.0.0 is known and the backport 1.9.4 appears, pass
	// prev = 1.9.3 to get BumpPatch (with prev = 2.0.0 the result is BumpOther).
	Bump(prev, next Version) Bump
}

// Bump is the kind of change between two versions, as used by the
// policy `bump: [major, minor]` filter and the event's bump field.
type Bump string

// Bump values.
const (
	BumpMajor      Bump = "major"
	BumpMinor      Bump = "minor"
	BumpPatch      Bump = "patch"
	BumpPrerelease Bump = "prerelease"
	BumpOther      Bump = "other"
)

// ErrInvalid is wrapped by every parse error.
var ErrInvalid = errors.New("invalid version")

// maxQuoted bounds how much of an invalid input is copied into an error.
const maxQuoted = 64

// ParseError describes why a raw string is not a valid version of a scheme.
type ParseError struct {
	Scheme string
	Raw    string
	Reason string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("%s: %q: %s: %v", e.Scheme, Truncate(e.Raw, maxQuoted), e.Reason, ErrInvalid)
}

// Truncate shortens s to at most n bytes, cut at a UTF-8 rune boundary,
// and appends "..." when it cut anything. Use it before logging or quoting
// untrusted upstream strings.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := n
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i] + "..."
}

// Unwrap makes errors.Is(err, ErrInvalid) true.
func (e *ParseError) Unwrap() error { return ErrInvalid }

// CompareDigits compares two non-empty strings of ASCII digits by numeric
// value, without overflow: "007" == "7" < "10".
func CompareDigits(a, b string) int {
	a = strings.TrimLeft(a, "0")
	b = strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

// TrimZeros strips leading zeros from a string of ASCII digits, keeping "0".
func TrimZeros(digits string) string {
	t := strings.TrimLeft(digits, "0")
	if t == "" && digits != "" {
		return "0"
	}
	return t
}

// IsDigits reports whether s is a non-empty string of ASCII digits.
func IsDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Natural compares two strings in natural order: runs of ASCII digits
// compare by numeric value ("1.10" > "1.9"), other runs compare bytewise,
// and a string that is a prefix of the other sorts first. Strings that are
// equal under these rules (e.g. "1.01" and "1.1") are ordered bytewise, so
// Natural returns 0 only for identical strings.
func Natural(a, b string) int {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		ra, na := nextRun(a, i)
		rb, nb := nextRun(b, j)
		da, db := isDigit(ra[0]), isDigit(rb[0])
		var c int
		switch {
		case da && db:
			c = CompareDigits(ra, rb)
		default:
			c = strings.Compare(ra, rb)
		}
		if c != 0 {
			return c
		}
		i, j = na, nb
	}
	switch {
	case i < len(a):
		return 1
	case j < len(b):
		return -1
	}
	return strings.Compare(a, b)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// nextRun returns the maximal run of digits or non-digits starting at i, and
// the index after it.
func nextRun(s string, i int) (string, int) {
	d := isDigit(s[i])
	j := i + 1
	for j < len(s) && isDigit(s[j]) == d {
		j++
	}
	return s[i:j], j
}
