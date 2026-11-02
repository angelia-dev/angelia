package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"text/template"
	"time"
	"unicode/utf8"
)

// funcMap is the curated function set (docs/templates.md), bound to the
// guard of one execution: every function first checks the deadline, so the
// timeout and the context also apply inside function calls. g is nil at
// Compile (parsing only needs the names); execute rebinds the map.
//
// Every function that builds a string bounds its size before building it
// (toJSON, the escapers and upper/lower right after: their output is at most
// a small multiple of an input that is already capped), so a loop cannot grow a
// value past the output cap. Nothing here reads the environment, files, the
// network or the clock. The builtins that build strings (print, printf,
// println, html, js, urlquery) are replaced by size-checked versions that
// take scalar arguments only, and call is disabled; the others (and, or,
// not, len, index, slice, eq, ne, lt, le, gt, ge) are kept.
func funcMap(g *guard, maxBytes int) template.FuncMap {
	c := capper{max: int64(maxBytes), g: g}
	tick := noTick
	if g != nil {
		tick = g.tick
	}
	return template.FuncMap{
		// Builtins replaced.
		"print":    c.print,
		"println":  c.println,
		"printf":   c.printf,
		"html":     c.escaper(template.HTMLEscapeString),
		"js":       c.escaper(template.JSEscapeString),
		"urlquery": c.escaper(url.QueryEscape),
		"call":     disabledCall,

		// Curated.
		"default":    c.dflt,
		"upper":      c.mapStr(strings.ToUpper),
		"lower":      c.mapStr(strings.ToLower),
		"trim":       c.mapStr(strings.TrimSpace),
		"trimPrefix": c.withArg(strings.TrimPrefix),
		"trimSuffix": c.withArg(strings.TrimSuffix),
		"contains":   c.strPred(strings.Contains),
		"hasPrefix":  c.strPred(strings.HasPrefix),
		"hasSuffix":  c.strPred(strings.HasSuffix),
		"replace":    c.replace,
		"split":      c.split,
		"join":       c.join,
		"truncate":   c.truncate,
		"toJSON":     c.toJSON,
		"date":       c.date,

		tickFunc: tick,
	}
}

// builtinNames are the text/template builtins kept as they are.
var builtinNames = []string{"and", "or", "not", "len", "index", "slice", "eq", "ne", "lt", "le", "gt", "ge"}

// FuncNames lists the functions a template can call, builtins included, in
// sorted order.
func FuncNames() []string {
	names := slices.Collect(maps.Keys(funcMap(nil, 1)))
	names = append(names, builtinNames...)
	names = slices.DeleteFunc(names, func(n string) bool { return n == tickFunc || n == "call" })
	slices.Sort(names)
	return names
}

// funcError is an error of a curated function. Its message is written here
// and never holds template text, so error reports may show it.
type funcError struct{ msg string }

func (e *funcError) Error() string { return e.msg }

func ferr(format string, a ...any) error { return &funcError{msg: fmt.Sprintf(format, a...)} }

func disabledCall(...any) (any, error) { return nil, ferr("call is not available") }

type capper struct {
	max int64
	g   *guard
}

// start is called first by every function.
func (c capper) start() error {
	if c.g == nil {
		return nil
	}
	return c.g.check()
}

func (c capper) fits(n int64) error {
	if n > c.max || n < 0 {
		return ErrOutputTooLarge
	}
	return nil
}

// argsLen checks that print-like arguments are scalars and bounds their
// printed size, failing as soon as the running total passes the cap. It
// returns the largest single size. Lists and maps are refused: fmt applies a
// width to each element, so "%100000s" of a long list would allocate far
// past the cap.
func (c capper) argsLen(fn string, args []any) (largest int64, err error) {
	var total int64
	for i, a := range args {
		var n int64
		switch a := a.(type) {
		case string:
			n = int64(len(a))
		case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
			n = int64(len(fmt.Sprint(a)))
		default:
			return 0, ferr("%s: argument %d is a %s; use join or toJSON for lists and objects", fn, i+1, kindName(a))
		}
		total += n
		largest = max(largest, n)
		if err := c.fits(total); err != nil {
			return 0, err
		}
	}
	return largest, nil
}

func kindName(v any) string {
	switch v.(type) {
	case []any, []string:
		return "list"
	case map[string]any, map[string]string:
		return "object"
	}
	return "non-scalar value"
}

func (c capper) result(s string) (string, error) {
	if err := c.fits(int64(len(s))); err != nil {
		return "", err
	}
	return s, nil
}

func (c capper) print(args ...any) (string, error) {
	return c.printLike("print", fmt.Sprint, args)
}

func (c capper) println(args ...any) (string, error) {
	return c.printLike("println", fmt.Sprintln, args)
}

func (c capper) printLike(fn string, f func(...any) string, args []any) (string, error) {
	if err := c.start(); err != nil {
		return "", err
	}
	largest, err := c.argsLen(fn, args)
	if err != nil {
		return "", err
	}
	// Sprint and Sprintln add at most one separator per argument.
	if err := c.fits(int64(len(args)) * (largest + 1)); err != nil {
		return "", err
	}
	return c.result(f(args...))
}

// printf refuses * widths and checks a bound of the output before
// formatting: the format, plus every explicit width and precision, plus the
// largest argument once per verb (an argument index can repeat one).
func (c capper) printf(format string, args ...any) (string, error) {
	if err := c.start(); err != nil {
		return "", err
	}
	largest, err := c.argsLen("printf", args)
	if err != nil {
		return "", err
	}
	bound, err := formatBound(format, largest, c.max)
	if err != nil {
		return "", err
	}
	if err := c.fits(bound); err != nil {
		return "", err
	}
	return c.result(fmt.Sprintf(format, args...))
}

// formatBound bounds the output of fmt.Sprintf(format, ...) whose arguments
// print to at most largest bytes each. It stops with ErrOutputTooLarge once
// the bound passes limit, so the sum cannot overflow.
func formatBound(format string, largest, limit int64) (int64, error) {
	bound := int64(len(format))
	add := func(n int64) error {
		bound += n
		if bound > limit {
			return ErrOutputTooLarge
		}
		return nil
	}
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		if err := add(largest); err != nil {
			return 0, err
		}
		var num int64
	verb:
		for i++; i < len(format); i++ {
			switch ch := format[i]; {
			case ch == '*':
				return 0, ferr("printf: * width and precision are not supported")
			case ch >= '0' && ch <= '9':
				num = num*10 + int64(ch-'0')
				if num > limit {
					return 0, ErrOutputTooLarge
				}
			case ch == '.' || ch == '[' || ch == ']' || strings.IndexByte("+-# ", ch) >= 0:
				if err := add(num); err != nil {
					return 0, err
				}
				num = 0
			default:
				break verb
			}
		}
		if err := add(num); err != nil {
			return 0, err
		}
	}
	return bound, nil
}

func (c capper) escaper(esc func(string) string) func(...any) (string, error) {
	return func(args ...any) (string, error) {
		s, err := c.print(args...)
		if err != nil {
			return "", err
		}
		return c.result(esc(s))
	}
}

// str is the string form of a template argument: a string as is, a JSON null
// (absent value) as "".
func str(fn string, v any) (string, error) {
	switch v := v.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	}
	return "", ferr("%s: want a string, got %T", fn, v)
}

func (c capper) dflt(def, v any) (any, error) {
	if err := c.start(); err != nil {
		return nil, err
	}
	if v == nil || v == "" {
		return def, nil
	}
	return v, nil
}

func (c capper) mapStr(f func(string) string) func(any) (string, error) {
	return func(v any) (string, error) {
		if err := c.start(); err != nil {
			return "", err
		}
		s, err := str("argument", v)
		if err != nil {
			return "", err
		}
		return c.result(f(s)) // case mapping grows text at most 3 times
	}
}

func (c capper) withArg(f func(string, string) string) func(string, any) (string, error) {
	return func(arg string, v any) (string, error) {
		if err := c.start(); err != nil {
			return "", err
		}
		s, err := str("argument", v)
		if err != nil {
			return "", err
		}
		return f(s, arg), nil // never longer than s
	}
}

func (c capper) strPred(f func(string, string) bool) func(string, any) (bool, error) {
	return func(arg string, v any) (bool, error) {
		if err := c.start(); err != nil {
			return false, err
		}
		s, err := str("argument", v)
		if err != nil {
			return false, err
		}
		return f(s, arg), nil
	}
}

func (c capper) replace(old, repl string, v any) (string, error) {
	if err := c.start(); err != nil {
		return "", err
	}
	s, err := str("replace", v)
	if err != nil {
		return "", err
	}
	n := int64(strings.Count(s, old))
	if err := c.fits(int64(len(s)) + n*(int64(len(repl))-int64(len(old)))); err != nil {
		return "", err
	}
	return strings.ReplaceAll(s, old, repl), nil
}

func (c capper) split(sep string, v any) ([]string, error) {
	if err := c.start(); err != nil {
		return nil, err
	}
	s, err := str("split", v)
	if err != nil {
		return nil, err
	}
	return strings.Split(s, sep), nil
}

func (c capper) join(sep string, v any) (string, error) {
	if err := c.start(); err != nil {
		return "", err
	}
	var elems []string
	switch v := v.(type) {
	case nil:
	case []string:
		elems = v
	case []any:
		elems = make([]string, len(v))
		for i, e := range v {
			s, ok := e.(string)
			if !ok {
				return "", ferr("join: element %d: want a string, got %T", i, e)
			}
			elems[i] = s
		}
	default:
		return "", ferr("join: want a list, got %T", v)
	}
	n := int64(len(sep)) * int64(max(len(elems)-1, 0))
	if err := c.fits(n); err != nil {
		return "", err
	}
	for _, e := range elems {
		n += int64(len(e))
		if err := c.fits(n); err != nil {
			return "", err
		}
	}
	return strings.Join(elems, sep), nil
}

// truncate cuts to at most n characters, the last being "…" when cut.
func (c capper) truncate(n int, v any) (string, error) {
	if err := c.start(); err != nil {
		return "", err
	}
	s, err := str("truncate", v)
	if err != nil {
		return "", err
	}
	if n < 0 {
		return "", ferr("truncate: negative length")
	}
	if utf8.RuneCountInString(s) <= n {
		return s, nil
	}
	if n == 0 {
		return "", nil
	}
	i, count := 0, 0
	for i = range s {
		if count == n-1 {
			break
		}
		count++
	}
	return s[:i] + "…", nil
}

func (c capper) toJSON(v any) (string, error) {
	if err := c.start(); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", ferr("toJSON: cannot encode a %T", v)
	}
	return c.result(strings.TrimSuffix(buf.String(), "\n"))
}

// date formats an RFC 3339 timestamp of the event in UTC with a Go layout; a
// null timestamp gives "".
func (c capper) date(layout string, v any) (string, error) {
	if err := c.start(); err != nil {
		return "", err
	}
	s, err := str("date", v)
	if err != nil || s == "" {
		return "", err
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return "", ferr("date: not an RFC 3339 timestamp")
	}
	return c.result(t.UTC().Format(layout))
}
