// Package semver implements Semantic Versioning 2.0.0 (https://semver.org)
// with the npm-style loose prefix: surrounding white space, a leading "="
// and a leading "v" or "V" are accepted and dropped ("v1.2.3" == "1.2.3").
// Everything else follows the spec strictly: three numeric components
// without leading zeros, dot-separated pre-release and build identifiers.
// Numbers have no size limit; they are compared as digit strings.
package semver

import (
	"strings"
	"unicode"

	"github.com/angelia-dev/angelia/internal/version"
	"github.com/angelia-dev/angelia/internal/version/lexical"
)

// Name is the scheme's identifier.
const Name = "semver"

// Version is a parsed SemVer 2.0.0 version.
type Version struct {
	major, minor, patch string   // digits, no leading zeros
	pre                 []string // pre-release identifiers, nil for a release
	build               []string // build metadata identifiers
}

// Major returns the major version as a decimal string.
func (v Version) Major() string { return v.major }

// Minor returns the minor version as a decimal string.
func (v Version) Minor() string { return v.minor }

// Patch returns the patch version as a decimal string.
func (v Version) Patch() string { return v.patch }

// Prerelease returns the pre-release part without the leading "-", or "".
func (v Version) Prerelease() string { return strings.Join(v.pre, ".") }

// Build returns the build metadata without the leading "+", or "".
func (v Version) Build() string { return strings.Join(v.build, ".") }

// String returns the canonical form, without any "v" prefix and with build metadata.
func (v Version) String() string {
	var b strings.Builder
	b.WriteString(v.major)
	b.WriteByte('.')
	b.WriteString(v.minor)
	b.WriteByte('.')
	b.WriteString(v.patch)
	if len(v.pre) > 0 {
		b.WriteByte('-')
		b.WriteString(v.Prerelease())
	}
	if len(v.build) > 0 {
		b.WriteByte('+')
		b.WriteString(v.Build())
	}
	return b.String()
}

// Scheme returns Name.
func (Version) Scheme() string { return Name }

// IsPrerelease reports whether v has pre-release identifiers.
func (v Version) IsPrerelease() bool { return len(v.pre) > 0 }

// Parse parses a SemVer 2.0.0 version with the loose prefix described in the
// package documentation. Errors wrap version.ErrInvalid.
func Parse(raw string) (Version, error) {
	fail := func(reason string) (Version, error) {
		return Version{}, &version.ParseError{Scheme: Name, Raw: raw, Reason: reason}
	}
	s := strings.TrimFunc(raw, unicode.IsSpace)
	s = strings.TrimPrefix(s, "=")
	if s != "" && (s[0] == 'v' || s[0] == 'V') {
		s = s[1:]
	}
	if s == "" {
		return fail("empty")
	}

	var v Version
	if i := strings.IndexByte(s, '+'); i >= 0 {
		build := s[i+1:]
		s = s[:i]
		ids, ok := identifiers(build, false)
		if !ok {
			return fail("invalid build metadata")
		}
		v.build = ids
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre := s[i+1:]
		s = s[:i]
		ids, ok := identifiers(pre, true)
		if !ok {
			return fail("invalid pre-release")
		}
		v.pre = ids
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return fail("want MAJOR.MINOR.PATCH")
	}
	for _, p := range parts {
		if !numeric(p) {
			return fail("version core must be numbers without leading zeros")
		}
	}
	v.major, v.minor, v.patch = parts[0], parts[1], parts[2]
	return v, nil
}

// identifiers splits a dot-separated list of non-empty [0-9A-Za-z-]
// identifiers. In a pre-release, numeric identifiers must not have leading zeros.
func identifiers(s string, pre bool) ([]string, bool) {
	ids := strings.Split(s, ".")
	for _, id := range ids {
		if id == "" {
			return nil, false
		}
		for i := 0; i < len(id); i++ {
			if !isIdentChar(id[i]) {
				return nil, false
			}
		}
		if pre && version.IsDigits(id) && !numeric(id) {
			return nil, false
		}
	}
	return ids, true
}

// isIdentChar reports whether c is in [0-9A-Za-z-].
func isIdentChar(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-'
}

// numeric reports whether s is "0" or digits without a leading zero.
func numeric(s string) bool {
	return version.IsDigits(s) && (s == "0" || s[0] != '0')
}

// Compare returns the SemVer precedence of a against b (-1, 0, +1).
// Build metadata is ignored, as the spec requires.
func Compare(a, b Version) int {
	for _, p := range [...][2]string{{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch}} {
		if c := version.CompareDigits(p[0], p[1]); c != 0 {
			return c
		}
	}
	return comparePre(a.pre, b.pre)
}

func comparePre(a, b []string) int {
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		return 1 // a release has higher precedence than its pre-releases
	case len(b) == 0:
		return -1
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := compareIdentifier(a[i], b[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

func compareIdentifier(a, b string) int {
	na, nb := version.IsDigits(a), version.IsDigits(b)
	switch {
	case na && nb:
		return version.CompareDigits(a, b)
	case na:
		return -1 // numeric identifiers have lower precedence than alphanumeric ones
	case nb:
		return 1
	}
	return strings.Compare(a, b)
}

// Diff classifies the change from prev to next; see Scheme.Bump.
func Diff(prev, next Version) version.Bump {
	if Compare(next, prev) <= 0 {
		return version.BumpOther
	}
	switch {
	case prev.major != next.major:
		return version.BumpMajor
	case prev.minor != next.minor:
		return version.BumpMinor
	case prev.patch != next.patch:
		return version.BumpPatch
	case next.IsPrerelease():
		return version.BumpPrerelease
	}
	// Same core, prev is a pre-release and next its release: classify by the
	// release it leads to, like npm's semver.diff: 2.0.0-rc.1 -> 2.0.0 is major.
	switch {
	case next.minor == "0" && next.patch == "0":
		return version.BumpMajor
	case next.patch == "0":
		return version.BumpMinor
	}
	return version.BumpPatch
}

// Scheme implements version.Scheme for SemVer. The zero value is ready to use.
type Scheme struct{}

var _ version.Scheme = Scheme{}

// Name returns "semver".
func (Scheme) Name() string { return Name }

// Parse implements version.Scheme.
func (Scheme) Parse(raw string) (version.Version, error) {
	v, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// Compare implements version.Scheme: SemVer precedence for SemVer versions,
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
	if sv, ok := v.(Version); ok {
		return sv.IsPrerelease()
	}
	return lexical.Scheme{}.IsPrerelease(v)
}

// Bump implements version.Scheme: the most significant core component that
// changed, BumpPrerelease between pre-releases of the same core, and for a
// pre-release followed by its release the level that release introduces.
// It returns BumpOther when next is not greater than prev or either is not SemVer.
func (Scheme) Bump(prev, next version.Version) version.Bump {
	p, okP := prev.(Version)
	n, okN := next.(Version)
	if !okP || !okN {
		return version.BumpOther
	}
	return Diff(p, n)
}
