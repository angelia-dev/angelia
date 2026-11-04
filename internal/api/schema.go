package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// A small JSON Schema (2020-12) validator for the keywords the specification
// uses: $ref (local), type, enum, pattern, minLength, maxLength, minimum,
// maximum, properties, required, additionalProperties, minProperties,
// maxProperties, propertyNames, items, minItems, maxItems, uniqueItems,
// oneOf, anyOf and allOf. Annotations (description, default, examples,
// format, x-*) are ignored: every format the specification uses is also a
// pattern. An external $ref (the event envelope) accepts anything: the server
// returns stored events byte for byte. architecture.md section 6 lists no
// JSON Schema or OpenAPI validation library, so this stays in house.

// fieldError is one validation error, at a JSON Pointer (RFC 6901).
type fieldError struct {
	Pointer string
	Detail  string
}

var patternCache sync.Map // pattern -> *regexp.Regexp

func compilePattern(p string) (*regexp.Regexp, error) {
	if re, ok := patternCache.Load(p); ok {
		return re.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, err
	}
	patternCache.Store(p, re)
	return re, nil
}

// decodeJSON decodes one JSON document, keeping numbers as json.Number.
func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected data after the JSON value")
	}
	return v, nil
}

// pointerJoin appends one reference token to a JSON Pointer.
func pointerJoin(ptr, tok string) string {
	return ptr + "/" + strings.ReplaceAll(strings.ReplaceAll(tok, "~", "~0"), "/", "~1")
}

// validate checks v against schema and appends every error found.
func (d *specDoc) validate(schema, v any, ptr string, errs *[]fieldError) {
	m, ok := schema.(map[string]any)
	if !ok {
		return // true schema, or an unresolvable external one
	}
	if ref, ok := m["$ref"].(string); ok {
		if !strings.HasPrefix(ref, "#/") {
			return // external: not followed
		}
		d.validate(d.lookup(ref), v, ptr, errs)
	}
	bad := func(p, format string, a ...any) {
		*errs = append(*errs, fieldError{Pointer: p, Detail: fmt.Sprintf(format, a...)})
	}
	if t, ok := m["type"]; ok && !typeMatches(t, v) {
		bad(ptr, "must be %s", typeName(t))
		return
	}
	if enum, ok := m["enum"].([]any); ok && !slices.ContainsFunc(enum, func(e any) bool { return jsonEqualValue(e, v) }) {
		names := make([]string, len(enum))
		for i, e := range enum {
			names[i] = fmt.Sprint(e)
		}
		bad(ptr, "must be one of %s", strings.Join(names, ", "))
		return
	}
	switch x := v.(type) {
	case string:
		d.validateString(m, x, ptr, bad)
	case json.Number:
		validateNumber(m, x, ptr, bad)
	case map[string]any:
		d.validateObject(m, x, ptr, errs, bad)
	case []any:
		d.validateArray(m, x, ptr, errs, bad)
	}
	for _, s := range listOf(m["allOf"]) {
		d.validate(s, v, ptr, errs)
	}
	if alts := listOf(m["anyOf"]); len(alts) > 0 {
		if d.countValid(alts, v) == 0 {
			bad(ptr, "%s", anyOfDetail(d, alts))
		}
	}
	if alts := listOf(m["oneOf"]); len(alts) > 0 {
		d.validateOneOf(alts, v, ptr, errs, bad)
	}
}

func (d *specDoc) validateString(m map[string]any, s, ptr string, bad func(string, string, ...any)) {
	n := utf8.RuneCountInString(s)
	if lo, ok := intOf(m["minLength"]); ok && n < lo {
		bad(ptr, "must have at least %d characters", lo)
	}
	if hi, ok := intOf(m["maxLength"]); ok && n > hi {
		bad(ptr, "must have at most %d characters", hi)
	}
	if p, ok := m["pattern"].(string); ok {
		re, err := compilePattern(p)
		if err == nil && !re.MatchString(s) {
			bad(ptr, "must match the pattern %s", p)
		}
	}
}

func validateNumber(m map[string]any, n json.Number, ptr string, bad func(string, string, ...any)) {
	f, err := n.Float64()
	if err != nil {
		bad(ptr, "is out of range")
		return
	}
	if lo, ok := numOf(m["minimum"]); ok && f < lo {
		bad(ptr, "must be at least %v", m["minimum"])
	}
	if hi, ok := numOf(m["maximum"]); ok && f > hi {
		bad(ptr, "must be at most %v", m["maximum"])
	}
}

func (d *specDoc) validateObject(m map[string]any, obj map[string]any, ptr string, errs *[]fieldError, bad func(string, string, ...any)) {
	props, _ := m["properties"].(map[string]any)
	for _, r := range listOf(m["required"]) {
		k := str(r)
		if _, ok := obj[k]; !ok {
			bad(pointerJoin(ptr, k), "is required")
		}
	}
	if lo, ok := intOf(m["minProperties"]); ok && len(obj) < lo {
		bad(ptr, "must have at least %d keys", lo)
	}
	if hi, ok := intOf(m["maxProperties"]); ok && len(obj) > hi {
		bad(ptr, "must have at most %d keys", hi)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		kp := pointerJoin(ptr, k)
		if pn, ok := m["propertyNames"]; ok {
			var kerrs []fieldError
			d.validate(pn, k, kp, &kerrs)
			if len(kerrs) > 0 {
				bad(kp, "invalid key: %s", kerrs[0].Detail)
				continue
			}
		}
		if ps, ok := props[k]; ok {
			d.validate(ps, obj[k], kp, errs)
			continue
		}
		switch ap := m["additionalProperties"].(type) {
		case bool:
			if !ap {
				bad(kp, "unknown key")
			}
		case map[string]any:
			d.validate(ap, obj[k], kp, errs)
		}
	}
}

func (d *specDoc) validateArray(m map[string]any, arr []any, ptr string, errs *[]fieldError, bad func(string, string, ...any)) {
	if lo, ok := intOf(m["minItems"]); ok && len(arr) < lo {
		bad(ptr, "must have at least %d items", lo)
	}
	if hi, ok := intOf(m["maxItems"]); ok && len(arr) > hi {
		bad(ptr, "must have at most %d items", hi)
	}
	if u, _ := m["uniqueItems"].(bool); u {
		seen := map[string]bool{}
		for i, it := range arr {
			b, _ := json.Marshal(it)
			if seen[string(b)] {
				bad(pointerJoin(ptr, strconv.Itoa(i)), "duplicates an earlier item")
			}
			seen[string(b)] = true
		}
	}
	if is, ok := m["items"]; ok {
		for i, it := range arr {
			d.validate(is, it, pointerJoin(ptr, strconv.Itoa(i)), errs)
		}
	}
}

func (d *specDoc) validateOneOf(alts []any, v any, ptr string, errs *[]fieldError, bad func(string, string, ...any)) {
	switch n := d.countValid(alts, v); {
	case n == 1:
	case n > 1:
		bad(ptr, "matches more than one allowed form")
	default:
		// The common case is "X or null": report X's errors.
		var nonNull []any
		for _, a := range alts {
			if am, ok := d.deref(a).(map[string]any); !ok || am["type"] != "null" {
				nonNull = append(nonNull, a)
			}
		}
		if len(nonNull) == 1 {
			d.validate(nonNull[0], v, ptr, errs)
			return
		}
		bad(ptr, "does not match any allowed form")
	}
}

func (d *specDoc) countValid(alts []any, v any) int {
	n := 0
	for _, a := range alts {
		var e []fieldError
		d.validate(a, v, "", &e)
		if len(e) == 0 {
			n++
		}
	}
	return n
}

// anyOfDetail names the alternatives of an anyOf made of required lists
// (ReplayRequest), or says that none matched.
func anyOfDetail(d *specDoc, alts []any) string {
	var forms []string
	for _, a := range alts {
		am, _ := d.deref(a).(map[string]any)
		req := listOf(am["required"])
		if len(am) != 1 || len(req) == 0 {
			return "does not match any allowed form"
		}
		keys := make([]string, len(req))
		for i, r := range req {
			keys[i] = str(r)
		}
		forms = append(forms, strings.Join(keys, ", "))
	}
	return "must have " + strings.Join(forms, ", or ")
}

func listOf(v any) []any {
	l, _ := v.([]any)
	return l
}

func intOf(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

func numOf(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func typeMatches(t, v any) bool {
	if l, ok := t.([]any); ok {
		return slices.ContainsFunc(l, func(x any) bool { return typeMatches(x, v) })
	}
	switch str(t) {
	case "null":
		return v == nil
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "number":
		_, ok := v.(json.Number)
		return ok
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		_, err := strconv.ParseInt(string(n), 10, 64)
		return err == nil
	}
	return true
}

func typeName(t any) string {
	if l, ok := t.([]any); ok {
		names := make([]string, len(l))
		for i, x := range l {
			names[i] = typeName(x)
		}
		return strings.Join(names, " or ")
	}
	switch s := str(t); s {
	case "integer", "array", "object":
		return "an " + s
	case "null":
		return "null"
	default:
		return "a " + s
	}
}

// jsonEqualValue compares an enum entry (from YAML) with a JSON value.
func jsonEqualValue(e, v any) bool {
	switch x := v.(type) {
	case nil:
		return e == nil
	case json.Number:
		f, err := x.Float64()
		g, ok := numOf(e)
		return err == nil && ok && f == g
	default:
		return fmt.Sprint(e) == fmt.Sprint(v) && fmt.Sprintf("%T", e) == fmt.Sprintf("%T", v)
	}
}
