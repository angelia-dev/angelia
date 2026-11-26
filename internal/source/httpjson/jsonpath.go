package httpjson

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The JSONPath subset of decision D50: a strict subset of RFC 9535, so a
// later full implementation keeps every expression valid with the same
// meaning.
//
//	query    = "$" *(S segment)
//	segment  = "." member-name-shorthand / ".*" / "[" S selector S "]"
//	selector = name-selector ('…' or "…" with the RFC escapes) / "*" / index
//	index    = "0" / ["-"] %x31-39 *DIGIT   (I-JSON range, negative from the end)
//
// Descendant segments (..), filters ([?…]), slices ([a:b]), unions ([a,b])
// and function expressions are refused.
const (
	// MaxPathBytes is the longest jsonpath coordinate.
	MaxPathBytes = 512
	// MaxSegments is the most segments a jsonpath may have.
	MaxSegments = 32
	maxIndex    = 1<<53 - 1
)

type selKind int

const (
	selName selKind = iota
	selWildcard
	selIndex
)

type selector struct {
	kind  selKind
	name  string
	index int64
}

// Path is a parsed JSONPath of the D50 subset. It is immutable and safe for
// concurrent use.
type Path struct{ segs []selector }

// pathError reports a parse error at a byte offset of the expression.
func pathError(pos int, format string, a ...any) error {
	return fmt.Errorf("jsonpath: at byte %d: %s", pos, fmt.Sprintf(format, a...))
}

// ParsePath parses expr.
func ParsePath(expr string) (*Path, error) {
	if expr == "" {
		return nil, fmt.Errorf("jsonpath: empty; start with $")
	}
	if len(expr) > MaxPathBytes {
		return nil, fmt.Errorf("jsonpath: longer than %d bytes", MaxPathBytes)
	}
	if !utf8.ValidString(expr) {
		return nil, fmt.Errorf("jsonpath: not valid UTF-8")
	}
	if expr[0] != '$' {
		return nil, pathError(0, "must start with $")
	}
	p := &parser{s: expr, pos: 1}
	var segs []selector
	for {
		blankAt := p.pos
		p.blank()
		if p.pos == len(p.s) {
			if p.pos != blankAt {
				return nil, pathError(blankAt, "trailing white space")
			}
			break
		}
		sel, err := p.segment()
		if err != nil {
			return nil, err
		}
		segs = append(segs, sel)
		if len(segs) > MaxSegments {
			return nil, fmt.Errorf("jsonpath: more than %d segments", MaxSegments)
		}
	}
	return &Path{segs: segs}, nil
}

type parser struct {
	s   string
	pos int
}

func (p *parser) peek() byte {
	if p.pos < len(p.s) {
		return p.s[p.pos]
	}
	return 0
}

func (p *parser) blank() {
	for p.pos < len(p.s) && strings.IndexByte(" \t\n\r", p.s[p.pos]) >= 0 {
		p.pos++
	}
}

func (p *parser) segment() (selector, error) {
	switch c := p.peek(); c {
	case '.':
		p.pos++
		switch p.peek() {
		case '.':
			return selector{}, pathError(p.pos-1, "descendant segments (..) are not supported")
		case '*':
			p.pos++
			return selector{kind: selWildcard}, nil
		}
		name, err := p.shorthand()
		return selector{kind: selName, name: name}, err
	case '[':
		p.pos++
		p.blank()
		sel, err := p.selector()
		if err != nil {
			return selector{}, err
		}
		p.blank()
		switch p.peek() {
		case ']':
			p.pos++
			return sel, nil
		case ',':
			return selector{}, pathError(p.pos, "unions ([a,b]) are not supported")
		case ':':
			return selector{}, pathError(p.pos, "array slices ([a:b]) are not supported")
		case 0:
			return selector{}, pathError(p.pos, "missing ]")
		}
		return selector{}, pathError(p.pos, "expected ] after the selector")
	case '(':
		return selector{}, pathError(p.pos, "function expressions are not supported")
	default:
		return selector{}, pathError(p.pos, "expected . or [")
	}
}

func isNameFirst(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' || r >= 0x80
}

// shorthand reads a member-name-shorthand (RFC 9535 §2.5.1.1).
func (p *parser) shorthand() (string, error) {
	start := p.pos
	for p.pos < len(p.s) {
		r, n := utf8.DecodeRuneInString(p.s[p.pos:])
		if !isNameFirst(r) && (p.pos == start || r < '0' || r > '9') {
			break
		}
		p.pos += n
	}
	if p.pos == start {
		if p.pos == len(p.s) {
			return "", pathError(p.pos, "expected a member name after .")
		}
		return "", pathError(p.pos, "a member name after . starts with a letter, _ or a non-ASCII character; use ['…'] for other names")
	}
	return p.s[start:p.pos], nil
}

func (p *parser) selector() (selector, error) {
	switch c := p.peek(); {
	case c == '\'' || c == '"':
		name, err := p.str(c)
		return selector{kind: selName, name: name}, err
	case c == '*':
		p.pos++
		return selector{kind: selWildcard}, nil
	case c == '-' || c >= '0' && c <= '9':
		i, err := p.index()
		return selector{kind: selIndex, index: i}, err
	case c == '?':
		return selector{}, pathError(p.pos, "filter selectors ([?…]) are not supported")
	case c == ':':
		return selector{}, pathError(p.pos, "array slices ([a:b]) are not supported")
	case c == 0:
		return selector{}, pathError(p.pos, "missing selector and ]")
	default:
		return selector{}, pathError(p.pos, "expected a quoted name, * or an index")
	}
}

// index reads an index selector: "0" or an optional "-" and digits without a
// leading zero, within the I-JSON range.
func (p *parser) index() (int64, error) {
	start := p.pos
	if p.peek() == '-' {
		p.pos++
	}
	digits := p.pos
	for p.pos < len(p.s) && p.s[p.pos] >= '0' && p.s[p.pos] <= '9' {
		p.pos++
	}
	lit := p.s[start:p.pos]
	switch {
	case p.pos == digits:
		return 0, pathError(start, "expected digits after -")
	case p.s[digits] == '0' && (p.pos-digits > 1 || digits > start):
		return 0, pathError(start, "index %s has a leading zero or is -0", lit)
	}
	i, err := strconv.ParseInt(lit, 10, 64)
	if err != nil || i > maxIndex || i < -maxIndex {
		return 0, pathError(start, "index %s is out of range", lit)
	}
	return i, nil
}

// str reads a string literal quoted with q (RFC 9535 §2.3.1.1).
func (p *parser) str(q byte) (string, error) {
	start := p.pos
	p.pos++
	var b strings.Builder
	for {
		if p.pos >= len(p.s) {
			return "", pathError(start, "unterminated string")
		}
		c := p.s[p.pos]
		switch {
		case c == q:
			p.pos++
			return b.String(), nil
		case c < 0x20:
			return "", pathError(p.pos, "control character in a string; escape it")
		case c == '\\':
			r, err := p.escape(q)
			if err != nil {
				return "", err
			}
			b.WriteRune(r)
		default:
			r, n := utf8.DecodeRuneInString(p.s[p.pos:])
			b.WriteRune(r)
			p.pos += n
		}
	}
}

// escape reads one escape sequence at p.pos (the backslash).
func (p *parser) escape(q byte) (rune, error) {
	at := p.pos
	p.pos++
	if p.pos >= len(p.s) {
		return 0, pathError(at, "unterminated escape")
	}
	c := p.s[p.pos]
	p.pos++
	switch c {
	case 'b':
		return '\b', nil
	case 'f':
		return '\f', nil
	case 'n':
		return '\n', nil
	case 'r':
		return '\r', nil
	case 't':
		return '\t', nil
	case '/', '\\':
		return rune(c), nil
	case q:
		return rune(q), nil
	case 'u':
		hi, err := p.hex4(at)
		if err != nil {
			return 0, err
		}
		switch {
		case hi >= 0xDC00 && hi <= 0xDFFF:
			return 0, pathError(at, "lone low surrogate")
		case hi < 0xD800 || hi > 0xDBFF:
			return hi, nil
		}
		if !strings.HasPrefix(p.s[p.pos:], `\u`) {
			return 0, pathError(at, "high surrogate without a low surrogate")
		}
		p.pos += 2
		lo, err := p.hex4(at)
		if err != nil {
			return 0, err
		}
		if lo < 0xDC00 || lo > 0xDFFF {
			return 0, pathError(at, "high surrogate without a low surrogate")
		}
		return 0x10000 + (hi-0xD800)<<10 + (lo - 0xDC00), nil
	}
	return 0, pathError(at, "invalid escape \\%c", c)
}

func (p *parser) hex4(at int) (rune, error) {
	if p.pos+4 > len(p.s) {
		return 0, pathError(at, "\\u needs 4 hex digits")
	}
	var v rune
	for _, c := range []byte(p.s[p.pos : p.pos+4]) {
		var d byte
		switch {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'a' && c <= 'f':
			d = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			d = c - 'A' + 10
		default:
			return 0, pathError(at, "\\u needs 4 hex digits")
		}
		v = v<<4 | rune(d)
	}
	p.pos += 4
	return v, nil
}

// Select returns the nodes the path selects in root, a value decoded by
// encoding/json (with UseNumber or without): arrays in document order,
// object wildcards in sorted member-name order (RFC 9535 leaves it open;
// sorting makes the result deterministic).
func (pa *Path) Select(root any) []any {
	nodes := []any{root}
	for _, sel := range pa.segs {
		var next []any
		for _, n := range nodes {
			next = sel.apply(n, next)
		}
		nodes = next
		if len(nodes) == 0 {
			break
		}
	}
	return nodes
}

func (s selector) apply(n any, out []any) []any {
	switch s.kind {
	case selName:
		if m, ok := n.(map[string]any); ok {
			if v, ok := m[s.name]; ok {
				out = append(out, v)
			}
		}
	case selWildcard:
		switch v := n.(type) {
		case []any:
			out = append(out, v...)
		case map[string]any:
			for _, k := range slices.Sorted(maps.Keys(v)) {
				out = append(out, v[k])
			}
		}
	case selIndex:
		if a, ok := n.([]any); ok {
			i := s.index
			if i < 0 {
				i += int64(len(a))
			}
			if i >= 0 && i < int64(len(a)) {
				out = append(out, a[i])
			}
		}
	}
	return out
}

// typeName names the JSON type of a decoded value, for errors.
func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number, float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	default:
		return "object"
	}
}
