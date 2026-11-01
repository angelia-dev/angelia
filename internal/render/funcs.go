package render

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"text/template"
	"time"
	"unicode/utf8"
)

// funcMap is the curated function set (docs/templates.md). Every function
// that builds a string checks its size against maxBytes before building it, so
// a loop cannot grow a value past the output cap. Nothing here reads the
// environment, files, the network or the clock. The text/template builtins
// that build strings (print, printf, println, html, js, urlquery) are
// replaced by size-checked versions and call is disabled; the others (and,
// or, not, len, index, slice, eq, ne, lt, le, gt, ge) are kept.
func funcMap(maxBytes int) template.FuncMap {
	c := capper{max: maxBytes}
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
		"default":    dflt,
		"upper":      c.mapStr(strings.ToUpper),
		"lower":      c.mapStr(strings.ToLower),
		"trim":       c.mapStr(strings.TrimSpace),
		"trimPrefix": c.withArg(strings.TrimPrefix),
		"trimSuffix": c.withArg(strings.TrimSuffix),
		"contains":   strPred(strings.Contains),
		"hasPrefix":  strPred(strings.HasPrefix),
		"hasSuffix":  strPred(strings.HasSuffix),
		"replace":    c.replace,
		"split":      split,
		"join":       c.join,
		"truncate":   truncate,
		"toJSON":     c.toJSON,
		"date":       c.date,

		tickFunc: noTick,
	}
}

// FuncNames lists the functions a template can call, builtins included, in
// sorted order.
func FuncNames() []string {
	names := slices.Collect(maps.Keys(funcMap(1)))
	names = append(names, "and", "or", "not", "len", "index", "slice", "eq", "ne", "lt", "le", "gt", "ge")
	names = slices.DeleteFunc(names, func(n string) bool { return n == tickFunc || n == "call" })
	slices.Sort(names)
	return names
}

var errCall = errors.New("call is not available")

func disabledCall(...any) (any, error) { return nil, errCall }

type capper struct{ max int }

func (c capper) fits(n int) error {
	if n > c.max {
		return ErrOutputTooLarge
	}
	return nil
}

// argsLen is the printed size of each argument; it fails as soon as the sum
// passes the cap. It also returns the largest single size.
func (c capper) argsLen(args []any) (total, largest int, err error) {
	for _, a := range args {
		n := 0
		if s, ok := a.(string); ok {
			n = len(s)
		} else {
			n = len(fmt.Sprint(a))
		}
		total += n
		largest = max(largest, n)
		if err := c.fits(total); err != nil {
			return 0, 0, err
		}
	}
	return total, largest, nil
}

func (c capper) result(s string) (string, error) {
	if err := c.fits(len(s)); err != nil {
		return "", err
	}
	return s, nil
}

func (c capper) print(args ...any) (string, error) {
	if _, _, err := c.argsLen(args); err != nil {
		return "", err
	}
	return c.result(fmt.Sprint(args...))
}

func (c capper) println(args ...any) (string, error) {
	if _, _, err := c.argsLen(args); err != nil {
		return "", err
	}
	return c.result(fmt.Sprintln(args...))
}

// printf refuses * widths and checks a bound of the output before
// formatting: the format, plus every explicit width and precision, plus the
// largest argument once per verb (an argument index can repeat one).
func (c capper) printf(format string, args ...any) (string, error) {
	_, largest, err := c.argsLen(args)
	if err != nil {
		return "", err
	}
	bound, err := formatBound(format, largest)
	if err != nil {
		return "", err
	}
	if err := c.fits(bound); err != nil {
		return "", err
	}
	return c.result(fmt.Sprintf(format, args...))
}

func formatBound(format string, largest int) (int, error) {
	bound := len(format)
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		bound += largest
		num := 0
	verb:
		for i++; i < len(format); i++ {
			switch ch := format[i]; {
			case ch == '*':
				return 0, errors.New("printf: * width and precision are not supported")
			case ch >= '0' && ch <= '9':
				num = num*10 + int(ch-'0')
				if num > 1<<30 {
					return 0, ErrOutputTooLarge
				}
			case ch == '.' || ch == '[' || ch == ']' || strings.IndexByte("+-# ", ch) >= 0:
				bound += num
				num = 0
			default:
				break verb
			}
		}
		bound += num
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
	return "", fmt.Errorf("%s: want a string, got %T", fn, v)
}

func dflt(def, v any) any {
	if v == nil || v == "" {
		return def
	}
	return v
}

func (c capper) mapStr(f func(string) string) func(any) (string, error) {
	return func(v any) (string, error) {
		s, err := str("argument", v)
		if err != nil {
			return "", err
		}
		return c.result(f(s))
	}
}

func (c capper) withArg(f func(string, string) string) func(string, any) (string, error) {
	return func(arg string, v any) (string, error) {
		s, err := str("argument", v)
		if err != nil {
			return "", err
		}
		return c.result(f(s, arg))
	}
}

func strPred(f func(string, string) bool) func(string, any) (bool, error) {
	return func(arg string, v any) (bool, error) {
		s, err := str("argument", v)
		if err != nil {
			return false, err
		}
		return f(s, arg), nil
	}
}

func (c capper) replace(old, repl string, v any) (string, error) {
	s, err := str("replace", v)
	if err != nil {
		return "", err
	}
	n := strings.Count(s, old)
	if err := c.fits(len(s) + n*(len(repl)-len(old))); err != nil {
		return "", err
	}
	return strings.ReplaceAll(s, old, repl), nil
}

func split(sep string, v any) ([]string, error) {
	s, err := str("split", v)
	if err != nil {
		return nil, err
	}
	return strings.Split(s, sep), nil
}

func (c capper) join(sep string, v any) (string, error) {
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
				return "", fmt.Errorf("join: element %d: want a string, got %T", i, e)
			}
			elems[i] = s
		}
	default:
		return "", fmt.Errorf("join: want a list, got %T", v)
	}
	n := len(sep) * max(len(elems)-1, 0)
	for _, e := range elems {
		n += len(e)
		if err := c.fits(n); err != nil {
			return "", err
		}
	}
	return strings.Join(elems, sep), nil
}

// truncate cuts to at most n characters, the last being "…" when cut.
func truncate(n int, v any) (string, error) {
	s, err := str("truncate", v)
	if err != nil {
		return "", err
	}
	if n < 0 {
		return "", errors.New("truncate: negative length")
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
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", fmt.Errorf("toJSON: %w", err)
	}
	return c.result(strings.TrimSuffix(buf.String(), "\n"))
}

// date formats an RFC 3339 timestamp of the event in UTC with a Go layout; a
// null timestamp gives "".
func (c capper) date(layout string, v any) (string, error) {
	s, err := str("date", v)
	if err != nil || s == "" {
		return "", err
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return "", errors.New("date: not an RFC 3339 timestamp")
	}
	return c.result(t.UTC().Format(layout))
}
