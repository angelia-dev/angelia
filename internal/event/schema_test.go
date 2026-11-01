package event

// A test-only validator for the subset of JSON Schema 2020-12 that the
// vendored spec schemas use (testdata/spec). architecture.md §6 lists no JSON
// Schema library, so none is added; TestValidatorAgainstSpecExamples checks
// this validator against the spec's own valid and invalid examples, and an
// unknown keyword fails loudly instead of being skipped.

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const (
	dataSchemaID     = "https://github.com/angelia-dev/spec/schemas/release/v1.json"
	envelopeSchemaID = "https://github.com/angelia-dev/spec/schemas/release/envelope/v1.json"
)

// specFS holds the vendored spec files (testdata/spec/SOURCE).
var specFS = os.DirFS("testdata/spec")

type schemaSet struct {
	docs map[string]any // by $id
	res  map[string]*regexp.Regexp
}

func loadSchemas(t *testing.T) *schemaSet {
	t.Helper()
	s := &schemaSet{docs: map[string]any{}, res: map[string]*regexp.Regexp{}}
	for _, p := range []string{"schemas/release/v1.json", "schemas/release/envelope/v1.json"} {
		b, err := fs.ReadFile(specFS, p)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		walkKeywords(doc, p)
		s.docs[doc["$id"].(string)] = doc
	}
	return s
}

// validate returns the violations of inst against the schema with the given
// $id; none means valid.
func (s *schemaSet) validate(id string, inst any) []string {
	return s.check(id, s.docs[id], inst, "$")
}

// annotations are keywords without assertions.
var annotations = []string{"$schema", "$id", "title", "description", "$comment", "examples", "$defs"}

func (s *schemaSet) check(base string, schema, inst any, at string) []string {
	switch sc := schema.(type) {
	case bool:
		if !sc {
			return []string{at + ": false schema"}
		}
		return nil
	case map[string]any:
		var errs []string
		fail := func(format string, a ...any) { errs = append(errs, at+": "+fmt.Sprintf(format, a...)) }
		obj, isObj := inst.(map[string]any)
		str, isStr := inst.(string)
		for kw, val := range sc {
			switch kw {
			case "$ref":
				target, tbase := s.resolve(base, val.(string))
				errs = append(errs, s.check(tbase, target, inst, at)...)
			case "type":
				types, ok := val.([]any)
				if !ok {
					types = []any{val}
				}
				if !slices.ContainsFunc(types, func(t any) bool { return hasType(inst, t.(string)) }) {
					fail("type %v, got %T", val, inst)
				}
			case "enum":
				if !slices.ContainsFunc(val.([]any), func(e any) bool { return reflect.DeepEqual(e, inst) }) {
					fail("%v not in enum %v", inst, val)
				}
			case "const":
				if !reflect.DeepEqual(val, inst) {
					fail("%v is not const %v", inst, val)
				}
			case "pattern":
				if isStr && !s.re(val.(string)).MatchString(str) {
					fail("%q does not match %s", str, val)
				}
			case "minLength":
				if isStr && utf8.RuneCountInString(str) < int(val.(float64)) {
					fail("shorter than %v", val)
				}
			case "maxLength":
				if isStr && utf8.RuneCountInString(str) > int(val.(float64)) {
					fail("longer than %v", val)
				}
			case "format":
				if isStr && !formatOK(val.(string), str) {
					fail("%q is not a %s", str, val)
				}
			case "anyOf":
				if !slices.ContainsFunc(val.([]any), func(b any) bool { return len(s.check(base, b, inst, at)) == 0 }) {
					fail("matches no anyOf branch")
				}
			case "required":
				for _, r := range val.([]any) {
					if _, ok := obj[r.(string)]; isObj && !ok {
						fail("missing required %q", r)
					}
				}
			case "maxProperties":
				if isObj && len(obj) > int(val.(float64)) {
					fail("more than %v properties", val)
				}
			case "propertyNames":
				for k := range obj {
					errs = append(errs, s.check(base, val, k, at+".<name "+k+">")...)
				}
			case "properties":
				props := val.(map[string]any)
				for k, v := range obj {
					if p, ok := props[k]; ok {
						errs = append(errs, s.check(base, p, v, at+"."+k)...)
					}
				}
			case "patternProperties":
				for pat, p := range val.(map[string]any) {
					for k, v := range obj {
						if s.re(pat).MatchString(k) {
							errs = append(errs, s.check(base, p, v, at+"."+k)...)
						}
					}
				}
			case "additionalProperties":
				props, _ := sc["properties"].(map[string]any)
				pats, _ := sc["patternProperties"].(map[string]any)
				for k, v := range obj {
					if _, ok := props[k]; ok {
						continue
					}
					matched := false
					for pat := range pats {
						matched = matched || s.re(pat).MatchString(k)
					}
					if !matched {
						errs = append(errs, s.check(base, val, v, at+"."+k)...)
					}
				}
			default:
				if !slices.Contains(annotations, kw) {
					panic("schema keyword not supported by the test validator: " + kw)
				}
			}
		}
		return errs
	}
	panic(fmt.Sprintf("schema of type %T", schema))
}

func hasType(v any, t string) bool {
	switch t {
	case "null":
		return v == nil
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	}
	panic("type not supported by the test validator: " + t)
}

func formatOK(format, s string) bool {
	switch format {
	case "date-time":
		_, err := time.Parse(time.RFC3339Nano, s)
		return err == nil
	case "uri":
		u, err := url.Parse(s)
		return err == nil && u.IsAbs()
	}
	panic("format not supported by the test validator: " + format)
}

// re compiles an ECMA-262 pattern of the schemas as RE2.
func (s *schemaSet) re(p string) *regexp.Regexp {
	if r, ok := s.res[p]; ok {
		return r
	}
	r := regexp.MustCompile(ecmaToRE2(p))
	s.res[p] = r
	return r
}

// ecmaSpace is the body of a class matching ECMA-262 \s (white space and
// line terminators); RE2's \s is only [\t\n\f\r ].
const ecmaSpace = `\s\v\p{Zs}\x{FEFF}\x{2028}\x{2029}`

// ecmaToRE2 rewrites the ECMA-262 escapes whose RE2 meaning differs: \uXXXX,
// \s and \S. A \S inside a class has no RE2 form and panics.
func ecmaToRE2(p string) string {
	var b strings.Builder
	inClass := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '\\' && i+1 < len(p):
			e := p[i+1]
			i++
			switch {
			case e == 'u' && i+4 < len(p):
				b.WriteString(`\x{` + p[i+1:i+5] + `}`)
				i += 4
			case e == 's' && inClass:
				b.WriteString(ecmaSpace)
			case e == 's':
				b.WriteString("[" + ecmaSpace + "]")
			case e == 'S' && inClass:
				panic(`\S inside a character class: ` + p)
			case e == 'S':
				b.WriteString("[^" + ecmaSpace + "]")
			default:
				b.WriteByte('\\')
				b.WriteByte(e)
			}
		case c == '[' && !inClass:
			inClass = true
			b.WriteByte(c)
		case c == ']' && inClass:
			inClass = false
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// supported are the keywords the validator asserts or ignores knowingly.
var supported = append([]string{
	"$ref", "type", "enum", "const", "pattern", "minLength", "maxLength", "format", "anyOf", "required",
	"maxProperties", "propertyNames", "properties", "patternProperties", "additionalProperties",
}, annotations...)

// walkKeywords visits every subschema of a schema document, including those
// no instance reaches, and panics on a keyword the validator does not know.
func walkKeywords(schema any, at string) {
	sc, ok := schema.(map[string]any)
	if !ok {
		if _, isBool := schema.(bool); !isBool {
			panic(fmt.Sprintf("%s: schema of type %T", at, schema))
		}
		return
	}
	for kw, val := range sc {
		if !slices.Contains(supported, kw) {
			panic(at + ": schema keyword not supported by the test validator: " + kw)
		}
		switch kw {
		case "properties", "patternProperties", "$defs":
			for k, sub := range val.(map[string]any) {
				walkKeywords(sub, at+"/"+kw+"/"+k)
			}
		case "anyOf":
			for i, sub := range val.([]any) {
				walkKeywords(sub, fmt.Sprintf("%s/anyOf/%d", at, i))
			}
		case "additionalProperties", "propertyNames":
			walkKeywords(val, at+"/"+kw)
		}
	}
}

// resolve finds the schema a $ref names, relative to the document base, and
// returns it with its own document's base.
func (s *schemaSet) resolve(base, ref string) (any, string) {
	b, _ := url.Parse(base)
	r, err := url.Parse(ref)
	if err != nil {
		panic(err)
	}
	u := b.ResolveReference(r)
	frag := u.Fragment
	u.Fragment = ""
	doc, ok := s.docs[u.String()]
	if !ok {
		panic("unresolvable $ref " + ref + " from " + base)
	}
	node := doc
	for _, seg := range strings.Split(strings.TrimPrefix(frag, "/"), "/") {
		if seg == "" {
			continue
		}
		node = node.(map[string]any)[seg]
	}
	return node, u.String()
}

func decode(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return v
}

// TestValidatorAgainstSpecExamples runs the spec repository's own examples:
// every valid one must pass and every invalid one fail, as the spec's CI
// checks with check-jsonschema.
func TestValidatorAgainstSpecExamples(t *testing.T) {
	s := loadSchemas(t)
	var n int
	err := fs.WalkDir(specFS, "examples", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(p, "examples/")
		parts := strings.Split(rel, "/")
		wantValid := parts[0] == "valid"
		id := dataSchemaID
		if parts[2] == "envelope" {
			id = envelopeSchemaID
		}
		b, err := fs.ReadFile(specFS, p)
		if err != nil {
			return err
		}
		errs := s.validate(id, decode(t, b))
		if wantValid && len(errs) > 0 {
			t.Errorf("%s: valid example rejected: %v", rel, errs)
		}
		if !wantValid && len(errs) == 0 {
			t.Errorf("%s: invalid example accepted", rel)
		}
		n++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 40 {
		t.Fatalf("checked %d examples, want the 41 vendored ones", n)
	}
}

// The validator follows ECMA-262 \s and \S, as check-jsonschema does: Unicode
// white space is white space, not a character of a URL or a package.
func TestValidatorECMAWhiteSpace(t *testing.T) {
	s := loadSchemas(t)
	b, err := fs.ReadFile(specFS, "examples/valid/release/v1/published-pypi.json")
	if err != nil {
		t.Fatal(err)
	}
	urls := func(d map[string]any) map[string]any { return d["urls"].(map[string]any) }
	for name, mod := range map[string]func(d map[string]any){
		"url with NBSP":       func(d map[string]any) { urls(d)["release"] = "https://a.org/x\u00a0y" },
		"url with LS":         func(d map[string]any) { urls(d)["release"] = "https://a.org/x\u2028y" },
		"url with VT":         func(d map[string]any) { urls(d)["changelog"] = "https://a.org/x\vy" },
		"host with BOM":       func(d map[string]any) { urls(d)["release"] = "https://a\ufeff.org/" },
		"package space":       func(d map[string]any) { d["package"] = " " },
		"package NBSP only":   func(d map[string]any) { d["package"] = "\u00a0" },
		"package ideographic": func(d map[string]any) { d["package"] = "\u3000\u3000" },
	} {
		d := decode(t, b).(map[string]any)
		if errs := s.validate(dataSchemaID, d); len(errs) > 0 {
			t.Fatalf("base example invalid: %v", errs)
		}
		mod(d)
		if errs := s.validate(dataSchemaID, d); len(errs) == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
	want := `^[^` + ecmaSpace + `/?#]+[^` + ecmaSpace + `]\x{0000}$`
	if got := ecmaToRE2(`^[^\s/?#]+\S\u0000$`); got != want {
		t.Errorf("ecmaToRE2 = %s, want %s", got, want)
	}
}

func TestWalkKeywordsFailsLoudly(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("an unsupported keyword in an unvisited subschema did not panic")
		}
	}()
	walkKeywords(map[string]any{"$defs": map[string]any{"x": map[string]any{"oneOf": []any{}}}}, "test")
}
