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

// re compiles an ECMA-262 pattern of the schemas as RE2: the only difference
// they use is \uXXXX.
func (s *schemaSet) re(p string) *regexp.Regexp {
	if r, ok := s.res[p]; ok {
		return r
	}
	r := regexp.MustCompile(regexp.MustCompile(`\\u([0-9A-Fa-f]{4})`).ReplaceAllString(p, `\x{$1}`))
	s.res[p] = r
	return r
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
