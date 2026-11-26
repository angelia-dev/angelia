package httpjson

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func doc(t *testing.T, s string) any {
	t.Helper()
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(b.String())
}

// Cases follow the RFC 9535 examples (§1.5, §2.3) and the compliance test
// suite (jsonpath-standard/jsonpath-compliance-test-suite) for the
// selectors of the subset.
func TestSelect(t *testing.T) {
	const store = `{"store":{"book":[{"author":"A","price":8.95},{"author":"B","price":12.99},{"author":"C","isbn":"0-553"}],"bicycle":{"color":"red","price":399}}}`
	cases := []struct{ doc, path, want string }{
		{`{"a":[1]}`, `$`, `[{"a":[1]}]`},
		{store, `$.store.book[*].author`, `["A","B","C"]`},
		{store, `$.store.book[0].author`, `["A"]`},
		{store, `$.store.book[-1].author`, `["C"]`},
		{store, `$.store.book[-3].author`, `["A"]`},
		{store, `$.store.book[-4].author`, `[]`},
		{store, `$.store.book[3]`, `[]`},
		{store, `$.store.bicycle.*`, `["red",399]`}, // sorted member names
		{store, `$['store']["bicycle"]['color']`, `["red"]`},
		{store, `$.store.book[*].isbn`, `["0-553"]`},
		{store, `$.store.*.price`, `[399]`},
		{store, `$.store.book.*.price`, `[8.95,12.99]`},
		{store, `$ .store [ 'bicycle' ] .color`, `["red"]`},
		{store, "$[\t'store'\n]['bicycle']", `[{"color":"red","price":399}]`},
		{store, `$.missing`, `[]`},
		{store, `$.store.book.author`, `[]`},
		{store, `$.store.bicycle[0]`, `[]`},
		{`{"o":{"j":1,"k":2},"a":[5,3]}`, `$[*]`, `[[5,3],{"j":1,"k":2}]`},
		{`{"o":{"j":1,"k":2},"a":[5,3]}`, `$.*.*`, `[5,3,1,2]`},
		{`[1,2,3]`, `$[*]`, `[1,2,3]`},
		{`[1,2,3]`, `$.*`, `[1,2,3]`},
		{`"x"`, `$[*]`, `[]`},
		{`{"a b":1,"'":2,"\"":3,"\\":4,"\u00e9":5,"\ud834\udd1e":6,"\n":7,"/":8}`, `$['a b']`, `[1]`},
		{`{"'":2}`, `$['\'']`, `[2]`},
		{`{"'":2}`, `$["'"]`, `[2]`},
		{`{"\"":3}`, `$["\""]`, `[3]`},
		{`{"\"":3}`, `$['"']`, `[3]`},
		{`{"\\":4}`, `$['\\']`, `[4]`},
		{`{"é":5}`, `$['\u00e9']`, `[5]`},
		{`{"é":5}`, `$['\u00E9']`, `[5]`},
		{`{"é":5}`, `$.é`, `[5]`},
		{`{"𝄞":6}`, `$['\uD834\uDD1E']`, `[6]`},
		{`{"\n\t\b\f\r":7}`, `$['\n\t\b\f\r']`, `[7]`},
		{`{"/":8}`, `$['\/']`, `[8]`},
		{`{"_a1":9}`, `$._a1`, `[9]`},
		{`{"a":{"b":[{"c":"v1"},{"c":"v2"}]}}`, `$.a.b[*].c`, `["v1","v2"]`},
		{`{"v":1.10}`, `$.v`, `[1.10]`},
		{`{"k":"v"}`, `$['']`, `[]`},
		{`{"":"e"}`, `$['']`, `["e"]`},
	}
	for _, c := range cases {
		p, err := ParsePath(c.path)
		if err != nil {
			t.Errorf("ParsePath(%q): %v", c.path, err)
			continue
		}
		got := p.Select(doc(t, c.doc))
		if got == nil {
			got = []any{}
		}
		if s := asJSON(t, got); s != c.want {
			t.Errorf("%s on %s = %s, want %s", c.path, c.doc, s, c.want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		``:                           "empty",
		`@.a`:                        "byte 0: must start with $",
		`a`:                          "byte 0",
		`$..a`:                       "byte 1: descendant",
		`$..[0]`:                     "descendant",
		`$[?@.a]`:                    "byte 2: filter",
		`$[?(@.a=='x')]`:             "filter",
		`$[1:2]`:                     "byte 3: array slices",
		`$[:2]`:                      "byte 2: array slices",
		`$[::]`:                      "slices",
		`$[0,1]`:                     "byte 3: unions",
		`$['a','b']`:                 "unions",
		`$.length()`:                 "byte 8: function",
		`$[length(@)]`:               "expected a quoted name",
		`$.a.`:                       "byte 4: expected a member name",
		`$.1a`:                       "byte 2: a member name",
		`$.-a`:                       "a member name",
		`$.a b`:                      "byte 4: expected . or [",
		`$ `:                         "byte 1: trailing white space",
		`$.a `:                       "trailing white space",
		`$[01]`:                      "leading zero",
		`$[-0]`:                      "-0",
		`$[-]`:                       "expected digits",
		`$[9007199254740992]`:        "out of range",
		`$[-9007199254740992]`:       "out of range",
		`$[99999999999999999999999]`: "out of range",
		`$[0`:                        "missing ]",
		`$[`:                         "missing selector",
		`$[0 1]`:                     "expected ]",
		`$['a`:                       "unterminated string",
		`$['a\`:                      "unterminated escape",
		`$['\x']`:                    "invalid escape",
		`$["\'"]`:                    "invalid escape",
		`$['\"']`:                    "invalid escape",
		`$['\u12']`:                  "4 hex digits",
		`$['\u12G4']`:                "4 hex digits",
		`$['\uDC00']`:                "lone low surrogate",
		`$['\uD834']`:                "without a low surrogate",
		`$['\uD834\u0041']`:          "without a low surrogate",
		"$['a\tb']":                  "control character",
		`$[a]`:                       "expected a quoted name",
		`$.store[*`:                  "missing ]",
		"$.\xff":                     "UTF-8",
		`$` + strings.Repeat(".a", MaxSegments+1): "more than 32 segments",
		`$.` + strings.Repeat("a", MaxPathBytes):  "longer than 512 bytes",
	}
	for expr, want := range cases {
		_, err := ParsePath(expr)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParsePath(%q) = %v, want an error containing %q", expr, err, want)
		}
	}
	if _, err := ParsePath(`$` + strings.Repeat(".a", MaxSegments)); err != nil {
		t.Errorf("%d segments: %v", MaxSegments, err)
	}
	if _, err := ParsePath(`$[9007199254740991]`); err != nil {
		t.Errorf("max index: %v", err)
	}
}

func TestSelectWithoutUseNumber(t *testing.T) {
	var v any
	if err := json.Unmarshal([]byte(`{"a":[1.5]}`), &v); err != nil {
		t.Fatal(err)
	}
	p, _ := ParsePath(`$.a[0]`)
	if got := p.Select(v); !reflect.DeepEqual(got, []any{1.5}) || typeName(got[0]) != "number" {
		t.Errorf("got %v", got)
	}
}

func TestTypeName(t *testing.T) {
	for v, want := range map[string]string{`null`: "null", `true`: "boolean", `1`: "number", `"s"`: "string", `[]`: "array", `{}`: "object"} {
		if got := typeName(doc(t, v)); got != want {
			t.Errorf("typeName(%s) = %s", v, got)
		}
	}
}
