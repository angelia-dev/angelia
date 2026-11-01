package render

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/template"
)

// Error messages never quote template text: a template may hold something
// the configuration's credential check missed. text/template's messages do
// quote it (the "at <…>" node, string literals, unexpected tokens), so they
// are mapped to a fixed set of reasons. An identifier is echoed only when
// it has the shape of identShape or is a function name; for execution errors
// only the position is read from the prefix, which text/template writes from
// the template's name (fixed here) and the node's position.

// identShape is the shape of an identifier safe to echo: short, lower case,
// unlike a credential.
var identShape = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,39}$`)

// echo quotes name when it is safe to show, and returns "" otherwise.
func echo(name string) string {
	if identShape.MatchString(name) || slices.Contains(FuncNames(), name) {
		return " " + strconv.Quote(name)
	}
	return ""
}

// field names a field: " field "name"" when safe to show, " a field" otherwise.
func field(name string) string {
	if s := echo(name); s != "" {
		return " field" + s
	}
	return " a field"
}

var (
	// parsePos is the prefix of parse errors: "template: NAME:LINE: ".
	parsePos = regexp.MustCompile(`^template: (?:` + templateName + `|tick):(\d+): `)
	// execPos is the prefix of execution errors: "template: NAME:LINE:COL: ".
	execPos = regexp.MustCompile(`^template: (?:` + templateName + `|tick):(\d+):(\d+): `)
	// locRE reads the line and column of parse.Tree.ErrorContext.
	locRE = regexp.MustCompile(`:(\d+):(\d+)$`)

	parseFuncRE  = regexp.MustCompile(`^function "([A-Za-z0-9_]*)" not defined$`)
	parseVarRE   = regexp.MustCompile(`^undefined variable "\$([A-Za-z0-9_]*)"$`)
	execKeyRE    = regexp.MustCompile(`map has no entry for key "([A-Za-z0-9_]*)"$`)
	execNilRE    = regexp.MustCompile(`nil pointer evaluating [^ ]+ \{\}\.([A-Za-z0-9_]+)$`)
	execFieldRE  = regexp.MustCompile(`can't evaluate field ([A-Za-z0-9_]+) in type [^ ]+$`)
	execMethodRE = regexp.MustCompile(`([A-Za-z0-9_]+) is not a method but has arguments$`)
	execArgsRE   = regexp.MustCompile(`wrong number of args for ([A-Za-z0-9_]+): want \d+ got \d+$`)
	execCallRE   = regexp.MustCompile(`error calling ([A-Za-z0-9_]+)$`)
	execTypeRE   = regexp.MustCompile(`(wrong type for value; expected|invalid value; expected) [^ ]+; got [^ ]+$|invalid value; expected [^ ]+$`)
	execRangeRE  = regexp.MustCompile(`range can't iterate over `)
	execNonFunRE = regexp.MustCompile(`can't give argument to non-function `)
)

// parseReasons maps a text/template parse message, by prefix, to a fixed
// reason. The first match wins.
var parseReasons = []struct{ prefix, reason string }{
	{"unclosed action", "unclosed action"},
	{"unclosed comment", "unclosed comment"},
	{"comment ends before closing delimiter", "unclosed comment"},
	{"unclosed left paren", "unbalanced parentheses"},
	{"unexpected right paren", "unbalanced parentheses"},
	{"unterminated quoted string", "unterminated string"},
	{"unterminated raw quoted string", "unterminated string"},
	{"unterminated character constant", "bad character constant"},
	{"malformed character constant", "bad character constant"},
	{"bad number syntax", "bad number"},
	{"illegal number syntax", "bad number"},
	{"bad character", "bad character"},
	{"unrecognized character", "bad character"},
	{"unexpected EOF", "unexpected end of template"},
	{"{{break}} outside {{range}}", "break outside range"},
	{"{{continue}} outside {{range}}", "continue outside range"},
	{"missing value for", "missing value"},
	{"empty command", "empty action"},
	{"empty pipeline", "empty action"},
	{"non executable command in pipeline stage", "bad pipeline"},
	{"too many declarations", "bad variable declaration"},
	{"multiple definition of template", "define and block are not allowed"},
	{"unexpected", "unexpected token"},
}

func parseError(err error) error {
	e := &Error{Reason: "syntax error", kind: ErrParse}
	msg := err.Error()
	m := parsePos.FindStringSubmatch(msg)
	if m == nil {
		return e
	}
	e.Line, _ = strconv.Atoi(m[1])
	rest := msg[len(m[0]):]
	if f := parseFuncRE.FindStringSubmatch(rest); f != nil {
		e.Reason = "unknown function" + echo(f[1])
		return e
	}
	if v := parseVarRE.FindStringSubmatch(rest); v != nil {
		e.Reason = "undefined variable"
		if s := echo(v[1]); s != "" {
			e.Reason += " " + strconv.Quote("$"+v[1])
		}
		return e
	}
	for _, r := range parseReasons {
		if strings.HasPrefix(rest, r.prefix) {
			e.Reason = r.reason
			break
		}
	}
	return e
}

// execError maps an execution error. The reason comes from the error chain
// (our sentinels and function errors) or from a suffix of the message: the
// true reason is always last, after any quoted node text, and the patterns
// only capture identifier characters.
func (t *Template) execError(err error, g *guard) error {
	e := &Error{kind: ErrExec, Reason: "execution failed"}
	switch {
	case errors.Is(err, ErrTimeout):
		e.kind, e.Reason = ErrTimeout, fmt.Sprintf("execution took longer than %s", g.timeout)
		return e // the position would be that of the instrumented check
	case errors.Is(err, ErrOutputTooLarge):
		e.kind, e.Reason = ErrOutputTooLarge, fmt.Sprintf("output larger than %d bytes", t.limits.MaxOutputBytes)
	}
	var ee template.ExecError
	if !errors.As(err, &ee) {
		return e
	}
	msg := ee.Error()
	if m := execPos.FindStringSubmatch(msg); m != nil {
		e.Line, _ = strconv.Atoi(m[1])
		e.Col, _ = strconv.Atoi(m[2])
	}
	if !errors.Is(e.kind, ErrExec) {
		return e
	}
	// A function returned an error: text/template wraps it with %w after
	// "error calling NAME: ".
	if inner := errors.Unwrap(ee.Err); inner != nil {
		if c := execCallRE.FindStringSubmatch(strings.TrimSuffix(msg, ": "+inner.Error())); c != nil {
			e.Reason = "error calling a function"
			if echo(c[1]) != "" {
				e.Reason = "error calling " + c[1]
			}
			var fe *funcError
			if errors.As(inner, &fe) {
				e.Reason += ": " + fe.msg
			}
			return e
		}
	}
	switch {
	case execKeyRE.MatchString(msg):
		e.Reason = "unknown key" + echo(execKeyRE.FindStringSubmatch(msg)[1])
	case execNilRE.MatchString(msg):
		e.Reason = "cannot read" + field(execNilRE.FindStringSubmatch(msg)[1]) + " of a null value"
	case execFieldRE.MatchString(msg):
		e.Reason = "cannot read" + field(execFieldRE.FindStringSubmatch(msg)[1]) + " of a value that is not an object"
	case execMethodRE.MatchString(msg):
		e.Reason = "arguments given to" + field(execMethodRE.FindStringSubmatch(msg)[1])
	case execArgsRE.MatchString(msg):
		e.Reason = "wrong number of arguments"
		if name := execArgsRE.FindStringSubmatch(msg)[1]; echo(name) != "" {
			e.Reason += " for " + name
		}
	case execTypeRE.MatchString(msg):
		e.Reason = "wrong argument type"
	case execRangeRE.MatchString(msg):
		e.Reason = "cannot range over this value"
	case execNonFunRE.MatchString(msg):
		e.Reason = "arguments given to a value that is not a function"
	}
	return e
}
