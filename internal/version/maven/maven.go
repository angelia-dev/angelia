// Package maven implements the ordering of Maven artifact versions: a port
// of org.apache.maven.artifact.versioning.ComparableVersion from Maven
// 3.9.16 (maven-artifact, Apache-2.0), the ordering Maven itself uses to pick
// the newest version of a dependency.
//
// A version is split into items at '.', '-' and at every transition between
// digits and letters. A '-' (and a digit/letter transition) opens a sub-list.
// Numbers compare numerically, without a size limit. Qualifiers compare as
//
//	alpha (a1) < beta (b1) < milestone (m1) < rc = cr < snapshot
//	  < "" = ga = final = release < sp < any other qualifier (lexically)
//
// case-insensitively, so 1.0-alpha-1 < 1.0-beta < 1.0-rc1 < 1.0-SNAPSHOT
// < 1.0 = 1.0.0 = 1-ga < 1.0-sp1 < 1.0-jre. Every non-blank string is a
// version; Parse refuses only blank strings, white space, control
// characters, invalid UTF-8 and more than MaxLen bytes.
//
// Maven has no display normal form, so String returns the (trimmed) input.
package maven

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/angelia-dev/angelia/internal/version"
	"github.com/angelia-dev/angelia/internal/version/lexical"
)

// Name is the scheme's identifier.
const Name = "maven"

// MaxLen bounds the length of a version in bytes.
const MaxLen = 256

// kind is an item's type. The numeric kinds mirror Maven's IntItem,
// LongItem and BigIntegerItem: a number of up to 9 digits, of 10 to 18
// digits and of more, counted after stripping leading zeros (an all-zero
// string keeps its length, as in Maven, so "0000000000.1" > "0.1").
type kind uint8

const (
	kindInt kind = iota
	kindLong
	kindBig
	kindString
	kindList
)

// item is one element of a parsed version.
type item struct {
	kind  kind
	value string  // numbers: digits without leading zeros ("0" for zero); strings: the qualifier
	list  []*item // kindList only
}

func (it *item) isNumber() bool { return it.kind <= kindBig }

// Qualifier ranks, in Maven's order. An unknown qualifier ranks rankUnknown
// and orders lexically among the other unknown ones.
var qualifierRank = map[string]int{
	"alpha":     0,
	"beta":      1,
	"milestone": 2,
	"rc":        3,
	"snapshot":  4,
	"":          5,
	"sp":        6,
}

const (
	rankRelease = 5
	rankUnknown = 7
)

// aliases are applied after the single-letter forms a1, b1, m1.
var aliases = map[string]string{"ga": "", "final": "", "release": "", "cr": "rc"}

func newString(value string, followedByDigit bool) *item {
	if followedByDigit && len(value) == 1 {
		switch value {
		case "a":
			value = "alpha"
		case "b":
			value = "beta"
		case "m":
			value = "milestone"
		}
	}
	if a, ok := aliases[value]; ok {
		value = a
	}
	return &item{kind: kindString, value: value}
}

func newNumber(runes []rune) *item {
	var b strings.Builder
	for _, r := range runes {
		b.WriteByte("0123456789"[digitValue(r)])
	}
	digits := b.String()
	// Maven strips leading zeros but keeps an all-zero string as it is; the
	// length of that string picks the numeric kind.
	stripped := strings.TrimLeft(digits, "0")
	if stripped == "" {
		stripped = digits
	}
	k := kindInt
	switch {
	case len(stripped) > 18:
		k = kindBig
	case len(stripped) > 9:
		k = kindLong
	}
	return &item{kind: k, value: version.TrimZeros(digits)}
}

func (it *item) isNull() bool {
	switch it.kind {
	case kindString:
		return it.value == ""
	case kindList:
		return len(it.list) == 0
	}
	return it.value == "0"
}

// normalize removes trailing null items (0, "", an empty list), also
// across non-null sub-lists: 1.0.0-alpha becomes 1-alpha.
func (it *item) normalize() {
	for i := len(it.list) - 1; i >= 0; i-- {
		last := it.list[i]
		if last.isNull() {
			it.list = append(it.list[:i], it.list[i+1:]...)
		} else if last.kind != kindList {
			break
		}
	}
}

func rank(q string) int {
	if r, ok := qualifierRank[q]; ok {
		return r
	}
	return rankUnknown
}

func compareQualifiers(a, b string) int {
	ra, rb := rank(a), rank(b)
	if ra != rb || ra != rankUnknown {
		return cmpInt(ra, rb)
	}
	return compareUTF16(a, b)
}

// compareNull compares it with a missing item (the shorter list's padding).
func (it *item) compareNull() int {
	switch it.kind {
	case kindString:
		return cmpInt(rank(it.value), rankRelease)
	case kindList:
		for _, x := range it.list {
			if c := x.compareNull(); c != 0 {
				return c
			}
		}
		return 0
	}
	if it.value == "0" {
		return 0
	}
	return 1
}

// compare orders two items: a number is above a list (1.1 > 1-1), which is
// above a string (1-1 > 1-sp); a nil item is padding.
func compare(a, b *item) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -b.compareNull()
	case b == nil:
		return a.compareNull()
	}
	switch {
	case a.isNumber() && b.isNumber():
		if a.kind != b.kind {
			return cmpInt(int(a.kind), int(b.kind))
		}
		return version.CompareDigits(a.value, b.value)
	case a.isNumber():
		return 1
	case b.isNumber():
		return -1
	case a.kind == kindString && b.kind == kindString:
		return compareQualifiers(a.value, b.value)
	case a.kind == kindString:
		return -1
	case b.kind == kindString:
		return 1
	}
	for i := 0; i < len(a.list) || i < len(b.list); i++ {
		var x, y *item
		if i < len(a.list) {
			x = a.list[i]
		}
		if i < len(b.list) {
			y = b.list[i]
		}
		if c := compare(x, y); c != 0 {
			return c
		}
	}
	return 0
}

// Version is a parsed Maven version.
type Version struct {
	raw   string
	items *item
}

// String returns the trimmed input: Maven has no display normal form.
func (v Version) String() string { return v.raw }

// Scheme returns Name.
func (Version) Scheme() string { return Name }

// Parse parses raw. Every non-blank string up to MaxLen bytes without white
// space or control characters is a Maven version; errors wrap
// version.ErrInvalid.
func Parse(raw string) (Version, error) {
	fail := func(reason string) (Version, error) {
		return Version{}, &version.ParseError{Scheme: Name, Raw: raw, Reason: reason}
	}
	s := strings.TrimSpace(raw)
	switch {
	case s == "":
		return fail("blank")
	case len(s) > MaxLen:
		return fail("longer than 256 bytes")
	case !utf8.ValidString(s):
		return fail("invalid UTF-8")
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fail("white space or control character")
		}
	}
	return Version{raw: s, items: parseItems(s)}, nil
}

// parseItems is the port of ComparableVersion.parseVersion.
func parseItems(s string) *item {
	rs := []rune(toLower(s))
	root := &item{kind: kindList}
	list := root
	stack := []*item{root}
	push := func() {
		l := &item{kind: kindList}
		list.list = append(list.list, l)
		list = l
		stack = append(stack, l)
	}
	add := func(it *item) { list.list = append(list.list, it) }
	parse := func(isDigit bool, part []rune) *item {
		if isDigit {
			return newNumber(part)
		}
		return newString(string(part), false)
	}

	isDigit := false
	start := 0
	for i, c := range rs {
		switch {
		case c == '.' || c == '-':
			if i == start {
				add(&item{kind: kindInt, value: "0"})
			} else {
				add(parse(isDigit, rs[start:i]))
			}
			start = i + 1
			if c == '-' {
				push()
			}
		case isDigitRune(c):
			if !isDigit && i > start {
				// 1.0.0.X1 < 1.0.0-X2: treat .X as -X for any qualifier X.
				if len(list.list) > 0 {
					push()
				}
				add(newString(string(rs[start:i]), true))
				start = i
				push()
			}
			isDigit = true
		default:
			if isDigit && i > start {
				add(parse(true, rs[start:i]))
				start = i
				push()
			}
			isDigit = false
		}
	}
	if len(rs) > start {
		if !isDigit && len(list.list) > 0 {
			push()
		}
		add(parse(isDigit, rs[start:]))
	}
	// Children were pushed after their parents: normalize innermost first.
	for i := len(stack) - 1; i >= 0; i-- {
		stack[i].normalize()
	}
	return root
}

// toLower lower-cases like Java's String.toLowerCase(Locale.ENGLISH) for
// every character a version realistically holds.
func toLower(s string) string { return strings.ToLower(s) }

// isDigitRune matches Java's Character.isDigit(char): a decimal digit
// (Unicode category Nd) of the Basic Multilingual Plane. Characters outside
// it are UTF-16 surrogate pairs in Java and never digits.
func isDigitRune(r rune) bool {
	if r >= '0' && r <= '9' {
		return true
	}
	return r > unicode.MaxLatin1 && r <= 0xFFFF && unicode.Is(unicode.Nd, r)
}

// digitValue returns the value of a decimal digit. Unicode encodes every
// decimal digit set as a contiguous run starting at its zero.
func digitValue(r rune) int {
	if r >= '0' && r <= '9' {
		return int(r - '0')
	}
	start := r
	for start > 0 && r-start < 100 && unicode.Is(unicode.Nd, start-1) {
		start--
	}
	return int(r-start) % 10
}

// compareUTF16 compares strings by UTF-16 code units, like Java's
// String.compareTo: a character above U+FFFF (a surrogate pair, 0xD800 and
// up) sorts below U+E000..U+FFFF.
func compareUTF16(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	for i := 0; i < len(ra) && i < len(rb); i++ {
		x, y := utf16Key(ra[i]), utf16Key(rb[i])
		if x != y {
			return cmpInt(int(x), int(y))
		}
		if ra[i] != rb[i] { // two supplementary characters with the same high surrogate
			return cmpInt(int(ra[i]), int(rb[i]))
		}
	}
	return cmpInt(len(ra), len(rb))
}

func utf16Key(r rune) rune {
	if r > 0xFFFF {
		return 0xD800 + (r-0x10000)>>10
	}
	return r
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

// Compare returns Maven's ordering of a against b (-1, 0, +1).
func Compare(a, b Version) int {
	if a.items == nil || b.items == nil {
		return cmpInt(btoi(a.items != nil), btoi(b.items != nil))
	}
	return compare(a.items, b.items)
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// numericPart returns v's items up to its first qualifier: the release that
// a qualified version belongs to ("1.0-rc1" and "1.0-jre" belong to "1.0",
// "1.0-1-SNAPSHOT" to "1.0-1"). Lists only ever end a list, so the items
// form a chain.
func numericPart(l *item) *item {
	out := &item{kind: kindList}
	for _, it := range l.list {
		switch it.kind {
		case kindString:
			return out
		case kindList:
			out.list = append(out.list, numericPart(it))
			return out
		}
		out.list = append(out.list, it)
	}
	return out
}

// IsPrerelease reports whether v is a pre-release: it orders below the
// release it belongs to (1.0-a1, 1.0.M2, 1.0-RC1, 1.0-SNAPSHOT), or any of
// its qualifiers ranks below a release (alpha, beta, milestone, rc, cr,
// snapshot, wherever they appear), so flavour snapshots such as
// 33.9.0-jre-SNAPSHOT, 1.0-sp1-SNAPSHOT and 1.0-jre-rc1 are pre-releases too.
// 1.0, 1.0-sp1 and 1.0-jre are not.
func (v Version) IsPrerelease() bool {
	if v.items == nil {
		return false
	}
	return compare(v.items, numericPart(v.items)) < 0 || hasPreQualifier(v.items)
}

// hasPreQualifier reports whether l holds, at any depth, a qualifier that
// ranks below a release.
func hasPreQualifier(l *item) bool {
	for _, it := range l.list {
		switch it.kind {
		case kindString:
			if rank(it.value) < rankRelease {
				return true
			}
		case kindList:
			if hasPreQualifier(it) {
				return true
			}
		}
	}
	return false
}

// release returns the leading numbers of the top-level list.
func (v Version) release() []string {
	var out []string
	if v.items == nil {
		return out
	}
	for _, it := range v.items.list {
		if !it.isNumber() {
			break
		}
		out = append(out, it.value)
	}
	return out
}

// firstQualifier returns the first qualifier of v ("" when it has none).
func (v Version) firstQualifier() string {
	l := v.items
	for l != nil {
		next := (*item)(nil)
		for _, it := range l.list {
			switch it.kind {
			case kindString:
				return it.value
			case kindList:
				next = it
			}
		}
		l = next
	}
	return ""
}

func segment(rel []string, i int) string {
	if i < len(rel) {
		return rel[i]
	}
	return "0"
}

// Diff classifies the change from prev to next; see Scheme.Bump.
func Diff(prev, next Version) version.Bump {
	if Compare(next, prev) <= 0 {
		return version.BumpOther
	}
	pr, nr := prev.release(), next.release()
	for i := 0; i < len(pr) || i < len(nr); i++ {
		if segment(pr, i) == segment(nr, i) {
			continue
		}
		switch i {
		case 0:
			return version.BumpMajor
		case 1:
			return version.BumpMinor
		}
		return version.BumpPatch
	}
	// Same release numbers.
	switch {
	case next.IsPrerelease():
		return version.BumpPrerelease
	case prev.IsPrerelease():
		// A pre-release followed by its release: the level the release introduces.
		switch {
		case allZero(nr, 1):
			return version.BumpMajor
		case allZero(nr, 2):
			return version.BumpMinor
		}
		return version.BumpPatch
	}
	pq, nq := prev.firstQualifier(), next.firstQualifier()
	switch {
	case nq == "sp" && (pq == "" || pq == "sp"):
		return version.BumpPatch // a service pack
	case nq == "" && pq == "":
		return version.BumpPatch // a build number: 1.0 -> 1.0-1
	}
	return version.BumpOther // another qualifier: 33.8.0-android -> 33.8.0-jre
}

// allZero reports whether every number of rel from index i on is 0.
func allZero(rel []string, i int) bool {
	for ; i < len(rel); i++ {
		if rel[i] != "0" {
			return false
		}
	}
	return true
}

// Scheme implements version.Scheme for Maven versions. The zero value is
// ready to use.
type Scheme struct{}

var _ version.Scheme = Scheme{}

// Name returns "maven".
func (Scheme) Name() string { return Name }

// Parse implements version.Scheme.
func (Scheme) Parse(raw string) (version.Version, error) {
	v, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// Compare implements version.Scheme: Maven ordering for Maven versions,
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
	if mv, ok := v.(Version); ok {
		return mv.IsPrerelease()
	}
	return lexical.Scheme{}.IsPrerelease(v)
}

// Bump implements version.Scheme. The leading numbers map to major, minor
// and patch (the third and any later number). With the same numbers: a
// pre-release is prerelease; a pre-release followed by a release counts as
// the level that release introduces; a new or higher service pack (-sp) or
// build number (1.0 -> 1.0-1) is patch; any other qualifier change
// (33.8.0-android -> 33.8.0-jre) is other. It returns BumpOther when next is
// not greater than prev or either is not a Maven version.
func (Scheme) Bump(prev, next version.Version) version.Bump {
	p, okP := prev.(Version)
	n, okN := next.(Version)
	if !okP || !okN {
		return version.BumpOther
	}
	return Diff(p, n)
}
