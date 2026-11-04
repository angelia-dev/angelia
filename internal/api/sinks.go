package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

// Masked is the value of a configuration key the kind marks secret.
const Masked = "[REDACTED]"

// configSchema returns the config_schema of a sink kind with its local
// references inlined, nil when the kind is unknown or publishes none (or an
// unreadable one). A built-in kind that publishes none has the one the
// specification documents. complete is false when the schema holds a
// construct secretPaths cannot follow (unclassifiable): the caller then
// masks as if there were no schema, on top of the secrets it found.
func (s *Server) configSchema(kind string) (sch map[string]any, complete bool) {
	pl, ok := s.plugin("sink", kind)
	switch {
	case !ok:
		return nil, false
	case len(pl.ConfigSchema) == 0 && pl.Runtime == "builtin":
		sch = s.spec.kindConfig(kind)
		return sch, sch != nil
	case len(pl.ConfigSchema) == 0:
		return nil, false
	}
	if json.Unmarshal(pl.ConfigSchema, &sch) != nil {
		return nil, false
	}
	r := &schemaResolver{root: sch, complete: true}
	resolved, _ := r.inline(sch, 0).(map[string]any)
	return resolved, r.complete && resolved != nil
}

// unclassifiable are the keywords that can make a value secret in ways
// secretPaths does not follow.
var unclassifiable = []string{
	"if", "then", "else", "dependentSchemas", "unevaluatedProperties", "unevaluatedItems",
	"$dynamicRef", "$recursiveRef", "prefixItems",
}

// schemaResolver inlines the local references of a plugin's config_schema
// (#, #/$defs/..., #/definitions/...) and notes what it cannot classify.
type schemaResolver struct {
	root     map[string]any
	complete bool
}

func (r *schemaResolver) inline(v any, depth int) any {
	if depth > 32 { // a reference cycle, or a schema too deep to trust
		r.complete = false
		return nil
	}
	switch x := v.(type) {
	case map[string]any:
		for _, k := range unclassifiable {
			if _, ok := x[k]; ok {
				r.complete = false
			}
		}
		if ref, ok := x["$ref"].(string); ok {
			target, ok := r.lookup(ref)
			if !ok {
				r.complete = false
				return nil
			}
			merged := maps.Clone(target)
			for k, e := range x { // siblings of $ref apply too
				if k != "$ref" {
					merged[k] = e
				}
			}
			delete(merged, "$ref")
			if inner, ok := target["$ref"]; ok {
				merged["$ref"] = inner // a reference to a reference
			}
			return r.inline(merged, depth+1)
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			if k == "$defs" || k == "definitions" {
				continue // reached through references only
			}
			out[k] = r.inline(e, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = r.inline(e, depth+1)
		}
		return out
	}
	return v
}

// lookup resolves a local reference against the schema's root.
func (r *schemaResolver) lookup(ref string) (map[string]any, bool) {
	if ref == "#" {
		return r.root, true
	}
	if !strings.HasPrefix(ref, "#/") {
		return nil, false // another document: not followed
	}
	var cur any = r.root
	for _, tok := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur = m[tok]
	}
	m, ok := cur.(map[string]any)
	return m, ok
}

// patternStep marks a path element that is a patternProperties regex.
const patternStep = "\x00re:"

// secretPaths returns the paths of the configuration values a schema marks
// with x-angelia-secret: true (spec docs/api.md decision 26), at any depth:
// under properties, under patternProperties (each key its regex matches),
// and "*" for every item of an array or every value of an
// additionalProperties map. The schema has its references inlined
// (configSchema).
func secretPaths(sch any, prefix []string, depth int) [][]string {
	m, ok := sch.(map[string]any)
	if !ok || depth > 64 {
		return nil
	}
	if b, _ := m["x-angelia-secret"].(bool); b && len(prefix) > 0 {
		return [][]string{slices.Clone(prefix)}
	}
	var out [][]string
	if props, ok := m["properties"].(map[string]any); ok {
		for _, k := range slices.Sorted(maps.Keys(props)) {
			out = append(out, secretPaths(props[k], append(slices.Clone(prefix), k), depth+1)...)
		}
	}
	if pats, ok := m["patternProperties"].(map[string]any); ok {
		for _, p := range slices.Sorted(maps.Keys(pats)) {
			out = append(out, secretPaths(pats[p], append(slices.Clone(prefix), patternStep+p), depth+1)...)
		}
	}
	for _, key := range []string{"items", "additionalProperties", "contains"} {
		out = append(out, secretPaths(m[key], append(slices.Clone(prefix), "*"), depth+1)...)
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf"} {
		if subs, ok := m[key].([]any); ok {
			for _, sub := range subs {
				out = append(out, secretPaths(sub, prefix, depth+1)...)
			}
		}
	}
	return out
}

// stepMatches reports whether a path element matches a key: "*" matches
// every key, a patternStep a key its regex matches (a regex that does not
// compile matches every key: masking fails closed).
func stepMatches(step, key string) bool {
	if step == "*" || step == key {
		return true
	}
	if p, ok := strings.CutPrefix(step, patternStep); ok {
		re, err := compilePattern(p)
		return err != nil || re.MatchString(key)
	}
	return false
}

// eachAt calls fn for every value of v at path (see stepMatches), with the
// path it was found at.
func eachAt(v any, path, at []string, fn func(at []string, set func(any))) {
	if len(path) == 0 {
		return
	}
	visit := func(key string, child any, set func(any)) {
		next := append(slices.Clone(at), key)
		if len(path) == 1 {
			fn(next, set)
			return
		}
		eachAt(child, path[1:], next, fn)
	}
	switch x := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(x)) {
			if stepMatches(path[0], k) {
				visit(k, x[k], func(n any) { x[k] = n })
			}
		}
	case []any:
		for i := range x {
			if stepMatches(path[0], strconv.Itoa(i)) {
				visit(strconv.Itoa(i), x[i], func(n any) { x[i] = n })
			}
		}
	}
}

// sinkJSONOf builds the response of a sink for a caller with the given scope.
// For every caller, the values the kind's config_schema marks secret are
// masked. Below admin every URL is cut (redactURLs) and, when the kind
// publishes no config_schema or one secretPaths cannot fully follow, every
// string value not under a *_env key is masked too: without a schema the
// server cannot tell a secret from the rest.
func (s *Server) sinkJSONOf(k store.Sink, scope Scope) (sinkJSON, error) {
	cfg := map[string]any{}
	// TODO(#75): decrypt here once sinks are encrypted at rest.
	if err := json.Unmarshal(k.ConfigEncrypted, &cfg); err != nil {
		return sinkJSON{}, fmt.Errorf("sink %s: stored configuration: %w", k.ID, err)
	}
	out := sinkJSON{Name: k.Name, ID: k.ID, ManagedBy: ownerOf(k), Kind: k.Kind, Config: cfg, MaskedFields: []string{}}
	masked := map[string]bool{}
	mask := func(at []string, set func(any)) {
		set(Masked)
		masked[strings.Join(at, ".")] = true
	}
	sch, complete := s.configSchema(k.Kind)
	for _, p := range secretPaths(sch, nil, 0) {
		eachAt(any(cfg), p, nil, mask)
	}
	if scope < ScopeAdmin {
		out.URLRedacted = true
		if !complete {
			maskStrings(cfg, nil, mask)
		}
		for key, v := range cfg {
			cfg[key] = redactURLs(v)
		}
	}
	out.MaskedFields = append([]string{}, slices.Sorted(maps.Keys(masked))...)
	return out, nil
}

// maskStrings masks every string value of v that is not under a key ending
// in _env, which names a variable and is never a secret.
func maskStrings(v any, at []string, mask func([]string, func(any))) {
	switch x := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(x)) {
			if strings.HasSuffix(k, "_env") {
				continue
			}
			next := append(slices.Clone(at), k)
			if _, ok := x[k].(string); ok {
				mask(next, func(n any) { x[k] = n })
				continue
			}
			maskStrings(x[k], next, mask)
		}
	case []any:
		for i := range x {
			next := append(slices.Clone(at), strconv.Itoa(i))
			if _, ok := x[i].(string); ok {
				mask(next, func(n any) { x[i] = n })
				continue
			}
			maskStrings(x[i], next, mask)
		}
	}
}

// redactURLs cuts every URL in a configuration value: an http(s) URL to
// scheme://host, any other scheme to scheme:// alone, because URLs such as
// shoutrrr's carry credentials in the host part.
func redactURLs(v any) any {
	switch x := v.(type) {
	case string:
		u, err := url.Parse(x)
		switch {
		case err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "":
			return u.Scheme + "://" + u.Host
		case strings.HasPrefix(x, "//"): // scheme-relative: the host may be a secret too
			return "[redacted]"
		case err == nil && u.Scheme != "" && strings.Contains(x, "://"):
			return u.Scheme + "://"
		case strings.Contains(x, "://"):
			return "[redacted]"
		}
		return x
	case map[string]any:
		for k, e := range x {
			x[k] = redactURLs(e)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = redactURLs(e)
		}
		return x
	}
	return v
}

// checkSinkConfig applies the API's own rules to a sink configuration: no
// value the kind marks secret, and every *_env key, at any depth, a string
// naming a variable on the allowlist api.allowed_env (names starting with
// ANGELIA_ never).
func (s *Server) checkSinkConfig(kind string, cfg map[string]any) (invalid, env []fieldError) {
	sch, _ := s.configSchema(kind)
	for _, p := range secretPaths(sch, nil, 0) {
		eachAt(any(cfg), p, nil, func(at []string, _ func(any)) {
			invalid = append(invalid, fieldError{Pointer: configPointer(at), Detail: "is a secret value and cannot be set through the API"})
		})
	}
	var walk func(v any, at []string)
	walk = func(v any, at []string) {
		switch x := v.(type) {
		case map[string]any:
			for _, k := range slices.Sorted(maps.Keys(x)) {
				next := append(slices.Clone(at), k)
				if !strings.HasSuffix(k, "_env") {
					walk(x[k], next)
					continue
				}
				name, ok := x[k].(string)
				switch {
				case !ok:
					invalid = append(invalid, fieldError{Pointer: configPointer(next), Detail: "must be a string naming an environment variable"})
				case !s.opts.API.AllowsEnv(name):
					env = append(env, fieldError{Pointer: configPointer(next), Detail: "not allowed by api.allowed_env"})
				}
			}
		case []any:
			for i, e := range x {
				walk(e, append(slices.Clone(at), strconv.Itoa(i)))
			}
		}
	}
	walk(cfg, nil)
	return invalid, env
}

// configPointer is the JSON Pointer of a configuration path in a sink body.
func configPointer(at []string) string {
	p := "/config"
	for _, k := range at {
		p = pointerJoin(p, k)
	}
	return p
}

// GetSink implements StrictServerInterface.
func (s *Server) GetSink(ctx context.Context, req GetSinkRequestObject) (GetSinkResponseObject, error) {
	k, err := s.opts.Store.SinkByName(ctx, req.Name)
	if k, err = readByName(k, err, "sink", req.Name); err != nil {
		return nil, err
	}
	body, err := s.sinkJSONOf(k, principalFrom(ctx).Scope)
	if err != nil {
		return nil, err
	}
	return jsonOK(body), nil
}

// ListSinks implements StrictServerInterface.
func (s *Server) ListSinks(ctx context.Context, req ListSinksRequestObject) (ListSinksResponseObject, error) {
	cur, p := decodeCursor(req.Params.Cursor, cursorSinks)
	if p != nil {
		return nil, p
	}
	var kinds []string
	if req.Params.Kind != nil {
		kinds = *req.Params.Kind
	}
	items, next, err := pageByName(cur, pageLimit(req.Params.Limit),
		func(afterName, afterID string, n int) ([]store.Sink, error) {
			return s.opts.Store.ListNamedSinks(ctx, afterName, afterID, n)
		},
		func(k store.Sink) (string, string) { return k.Name, k.ID },
		func(k store.Sink) (bool, error) { return len(kinds) == 0 || slices.Contains(kinds, k.Kind), nil })
	if err != nil {
		return nil, err
	}
	scope := principalFrom(ctx).Scope
	page := pageJSON[sinkJSON]{Items: make([]sinkJSON, 0, len(items)), NextCursor: next}
	for _, k := range items {
		body, err := s.sinkJSONOf(k, scope)
		if err != nil {
			return nil, err
		}
		page.Items = append(page.Items, body)
	}
	return jsonOK(page), nil
}

// parseSinkBody validates a PUT body by the rules of the watch file and the
// API's own (checkSinkConfig). env holds the *_env names the allowlist
// refuses.
func (s *Server) parseSinkBody(name string, raw []byte) (k watchfile.Sink, invalid, env []fieldError) {
	invalid = bodyNameErrors(raw, name)
	data := truncateDurations(raw, []string{"config", "timeout"})
	k, perrs := watchfile.ParseSink(data, name, s.parseOptions())
	obj := rawObject(raw)
	kind, _ := obj["kind"].(string)
	cfg, _ := obj["config"].(map[string]any)
	secret, env := s.checkSinkConfig(kind, cfg)
	return k, append(append(invalid, parseErrors(perrs)...), secret...), env
}

// PutSink implements StrictServerInterface.
func (s *Server) PutSink(ctx context.Context, req PutSinkRequestObject) (PutSinkResponseObject, error) {
	k, fe, env := s.parseSinkBody(req.Name, stateFrom(ctx).body)
	if len(fe) > 0 {
		return nil, validationProblem("sink", append(fe, env...))
	}
	if len(env) > 0 {
		p := problem(SlugEnvNotAllowed, "config.%s names a variable that api.allowed_env does not allow", strings.TrimPrefix(env[0].Pointer, "/config/"))
		for _, e := range env {
			p.Errors = append(p.Errors, ProblemDetailError(e))
		}
		return nil, p
	}
	dry := req.Params.DryRun != nil && *req.Params.DryRun
	var body sinkJSON
	var change Change
	err := s.inTx(ctx, dry, func(tx store.APITx) error {
		k0, err := tx.SinkByName(ctx, k.Name)
		cur, err := writeByName(k0, err, "sink", k.Name)
		if err != nil {
			return err
		}
		row := store.Sink{Kind: k.Kind, ConfigEncrypted: k.Config, Name: k.Name, ManagedBy: store.ManagedByAPI}
		switch {
		case cur == nil:
			row.ID = s.opts.NewID()
			// TODO(#75): encrypt the configuration at rest; the file apply
			// stores it as plain JSON too.
			if err := tx.CreateSink(ctx, row); err != nil {
				return err
			}
			change = ChangeCreated
		case cur.Kind != k.Kind || !bytes.Equal(cur.ConfigEncrypted, k.Config):
			row.ID = cur.ID
			if err := tx.UpdateSink(ctx, row); err != nil {
				return err
			}
			change = ChangeUpdated
		default:
			row.ID = cur.ID
			change = ChangeUnchanged
		}
		body, err = s.sinkJSONOf(row, principalFrom(ctx).Scope)
		return err
	})
	if err != nil {
		return nil, err
	}
	return putResponse(change, location("sinks", k.Name), body), nil
}

// DeleteSink implements StrictServerInterface.
func (s *Server) DeleteSink(ctx context.Context, req DeleteSinkRequestObject) (DeleteSinkResponseObject, error) {
	dry := req.Params.DryRun != nil && *req.Params.DryRun
	err := s.inTx(ctx, dry, func(tx store.APITx) error {
		k, err := tx.SinkByName(ctx, req.Name)
		cur, err := writeByName(k, err, "sink", req.Name)
		if err != nil {
			return err
		}
		if cur == nil {
			return problem(SlugNotFound, "no sink named %s", req.Name)
		}
		subs, err := tx.SubscriptionsBySink(ctx, cur.ID)
		if err != nil {
			return err
		}
		if len(subs) > 0 {
			return problem(SlugSinkInUse, "sink %s is used by subscription %s", req.Name, subs[0].Name)
		}
		return tx.DeleteSink(ctx, cur.ID)
	})
	if err != nil {
		return nil, err
	}
	return deleteResponse(), nil
}
