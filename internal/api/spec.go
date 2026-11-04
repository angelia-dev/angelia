package api

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/angelia-dev/angelia/api/openapi"
)

// specDoc is the vendored OpenAPI document (api/openapi), read once at start.
// The server routes, authorizes and validates requests from it, so the
// specification and the server cannot disagree on a scope, a parameter or a
// body rule.
type specDoc struct {
	root map[string]any
	ops  []*operation
}

// operation is one operation of the specification.
type operation struct {
	ID     string
	Method string
	Path   string
	segs   []string
	// Scope is the scope its security requirement names.
	Scope Scope
	// Params are the path-level and operation-level parameters.
	Params []*param
	// Body is the schema of the application/json request body, or nil.
	Body         any
	BodyRequired bool
	// Statuses are the response codes the operation declares.
	Statuses map[int]bool
}

type param struct {
	Name     string
	In       string // path, query or header
	Required bool
	Schema   any
}

// loadSpec parses the embedded specification.
func loadSpec() (*specDoc, error) {
	return parseSpec(openapi.Spec)
}

func parseSpec(data []byte) (*specDoc, error) {
	d := &specDoc{}
	if err := yaml.Unmarshal(data, &d.root); err != nil {
		return nil, fmt.Errorf("api: parse specification: %w", err)
	}
	paths, _ := d.root["paths"].(map[string]any)
	defScope := scopeOf(d.root["security"])
	keys := make([]string, 0, len(paths))
	for k := range paths {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, path := range keys {
		item, _ := paths[path].(map[string]any)
		shared, err := d.params(item["parameters"])
		if err != nil {
			return nil, err
		}
		for _, method := range []string{"get", "put", "post", "delete", "patch"} {
			raw, ok := item[method].(map[string]any)
			if !ok {
				continue
			}
			op := &operation{
				ID: str(raw["operationId"]), Method: strings.ToUpper(method), Path: path,
				segs: strings.Split(strings.TrimPrefix(path, "/"), "/"), Scope: defScope, Statuses: map[int]bool{},
			}
			if s, ok := raw["security"]; ok {
				op.Scope = scopeOf(s)
			}
			if op.Scope == ScopeNone {
				return nil, fmt.Errorf("api: operation %s names no known scope", op.ID)
			}
			own, err := d.params(raw["parameters"])
			if err != nil {
				return nil, err
			}
			op.Params = mergeParams(shared, own)
			if rb, ok := d.deref(raw["requestBody"]).(map[string]any); ok {
				op.BodyRequired, _ = rb["required"].(bool)
				if c, ok := rb["content"].(map[string]any); ok {
					if mt, ok := c["application/json"].(map[string]any); ok {
						op.Body = mt["schema"]
					}
				}
				if op.Body == nil {
					return nil, fmt.Errorf("api: operation %s: request body without an application/json schema", op.ID)
				}
			}
			responses, _ := raw["responses"].(map[string]any)
			for code := range responses {
				n, err := strconv.Atoi(code)
				if err != nil {
					return nil, fmt.Errorf("api: operation %s: response %q is not a status code", op.ID, code)
				}
				op.Statuses[n] = true
			}
			d.ops = append(d.ops, op)
		}
	}
	if len(d.ops) == 0 {
		return nil, fmt.Errorf("api: the specification has no operations")
	}
	if err := checkPatterns(any(d.root), "#"); err != nil {
		return nil, err
	}
	return d, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// scopeOf reads the bearerAuth scope of a security requirement list.
func scopeOf(v any) Scope {
	reqs, _ := v.([]any)
	for _, r := range reqs {
		m, _ := r.(map[string]any)
		scopes, _ := m["bearerAuth"].([]any)
		for _, s := range scopes {
			if sc, ok := ParseScope(str(s)); ok {
				return sc
			}
		}
	}
	return ScopeNone
}

func (d *specDoc) params(v any) ([]*param, error) {
	list, _ := v.([]any)
	out := make([]*param, 0, len(list))
	for _, raw := range list {
		m, ok := d.deref(raw).(map[string]any)
		if !ok {
			return nil, fmt.Errorf("api: unresolvable parameter %v", raw)
		}
		p := &param{Name: str(m["name"]), In: str(m["in"]), Schema: m["schema"]}
		p.Required, _ = m["required"].(bool)
		out = append(out, p)
	}
	return out, nil
}

func mergeParams(shared, own []*param) []*param {
	out := slices.Clone(own)
	for _, p := range shared {
		if !slices.ContainsFunc(own, func(o *param) bool { return o.Name == p.Name && o.In == p.In }) {
			out = append(out, p)
		}
	}
	return out
}

// deref follows a local $ref ("#/components/..."). Anything else is returned
// as it is; an external $ref (the event envelope schema) is not followed.
func (d *specDoc) deref(v any) any {
	for range 16 { // refs to refs, bounded
		m, ok := v.(map[string]any)
		if !ok {
			return v
		}
		ref, ok := m["$ref"].(string)
		if !ok || !strings.HasPrefix(ref, "#/") {
			return v
		}
		v = d.lookup(ref)
		if v == nil {
			return nil
		}
	}
	return v
}

// lookup resolves a JSON Pointer fragment ("#/components/schemas/Name").
func (d *specDoc) lookup(ref string) any {
	var cur any = d.root
	for _, tok := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[tok]
	}
	return cur
}

// schema returns the component schema with this name.
func (d *specDoc) schema(name string) any { return d.lookup("#/components/schemas/" + name) }

// match finds the operation for a method and an escaped request path. A
// path with only literal segments wins over one with parameters
// (/v1/events/stream over /v1/events/{id}). allowed lists the methods of the
// path when no operation has this method.
func (d *specDoc) match(method, escapedPath string) (op *operation, vars map[string]string, allowed []string) {
	segs := strings.Split(strings.TrimPrefix(escapedPath, "/"), "/")
	bestLiterals := -1
	var bestPath string
	for _, o := range d.ops {
		lits, ok := matchSegs(o.segs, segs)
		if !ok || lits < bestLiterals {
			continue
		}
		if lits > bestLiterals {
			bestLiterals, bestPath, op, allowed = lits, o.Path, nil, nil
		}
		if o.Path != bestPath {
			continue
		}
		if o.Method == method {
			op = o
		} else {
			allowed = append(allowed, o.Method)
		}
	}
	if op == nil {
		return nil, nil, allowed
	}
	vars = map[string]string{}
	for i, s := range op.segs {
		if name, ok := strings.CutPrefix(s, "{"); ok {
			v, err := url.PathUnescape(segs[i])
			if err != nil {
				v = segs[i]
			}
			vars[strings.TrimSuffix(name, "}")] = v
		}
	}
	return op, vars, nil
}

func matchSegs(pattern, segs []string) (literals int, ok bool) {
	if len(pattern) != len(segs) {
		return 0, false
	}
	for i, p := range pattern {
		switch {
		case strings.HasPrefix(p, "{"):
			if segs[i] == "" {
				return 0, false
			}
		case p == segs[i]:
			literals++
		default:
			return 0, false
		}
	}
	return literals, true
}

// operation returns the operation with this id, or nil.
func (d *specDoc) operation(id string) *operation {
	for _, o := range d.ops {
		if o.ID == id {
			return o
		}
	}
	return nil
}
