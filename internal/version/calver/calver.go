// Package calver implements calendar versioning for tags such as
// "2026.10.1", "2026.10.15", "2026.3", "24.04" and "24.04.1-rc1".
//
// Accepted shape, after surrounding white space and one optional leading
// "v" or "V" are dropped:
//
//	<year>.<n>[.<n>[.<n>]][suffix]
//
// where year is 2 digits (YY, "24") or 4 digits (YYYY, "2026"), each <n> is
// a run of digits (month, day, week, counter, micro: "04", "1", "15") and
// there are two to four numeric components. Leading zeros are allowed and
// ignored when comparing, so "24.04" == "24.4". The format is not tied to a
// calendar: "2026.13" or "2026.2.31" parse, because projects use the
// components as counters (YYYY.N). Numbers have no size limit.
//
// A suffix starts with a letter or one of "-", ".", "_", "+" followed by at
// least one of [0-9A-Za-z._+-]: "24.04-rc1", "2026.10.1.hotfix",
// "2026.10b1", "2026.10.1+build5". Ordering is by the numeric components
// (missing ones count as 0), then by suffix class, then by suffix:
//
//	pre-release suffix < no suffix < any other suffix
//
// A version is a pre-release when its last number plus its suffix holds a
// common marker (alpha, beta, rc, dev, pre, snapshot, nightly, ... see the
// lexical scheme's heuristic), so "2026.10b" and "24.04a" count. Among
// pre-releases the marker ranks first (dev < alpha < beta < pre/rc), then
// the suffix in natural order, ignoring case. Other suffixes are ordered
// in natural order, ignoring case. Build metadata after the first "+"
// ("2026.10.1+build5") is ignored for ordering and for the pre-release
// class, as in SemVer. A suffix needs at least one letter or digit.
//
// The scheme is never auto-detected: a watch selects it explicitly with
// `scheme: calver`. Versions that do not fit (a SemVer "1.2.3" has a first
// component of one digit) are parse errors, which the schemes package turns
// into lexical fallback.
package calver

import (
	"strings"
	"unicode"

	"github.com/angelia-dev/angelia/internal/version"
	"github.com/angelia-dev/angelia/internal/version/lexical"
)

// Name is the scheme's identifier.
const Name = "calver"

// maxComponents is the maximum number of numeric components.
const maxComponents = 4

// Version is a parsed calendar version.
type Version struct {
	parts  []string // numeric components, as written (leading zeros kept)
	suffix string   // suffix as written including its separator, or ""
	pre    bool     // last number plus suffix holds a pre-release marker
	rank   int      // pre-release marker rank, see markerRank
	core   string   // lower-case suffix without build metadata
}

// Year returns the first component as written ("2026" or "24").
func (v Version) Year() string { return v.parts[0] }

// Parts returns a copy of the numeric components as written.
func (v Version) Parts() []string { return append([]string(nil), v.parts...) }

// Suffix returns the suffix as written, including its separator, or "".
func (v Version) Suffix() string { return v.suffix }

// String returns the normalized form: no "v" prefix, components and suffix
// as written.
func (v Version) String() string { return strings.Join(v.parts, ".") + v.suffix }

// Scheme returns Name.
func (Version) Scheme() string { return Name }

// IsPrerelease reports whether v has a pre-release suffix.
func (v Version) IsPrerelease() bool { return v.pre }

// Parse parses a calendar version. Errors wrap version.ErrInvalid.
func Parse(raw string) (Version, error) {
	fail := func(reason string) (Version, error) {
		return Version{}, &version.ParseError{Scheme: Name, Raw: raw, Reason: reason}
	}
	s := strings.TrimFunc(raw, unicode.IsSpace)
	if s != "" && (s[0] == 'v' || s[0] == 'V') {
		s = s[1:]
	}
	var v Version
	i := 0
	for len(v.parts) < maxComponents {
		j := i
		for j < len(s) && isDigit(s[j]) {
			j++
		}
		if j == i {
			break
		}
		v.parts = append(v.parts, s[i:j])
		i = j
		if i+1 < len(s) && s[i] == '.' && isDigit(s[i+1]) {
			i++
			continue
		}
		break
	}
	if len(v.parts) < 2 {
		return fail("want at least year and one more number, e.g. 2026.10 or 24.04")
	}
	if y := len(v.parts[0]); y != 2 && y != 4 {
		return fail("the first number must be a 2-digit or 4-digit year")
	}
	rest := s[i:]
	if rest == "" {
		return v, nil
	}
	if len(v.parts) == maxComponents && isDigit(rest[0]) {
		// The loop consumed the "." before a fifth number.
		return fail("too many numeric components")
	}
	if !isLetter(rest[0]) && !strings.ContainsRune("-._+", rune(rest[0])) {
		return fail("unexpected character after the numbers")
	}
	alnum := false
	for k := 0; k < len(rest); k++ {
		c := rest[k]
		switch {
		case isLetter(c) || isDigit(c):
			alnum = true
		case !strings.ContainsRune("-._+", rune(c)):
			return fail("invalid character in suffix")
		}
	}
	if !alnum {
		return fail("empty suffix")
	}
	v.suffix = rest
	core := rest
	if k := strings.IndexByte(rest, '+'); k >= 0 {
		core = rest[:k]
	}
	v.core = strings.ToLower(core)
	text := v.parts[len(v.parts)-1] + core
	if lv, err := lexical.Parse(text); err == nil {
		v.pre = lexical.Scheme{}.IsPrerelease(lv)
	}
	if v.pre {
		v.rank = markerRank(strings.ToLower(text))
	}
	return v, nil
}

// markerRank ranks the first known pre-release marker among the letter runs
// of s: dev-like markers 0, alpha 1, beta 2, pre/rc 3. Unknown markers rank 0.
func markerRank(s string) int {
	for i := 0; i < len(s); {
		if !isLetter(s[i]) {
			i++
			continue
		}
		j := i
		for j < len(s) && isLetter(s[j]) {
			j++
		}
		switch s[i:j] {
		case "dev", "snapshot", "nightly", "canary", "next":
			return 0
		case "a", "alpha":
			return 1
		case "b", "beta", "m", "milestone":
			return 2
		case "pre", "preview", "c", "rc", "cr", "ea":
			return 3
		}
		i = j
	}
	return 0
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// class orders suffixes: pre-release, none, other.
func (v Version) class() int {
	switch {
	case v.pre:
		return -1
	case v.core == "":
		return 0
	}
	return 1
}

// part returns numeric component i, 0 when missing.
func (v Version) part(i int) string {
	if i < len(v.parts) {
		return v.parts[i]
	}
	return "0"
}

// Compare orders two calendar versions, returning -1, 0 or +1. It returns 0
// for "24.04" and "24.4"; key known-version sets by the raw string.
func Compare(a, b Version) int {
	for i := 0; i < maxComponents; i++ {
		if c := version.CompareDigits(a.part(i), b.part(i)); c != 0 {
			return c
		}
	}
	if ca, cb := a.class(), b.class(); ca != cb {
		if ca < cb {
			return -1
		}
		return 1
	}
	if a.rank != b.rank {
		if a.rank < b.rank {
			return -1
		}
		return 1
	}
	return version.Natural(a.core, b.core)
}

// Diff classifies the change from prev to next: the first numeric component
// that differs decides (year: BumpMajor, second: BumpMinor, third or
// fourth: BumpPatch). With the same numbers, a pre-release next gives
// BumpPrerelease; a pre-release followed by its release (or by a version
// with another suffix) gives the level the release introduces, by the last
// non-zero number of next (year only: BumpMajor, second: BumpMinor,
// otherwise BumpPatch); any other suffix change gives BumpPatch. It
// returns BumpOther unless next is greater than prev.
func Diff(prev, next Version) version.Bump {
	if Compare(prev, next) >= 0 {
		return version.BumpOther
	}
	for i := 0; i < maxComponents; i++ {
		if version.CompareDigits(prev.part(i), next.part(i)) != 0 {
			switch i {
			case 0:
				return version.BumpMajor
			case 1:
				return version.BumpMinor
			}
			return version.BumpPatch
		}
	}
	switch {
	case next.pre:
		return version.BumpPrerelease
	case prev.pre:
		for i := len(next.parts) - 1; i >= 0; i-- {
			if version.TrimZeros(next.parts[i]) != "0" {
				switch i {
				case 0:
					return version.BumpMajor
				case 1:
					return version.BumpMinor
				}
				return version.BumpPatch
			}
		}
		return version.BumpMajor
	}
	return version.BumpPatch
}

// Scheme implements version.Scheme for calendar versions. The zero value is ready to use.
type Scheme struct{}

var _ version.Scheme = Scheme{}

// Name returns "calver".
func (Scheme) Name() string { return Name }

// Parse implements version.Scheme.
func (Scheme) Parse(raw string) (version.Version, error) {
	v, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// Compare implements version.Scheme: calver precedence for calver versions,
// which rank above any other version; other versions compare lexically.
func (Scheme) Compare(a, b version.Version) int {
	va, okA := a.(Version)
	vb, okB := b.(Version)
	switch {
	case okA && okB:
		return Compare(va, vb)
	case okA:
		return 1
	case okB:
		return -1
	}
	return lexical.Scheme{}.Compare(a, b)
}

// IsPrerelease implements version.Scheme. For a version of another scheme
// it uses the lexical heuristic.
func (Scheme) IsPrerelease(v version.Version) bool {
	if cv, ok := v.(Version); ok {
		return cv.pre
	}
	return lexical.Scheme{}.IsPrerelease(v)
}

// Bump implements version.Scheme; see Diff. It returns version.BumpOther if
// either version is not calver.
func (Scheme) Bump(prev, next version.Version) version.Bump {
	p, okP := prev.(Version)
	n, okN := next.(Version)
	if !okP || !okN {
		return version.BumpOther
	}
	return Diff(p, n)
}
