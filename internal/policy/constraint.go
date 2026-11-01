package policy

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/angelia-dev/angelia/internal/version"
)

// Limits on a constraint.
const (
	// MaxConstraintLen bounds the length of a constraint in bytes.
	MaxConstraintLen = 256
	// MaxComparators bounds the number of comparators in a constraint.
	MaxComparators = 16
)

// Comparison operators, longest first so that ">=" is not read as ">".
var operators = []string{">=", "<=", "==", "!=", ">", "<", "="}

// Comparator is one operator and its operand, as written: ">=2".
type Comparator struct {
	Op      string // one of >=, <=, ==, !=, >, <, = (= and == are equal)
	Operand string
}

// Constraint is a parsed, scheme-independent version range: alternatives
// joined by "||", each a list of comparators that must all hold. Its
// operands are only parsed by Bind, with the watch's scheme.
type Constraint struct {
	alts [][]Comparator
}

// ParseConstraint parses a range such as ">=2 <3", ">=1.2, <2 || >=3" or
// "1.4.2" (a bare version means "=="):
//
//   - comparators are separated by white space or ",", and all must hold;
//   - "||" separates alternatives, one of which must hold;
//   - an operator may be followed by white space (">= 2").
//
// Empty or blank input is the zero Constraint, which matches every version.
// Errors wrap ErrInvalid.
func ParseConstraint(s string) (Constraint, error) {
	if len(s) > MaxConstraintLen {
		return Constraint{}, fmt.Errorf("%w: longer than %d bytes", ErrInvalid, MaxConstraintLen)
	}
	if strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) && r != '\t' }) >= 0 {
		return Constraint{}, fmt.Errorf("%w: control characters (the constraint must be one line)", ErrInvalid)
	}
	if strings.TrimSpace(s) == "" {
		return Constraint{}, nil
	}
	var c Constraint
	n := 0
	for _, alt := range strings.Split(s, "||") {
		fields := strings.Fields(strings.ReplaceAll(alt, ",", " "))
		if len(fields) == 0 {
			return Constraint{}, fmt.Errorf("%w: %q: empty alternative", ErrInvalid, s)
		}
		var cmps []Comparator
		for i := 0; i < len(fields); i++ {
			op, operand, err := splitOp(fields[i])
			if err != nil {
				return Constraint{}, fmt.Errorf("%w: %q: %w", ErrInvalid, s, err)
			}
			if operand == "" && op != "" && i+1 < len(fields) && opPrefix(fields[i+1]) == "" {
				i++
				operand = fields[i]
			}
			if operand == "" {
				return Constraint{}, fmt.Errorf("%w: %q: operator %q without a version", ErrInvalid, s, op)
			}
			if j := strings.IndexAny(operand, "<>=~^|"); j >= 0 {
				if operand[j] == '|' {
					return Constraint{}, fmt.Errorf("%w: %q: single \"|\" in %q: separate alternatives with \"||\"",
						ErrInvalid, s, version.Truncate(operand, 32))
				}
				return Constraint{}, fmt.Errorf("%w: %q: missing separator before %q: separate comparators with a space or a comma",
					ErrInvalid, s, version.Truncate(operand[j:], 32))
			}
			if op == "" || op == "=" {
				op = "=="
			}
			cmps = append(cmps, Comparator{Op: op, Operand: operand})
			if n++; n > MaxComparators {
				return Constraint{}, fmt.Errorf("%w: more than %d comparators", ErrInvalid, MaxComparators)
			}
		}
		c.alts = append(c.alts, cmps)
	}
	return c, nil
}

// opChars are the characters an operator is made of, supported or not.
const opChars = "<>=!~^"

// opPrefix returns the leading run of operator characters of tok.
func opPrefix(tok string) string {
	n := 0
	for n < len(tok) && strings.IndexByte(opChars, tok[n]) >= 0 {
		n++
	}
	return tok[:n]
}

// splitOp splits a comparator token into its operator and operand. An
// operator that is not one of operators ("~", "^", "=>", ">>") is an error
// that names it.
func splitOp(tok string) (op, operand string, err error) {
	p := opPrefix(tok)
	if p == "" {
		return "", tok, nil
	}
	if !slices.Contains(operators, p) {
		return "", "", fmt.Errorf("unsupported operator %q in %q (use >=, >, <=, <, ==, != or a bare version; ^, ~ and x ranges are not supported)",
			version.Truncate(p, 8), version.Truncate(tok, 32))
	}
	return p, tok[len(p):], nil
}

// IsZero reports whether c matches every version.
func (c Constraint) IsZero() bool { return len(c.alts) == 0 }

// String returns c in canonical form: ">=2 <3 || ==4".
func (c Constraint) String() string {
	alts := make([]string, len(c.alts))
	for i, alt := range c.alts {
		parts := make([]string, len(alt))
		for j, cmp := range alt {
			parts[j] = cmp.Op + cmp.Operand
		}
		alts[i] = strings.Join(parts, " ")
	}
	return strings.Join(alts, " || ")
}

// Range is a Constraint whose operands are parsed with a scheme.
type Range struct {
	scheme version.Scheme
	alts   [][]bound
}

type bound struct {
	op string
	v  version.Version
}

// Bind parses the operands of c with s. An operand that s cannot parse is
// retried with ".0" appended, at most twice, so that ">=2" works with
// strict SemVer ("2.0.0") and calver ("2026" is "2026.0"). An operand that
// still does not parse, or that only parses as a lexical fallback while s
// is not lexical, is an error wrapping ErrInvalid.
func (c Constraint) Bind(s version.Scheme) (Range, error) {
	r := Range{scheme: s}
	for _, alt := range c.alts {
		bs := make([]bound, len(alt))
		for i, cmp := range alt {
			v, err := parseOperand(s, cmp.Operand)
			if err != nil {
				return Range{}, err
			}
			bs[i] = bound{op: cmp.Op, v: v}
		}
		r.alts = append(r.alts, bs)
	}
	return r, nil
}

func parseOperand(s version.Scheme, operand string) (version.Version, error) {
	cand := operand
	for range 3 {
		if v, err := s.Parse(cand); err == nil && v.Scheme() == s.Name() {
			return v, nil
		}
		cand += ".0"
	}
	return nil, fmt.Errorf("%w: constraint operand %q is not a %s version", ErrInvalid, version.Truncate(operand, 32), s.Name())
}

// Match reports whether v satisfies the range. A zero Range matches every
// version. Otherwise a version that the scheme did not parse itself (a
// lexical fallback such as "latest") never matches, and comparisons use the
// scheme's precedence only: pre-releases are not treated specially (the
// prerelease mode filters them), so "<3" matches 3.0.0-rc.1 in SemVer.
func (r Range) Match(v version.Version) bool {
	if len(r.alts) == 0 {
		return true
	}
	if v == nil || v.Scheme() != r.scheme.Name() {
		return false
	}
	for _, alt := range r.alts {
		ok := true
		for _, b := range alt {
			if !holds(b.op, r.scheme.Compare(v, b.v)) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func holds(op string, c int) bool {
	switch op {
	case ">=":
		return c >= 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	case "<":
		return c < 0
	case "!=":
		return c != 0
	}
	return c == 0 // "=="
}
