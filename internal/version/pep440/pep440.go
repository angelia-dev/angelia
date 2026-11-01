// Package pep440 implements Python version identifiers as specified by
// PEP 440 / the PyPA "Version specifiers" specification, including epochs,
// pre-, post- and dev-releases, local versions and the normalization rules
// (case, separators, spelling variants such as "alpha", "c", "rev", implicit
// numbers and implicit post-releases "1.0-1"). It matches the behaviour of
// the reference implementation, packaging.version.Version.
//
// Numbers have no size limit; they are kept and compared as digit strings.
package pep440

import (
	"strings"

	"github.com/angelia-dev/angelia/internal/version"
	"github.com/angelia-dev/angelia/internal/version/lexical"
)

// Name is the scheme's identifier.
const Name = "pep440"

// Pre-release phases, in increasing order.
const (
	phaseNone  = ""
	phaseAlpha = "a"
	phaseBeta  = "b"
	phaseRC    = "rc"
)

// Version is a parsed PEP 440 version. All numbers are decimal strings
// without leading zeros.
type Version struct {
	epoch   string
	release []string
	pre     string // phaseNone, phaseAlpha, phaseBeta or phaseRC
	preN    string
	post    string // "" when there is no post-release segment
	dev     string // "" when there is no dev-release segment
	local   []string
}

// String returns the normalized form, e.g. "1!2.0rc1.post2.dev3+ubuntu.1".
func (v Version) String() string {
	var b strings.Builder
	if v.epoch != "0" {
		b.WriteString(v.epoch)
		b.WriteByte('!')
	}
	b.WriteString(strings.Join(v.release, "."))
	if v.pre != phaseNone {
		b.WriteString(v.pre)
		b.WriteString(v.preN)
	}
	if v.post != "" {
		b.WriteString(".post")
		b.WriteString(v.post)
	}
	if v.dev != "" {
		b.WriteString(".dev")
		b.WriteString(v.dev)
	}
	if len(v.local) > 0 {
		b.WriteByte('+')
		b.WriteString(strings.Join(v.local, "."))
	}
	return b.String()
}

// Scheme returns Name.
func (Version) Scheme() string { return Name }

// Public returns the normalized version without its local segment.
func (v Version) Public() string {
	v.local = nil
	return v.String()
}

// IsPrerelease reports whether v has a pre-release or dev-release segment,
// like packaging's Version.is_prerelease ("1.0.post1.dev1" is a pre-release).
func (v Version) IsPrerelease() bool { return v.pre != phaseNone || v.dev != "" }

// parser walks a lower-cased ASCII version string.
type parser struct {
	s string
	i int
}

func (p *parser) eof() bool { return p.i >= len(p.s) }

// digits consumes a run of ASCII digits and returns it ("" if none).
func (p *parser) digits() string {
	j := p.i
	for j < len(p.s) && p.s[j] >= '0' && p.s[j] <= '9' {
		j++
	}
	d := p.s[p.i:j]
	p.i = j
	return d
}

// sep consumes one optional '.', '_' or '-' and reports whether it did.
func (p *parser) sep() bool {
	if !p.eof() && (p.s[p.i] == '.' || p.s[p.i] == '_' || p.s[p.i] == '-') {
		p.i++
		return true
	}
	return false
}

// word consumes the first of words that matches at the cursor.
func (p *parser) word(words ...string) (string, bool) {
	for _, w := range words {
		if strings.HasPrefix(p.s[p.i:], w) {
			p.i += len(w)
			return w, true
		}
	}
	return "", false
}

// labelled parses the optional segment [sep] label [sep] [number], as used
// by the pre-, post- and dev-release segments. On no match the cursor is
// left untouched. A missing number means 0. Like the reference regular
// expression (possessive quantifiers), the separator after the label is
// consumed even without a number: "1.0a." and "1.0a..dev" are valid.
func (p *parser) labelled(labels ...string) (label, n string, ok bool) {
	start := p.i
	p.sep()
	label, ok = p.word(labels...)
	if !ok {
		p.i = start
		return "", "", false
	}
	p.sep()
	n = p.digits()
	if n == "" {
		n = "0"
	}
	return label, version.TrimZeros(n), true
}

// Parse parses a PEP 440 version, accepting every spelling the
// specification allows and normalizing it. Errors wrap version.ErrInvalid.
func Parse(raw string) (Version, error) {
	fail := func(reason string) (Version, error) {
		return Version{}, &version.ParseError{Scheme: Name, Raw: raw, Reason: reason}
	}
	s := strings.TrimFunc(raw, isPySpace)
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return fail("non-ASCII character")
		}
	}
	p := parser{s: strings.ToLower(s)}
	if strings.HasPrefix(p.s, "v") {
		p.i++
	}

	var v Version
	// Epoch and the first release number: "N!" or "N".
	first := p.digits()
	if first == "" {
		return fail("missing release number")
	}
	v.epoch = "0"
	if !p.eof() && p.s[p.i] == '!' {
		p.i++
		v.epoch = version.TrimZeros(first)
		if first = p.digits(); first == "" {
			return fail("missing release number after epoch")
		}
	}
	v.release = append(v.release, version.TrimZeros(first))
	for p.i+1 < len(p.s) && p.s[p.i] == '.' && isDigit(p.s[p.i+1]) {
		p.i++
		v.release = append(v.release, version.TrimZeros(p.digits()))
	}

	// Pre-release. "preview" before "pre", "alpha" before "a" and so on, so
	// the longest spelling wins.
	if label, n, ok := p.labelled("alpha", "a", "beta", "b", "preview", "pre", "c", "rc"); ok {
		switch label {
		case "alpha", "a":
			v.pre = phaseAlpha
		case "beta", "b":
			v.pre = phaseBeta
		default:
			v.pre = phaseRC
		}
		v.preN = n
	}

	// Post-release: "-N" (implicit) or [sep] post|rev|r [sep] [N].
	if p.i+1 < len(p.s) && p.s[p.i] == '-' && isDigit(p.s[p.i+1]) {
		p.i++
		v.post = version.TrimZeros(p.digits())
	} else if _, n, ok := p.labelled("post", "rev", "r"); ok {
		v.post = n
	}

	// Dev-release.
	if _, n, ok := p.labelled("dev"); ok {
		v.dev = n
	}

	// Local version: "+" then [a-z0-9]+ separated by '.', '_' or '-'.
	if !p.eof() && p.s[p.i] == '+' {
		local, ok := parseLocal(p.s[p.i+1:])
		if !ok {
			return fail("invalid local version")
		}
		v.local = local
		p.i = len(p.s)
	}
	if !p.eof() {
		return fail("unexpected trailing characters")
	}
	return v, nil
}

func parseLocal(s string) ([]string, bool) {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '.' || r == '_' || r == '-' })
	// FieldsFunc drops empty fields; reject them by checking the joined length.
	if len(parts) == 0 || len(strings.Join(parts, ".")) != len(s) {
		return nil, false
	}
	for i, part := range parts {
		for j := 0; j < len(part); j++ {
			c := part[j]
			if !isDigit(c) && (c < 'a' || c > 'z') {
				return nil, false
			}
		}
		if version.IsDigits(part) {
			parts[i] = version.TrimZeros(part)
		}
	}
	return parts, true
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// isPySpace matches the characters Python's str.strip() removes: Unicode
// white space plus the ASCII information separators U+001C..U+001F.
func isPySpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x1f, 0x85, 0xa0,
		0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// Compare returns the PEP 440 ordering of a against b (-1, 0, +1).
func Compare(a, b Version) int {
	if c := version.CompareDigits(a.epoch, b.epoch); c != 0 {
		return c
	}
	if c := compareRelease(a.release, b.release); c != 0 {
		return c
	}
	if c := cmpInt(a.phaseRank(), b.phaseRank()); c != 0 {
		return c
	}
	if a.pre != phaseNone {
		if c := version.CompareDigits(a.preN, b.preN); c != 0 {
			return c
		}
	}
	// No post-release sorts before any post-release.
	if c := compareOptional(a.post, b.post, -1); c != 0 {
		return c
	}
	// No dev-release sorts after any dev-release.
	if c := compareOptional(a.dev, b.dev, 1); c != 0 {
		return c
	}
	return compareLocal(a.local, b.local)
}

// phaseRank orders the pre-release part like packaging's _cmpkey: a bare
// dev-release (1.0.dev0) sorts before every pre-release of the same
// release, a release without pre-release after all of them.
func (v Version) phaseRank() int {
	switch v.pre {
	case phaseAlpha:
		return 1
	case phaseBeta:
		return 2
	case phaseRC:
		return 3
	}
	if v.post == "" && v.dev != "" {
		return 0
	}
	return 4
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// compareRelease compares release segments as if padded with zeros.
func compareRelease(a, b []string) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		x, y := "0", "0"
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if c := version.CompareDigits(x, y); c != 0 {
			return c
		}
	}
	return 0
}

// compareOptional compares two optional numbers; absent sorts as `absent`
// (-1: before any number, +1: after any number).
func compareOptional(a, b string, absent int) int {
	switch {
	case a == "" && b == "":
		return 0
	case a == "":
		return absent
	case b == "":
		return -absent
	}
	return version.CompareDigits(a, b)
}

// compareLocal: no local sorts first; segments compare pairwise, numeric
// segments numerically and above alphanumeric ones, alphanumeric ones
// lexically; a shorter prefix sorts first.
func compareLocal(a, b []string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		na, nb := version.IsDigits(a[i]), version.IsDigits(b[i])
		var c int
		switch {
		case na && nb:
			c = version.CompareDigits(a[i], b[i])
		case na:
			c = 1
		case nb:
			c = -1
		default:
			c = strings.Compare(a[i], b[i])
		}
		if c != 0 {
			return c
		}
	}
	return cmpInt(len(a), len(b))
}

// segment returns release segment i, or "0" past the end.
func (v Version) segment(i int) string {
	if i < len(v.release) {
		return v.release[i]
	}
	return "0"
}

// Diff classifies the change from prev to next; see Scheme.Bump.
func Diff(prev, next Version) version.Bump {
	if Compare(next, prev) <= 0 {
		return version.BumpOther
	}
	if prev.epoch != next.epoch || prev.segment(0) != next.segment(0) {
		return version.BumpMajor
	}
	if prev.segment(1) != next.segment(1) {
		return version.BumpMinor
	}
	if compareRelease(prev.release, next.release) != 0 {
		return version.BumpPatch
	}
	// Same release.
	switch {
	case next.IsPrerelease():
		return version.BumpPrerelease
	case prev.pre != phaseNone || prev.post == "" && prev.dev != "":
		// A pre-release (1.0rc1, 1.0.dev1) followed by its release: classify by
		// the level that release introduces, as for SemVer.
		if next.segment(1) == "0" && trailingZeros(next.release, 2) {
			return version.BumpMajor
		}
		if trailingZeros(next.release, 2) {
			return version.BumpMinor
		}
		return version.BumpPatch
	case prev.post != next.post || prev.dev != "":
		return version.BumpPatch // post-release, or the release of a dev post-release
	}
	return version.BumpOther // only the local segment changed
}

// trailingZeros reports whether every release segment from index i on is 0.
func trailingZeros(release []string, i int) bool {
	for ; i < len(release); i++ {
		if release[i] != "0" {
			return false
		}
	}
	return true
}

// Scheme implements version.Scheme for PEP 440. The zero value is ready to use.
type Scheme struct{}

var _ version.Scheme = Scheme{}

// Name returns "pep440".
func (Scheme) Name() string { return Name }

// Parse implements version.Scheme.
func (Scheme) Parse(raw string) (version.Version, error) {
	v, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// Compare implements version.Scheme: PEP 440 ordering for PEP 440 versions,
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
	if pv, ok := v.(Version); ok {
		return pv.IsPrerelease()
	}
	return lexical.Scheme{}.IsPrerelease(v)
}

// Bump implements version.Scheme. Release segments map to major, minor and
// patch (the third and any later segment); an epoch change is major; a new
// pre- or dev-release of the same release is prerelease; a post-release is
// patch; a pre-release followed by its release counts as the level that
// release introduces; a change of the local segment only is other. It
// returns BumpOther when next is not greater than prev or either is not PEP 440.
func (Scheme) Bump(prev, next version.Version) version.Bump {
	p, okP := prev.(Version)
	n, okN := next.(Version)
	if !okP || !okN {
		return version.BumpOther
	}
	return Diff(p, n)
}
