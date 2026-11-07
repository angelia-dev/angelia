package api

import (
	"fmt"
	"slices"
	"strings"
)

// SinkDestinations maps every sink kind in plugins to the JSON Pointer of
// the configuration key its config_schema marks x-angelia-destination: true
// (ADR 0024 §2), for dispatch.Options.Destinations. A built-in kind without
// a schema of its own uses the one the specification documents. A kind
// that marks no key is left out; one that marks several is an error.
func SinkDestinations(plugins []PluginInfo) (map[string]string, error) {
	spec, err := loadSpec()
	if err != nil {
		return nil, err
	}
	s := &Server{spec: spec, opts: Options{Plugins: plugins}}
	out := map[string]string{}
	for _, p := range plugins {
		if p.Interface != "sink" {
			continue
		}
		sch, _ := s.configSchema(p.Kind)
		var found []string
		annotatedPointers(sch, "x-angelia-destination", "", &found, 0)
		switch len(found) {
		case 0:
		case 1:
			out[p.Kind] = found[0]
		default:
			return nil, fmt.Errorf("api: sink kind %s marks more than one key x-angelia-destination: %s", p.Kind, strings.Join(found, ", "))
		}
	}
	return out, nil
}

// SinkSecretDestinations maps every sink kind in plugins to the JSON
// Pointers of the reference keys its config_schema marks
// x-angelia-secret-destination: true (ADR 0026 §1), for
// dispatch.Options.SecretDestinations: the resolved value of such a key is
// the destination itself. A built-in kind without a schema of its own uses
// the one the specification documents (the shoutrrr kind's url_ref and
// url_env). A kind that marks no key is left out. Only *_ref and *_env keys
// can carry the annotation, since only they are resolved; another key is an
// error, as is a kind that also marks x-angelia-destination.
func SinkSecretDestinations(plugins []PluginInfo) (map[string][]string, error) {
	spec, err := loadSpec()
	if err != nil {
		return nil, err
	}
	s := &Server{spec: spec, opts: Options{Plugins: plugins}}
	out := map[string][]string{}
	for _, p := range plugins {
		if p.Interface != "sink" {
			continue
		}
		sch, _ := s.configSchema(p.Kind)
		var found, plain []string
		annotatedPointers(sch, "x-angelia-secret-destination", "", &found, 0)
		if len(found) == 0 {
			continue
		}
		for _, ptr := range found {
			if !strings.HasSuffix(ptr, "_ref") && !strings.HasSuffix(ptr, "_env") {
				return nil, fmt.Errorf("api: sink kind %s marks %s x-angelia-secret-destination, but only *_ref and *_env keys are resolved", p.Kind, ptr)
			}
		}
		if annotatedPointers(sch, "x-angelia-destination", "", &plain, 0); len(plain) > 0 {
			return nil, fmt.Errorf("api: sink kind %s marks both x-angelia-destination and x-angelia-secret-destination", p.Kind)
		}
		out[p.Kind] = found
	}
	return out, nil
}

// annotatedPointers collects the pointers of the object properties, at any
// depth, whose schema has the annotation set to true.
func annotatedPointers(sch map[string]any, annotation, ptr string, found *[]string, depth int) {
	props, _ := sch["properties"].(map[string]any)
	if depth > 32 || props == nil {
		return
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		ps, _ := props[k].(map[string]any)
		p := ptr + "/" + strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1")
		if d, _ := ps[annotation].(bool); d {
			*found = append(*found, p)
		}
		annotatedPointers(ps, annotation, p, found, depth+1)
	}
}
