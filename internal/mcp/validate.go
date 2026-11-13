package mcp

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// fieldError is one item of the errors of invalid-arguments: the same
// {pointer, detail} shape as the REST problem validation-failed.
type fieldError struct {
	Pointer string `json:"pointer"`
	Detail  string `json:"detail"`
}

// validator checks tool arguments against the contract's inputSchema and
// reports every failure at a JSON Pointer into the arguments. jsonschema-go
// stops at the first error and names schema paths, not instance paths, so
// the validator walks the object levels itself (required, properties,
// additionalProperties, min/maxProperties, dependentSchemas) and hands every
// other value to jsonschema-go. A last check of the whole schema catches a
// rule the walk does not know.
type validator struct {
	root   *jsonschema.Resolved
	tree   *jsonschema.Schema
	leaves map[*jsonschema.Schema]*jsonschema.Resolved
}

func newValidator(raw json.RawMessage) (*validator, error) {
	var root, tree jsonschema.Schema
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("input schema: %w", err)
	}
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("input schema: %w", err)
	}
	rr, err := root.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("input schema: %w", err)
	}
	v := &validator{root: rr, tree: &tree, leaves: map[*jsonschema.Schema]*jsonschema.Resolved{}}
	if err := v.prepare(&tree); err != nil {
		return nil, err
	}
	return v, nil
}

// objectNode reports whether the walk handles s itself.
func objectNode(s *jsonschema.Schema) bool {
	return s.Type == "object" && s.Properties != nil
}

// isFalse reports whether s is the schema false (additionalProperties: false).
func isFalse(s *jsonschema.Schema) bool {
	b, err := json.Marshal(s)
	return err == nil && string(b) == "false"
}

// prepare resolves, once, every schema that the walk hands to jsonschema-go.
func (v *validator) prepare(s *jsonschema.Schema) error {
	if !objectNode(s) {
		return v.leaf(s)
	}
	for _, p := range s.Properties {
		if err := v.prepare(p); err != nil {
			return err
		}
	}
	if s.AdditionalProperties != nil && !isFalse(s.AdditionalProperties) {
		if err := v.prepare(s.AdditionalProperties); err != nil {
			return err
		}
	}
	for _, d := range s.DependentSchemas {
		if err := v.leaf(d); err != nil {
			return err
		}
	}
	return nil
}

// leaf resolves a copy of s on its own: a subschema of the contract is
// self-contained (no $ref).
func (v *validator) leaf(s *jsonschema.Schema) error {
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("input schema: %w", err)
	}
	var c jsonschema.Schema
	if err := json.Unmarshal(b, &c); err != nil {
		return fmt.Errorf("input schema: %w", err)
	}
	r, err := c.Resolve(nil)
	if err != nil {
		return fmt.Errorf("input schema: %w", err)
	}
	v.leaves[s] = r
	return nil
}

// validate returns every failure of the arguments, or none.
func (v *validator) validate(raw json.RawMessage) []fieldError {
	var inst any
	if err := json.Unmarshal(raw, &inst); err != nil {
		return []fieldError{{Pointer: "", Detail: "the arguments are not valid JSON"}}
	}
	var errs []fieldError
	add := func(p, d string) { errs = append(errs, fieldError{Pointer: p, Detail: d}) }
	v.check("", v.tree, inst, add)
	if len(errs) == 0 {
		if err := v.root.Validate(inst); err != nil {
			add("", cleanError(err))
		}
	}
	return errs
}

func (v *validator) check(ptr string, s *jsonschema.Schema, inst any, add func(p, d string)) {
	if !objectNode(s) {
		// jsonschema-go words number bounds as fractions ("101/1").
		if n, ok := inst.(float64); ok {
			switch {
			case s.Maximum != nil && n > *s.Maximum:
				add(ptr, fmt.Sprintf("must be at most %g", *s.Maximum))
				return
			case s.Minimum != nil && n < *s.Minimum:
				add(ptr, fmt.Sprintf("must be at least %g", *s.Minimum))
				return
			}
		}
		if err := v.leaves[s].Validate(inst); err != nil {
			add(ptr, cleanError(err))
		}
		return
	}
	obj, ok := inst.(map[string]any)
	if !ok {
		add(ptr, "must be an object")
		return
	}
	for _, k := range s.Required {
		if _, ok := obj[k]; !ok {
			add(pointerJoin(ptr, k), "is required")
		}
	}
	keys := slices.Sorted(maps.Keys(obj))
	for _, k := range keys {
		switch ps, ok := s.Properties[k]; {
		case ok:
			v.check(pointerJoin(ptr, k), ps, obj[k], add)
		case s.AdditionalProperties != nil && isFalse(s.AdditionalProperties):
			add(pointerJoin(ptr, k), "unknown key")
		case s.AdditionalProperties != nil:
			v.check(pointerJoin(ptr, k), s.AdditionalProperties, obj[k], add)
		}
	}
	if s.MinProperties != nil && len(obj) < *s.MinProperties {
		add(ptr, fmt.Sprintf("needs at least %d keys, has %d", *s.MinProperties, len(obj)))
	}
	if s.MaxProperties != nil && len(obj) > *s.MaxProperties {
		add(ptr, fmt.Sprintf("allows at most %d keys, has %d", *s.MaxProperties, len(obj)))
	}
	for _, d := range slices.Sorted(maps.Keys(s.DependentSchemas)) {
		if _, ok := obj[d]; !ok {
			continue
		}
		ds := s.DependentSchemas[d]
		if ds.PropertyNames != nil && len(ds.PropertyNames.Enum) > 0 {
			for _, k := range keys {
				if !slices.Contains(ds.PropertyNames.Enum, any(k)) {
					add(pointerJoin(ptr, k), "cannot be combined with "+d)
				}
			}
			continue
		}
		if err := v.leaves[ds].Validate(obj); err != nil {
			add(ptr, cleanError(err))
		}
	}
}

// pointerJoin appends a reference token to a JSON Pointer (RFC 6901).
func pointerJoin(ptr, tok string) string {
	return ptr + "/" + strings.NewReplacer("~", "~0", "/", "~1").Replace(tok)
}

// cleanError drops jsonschema-go's "validating <schema path>: " prefixes,
// which name the schema, not the arguments.
func cleanError(err error) string {
	msg := err.Error()
	for strings.HasPrefix(msg, "validating ") {
		i := strings.Index(msg, ": ")
		if i < 0 {
			break
		}
		msg = msg[i+2:]
	}
	return msg
}
