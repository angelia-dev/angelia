package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
)

// checkParams validates the path, query and header parameters against the
// operation's specification. An unknown query parameter is 400. An invalid
// path parameter names no resource: 404, except on PUT, which would create
// it (422).
func (s *Server) checkParams(r *http.Request, op *operation, vars map[string]string) *ProblemDetail {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return problem(SlugBadRequest, "the query string is malformed")
	}
	known := map[string]bool{}
	for _, p := range op.Params {
		if p.In == "query" {
			known[p.Name] = true
		}
	}
	names := make([]string, 0, len(query))
	for k := range query {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		if !known[k] {
			return problem(SlugBadRequest, "unknown query parameter %q", truncate(k, 64))
		}
	}
	if known["consumer"] && query.Has("consumer") {
		for _, k := range consumerExclusive {
			if query.Has(k) {
				return problem(SlugBadRequest, "query parameter consumer: cannot be combined with after, watch, type or source")
			}
		}
	}
	for _, p := range op.Params {
		var vals []string
		switch p.In {
		case "path":
			vals = []string{vars[p.Name]}
		case "query":
			vals = query[p.Name]
		case "header":
			vals = r.Header.Values(p.Name)
		}
		if len(vals) == 0 {
			if p.Required {
				return problem(SlugBadRequest, "%s parameter %s is required", p.In, p.Name)
			}
			continue
		}
		v, detail := s.coerce(p.Schema, vals)
		if detail == "" {
			var errs []fieldError
			s.spec.validate(p.Schema, v, "", &errs)
			if len(errs) > 0 {
				detail = errs[0].Detail
			}
		}
		if detail == "" {
			continue
		}
		if p.In == "path" {
			if op.Method == http.MethodPut {
				return validationProblem("request", []fieldError{{Pointer: "/" + p.Name, Detail: "the " + p.Name + " in the path " + detail}})
			}
			return problem(SlugNotFound, "no resource with this %s", p.Name)
		}
		return problem(SlugBadRequest, "%s parameter %s: %s", p.In, p.Name, detail)
	}
	return nil
}

// consumerExclusive are the query parameters that cannot be present with
// consumer, whatever their value: after=0 and an empty watch= too (spec
// docs/api.md decision 16).
var consumerExclusive = []string{"after", "watch", "type", "source"}

// coerce turns the string values of a parameter into the JSON value its
// schema describes, so that the schema validator can check it.
func (s *Server) coerce(schema any, vals []string) (any, string) {
	m, _ := s.spec.deref(schema).(map[string]any)
	if str(m["type"]) == "array" {
		out := make([]any, len(vals))
		for i, v := range vals {
			x, detail := s.coerceOne(m["items"], v)
			if detail != "" {
				return nil, detail
			}
			out[i] = x
		}
		return out, ""
	}
	if len(vals) > 1 {
		return nil, "must be given once"
	}
	return s.coerceOne(schema, vals[0])
}

func (s *Server) coerceOne(schema any, v string) (any, string) {
	m, _ := s.spec.deref(schema).(map[string]any)
	switch str(m["type"]) {
	case "integer":
		if _, err := strconv.ParseInt(v, 10, 64); err != nil {
			return nil, "must be an integer"
		}
		return json.Number(v), ""
	case "boolean":
		b, err := strconv.ParseBool(v)
		if err != nil || (v != "true" && v != "false") {
			return nil, "must be true or false"
		}
		return b, ""
	}
	return v, ""
}

// truncate cuts s to at most n bytes, on a character boundary, and marks
// the cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return truncateUTF8(s, n) + "..."
}
