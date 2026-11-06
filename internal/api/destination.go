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
		destinationPointers(sch, "", &found, 0)
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

// destinationPointers collects the pointers of the object properties, at
// any depth, whose schema has x-angelia-destination: true.
func destinationPointers(sch map[string]any, ptr string, found *[]string, depth int) {
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
		if d, _ := ps["x-angelia-destination"].(bool); d {
			*found = append(*found, p)
		}
		destinationPointers(ps, p, found, depth+1)
	}
}
