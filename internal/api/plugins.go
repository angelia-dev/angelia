package api

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/angelia-dev/angelia/internal/supervisor"
)

// PluginInfo is one entry of the plugin catalogue: a source or sink kind the
// instance can use (spec docs/api.md decision 29).
type PluginInfo struct {
	Kind      string
	Interface string // "source" or "sink"
	Runtime   string // "builtin", "wasm", ...
	// Version is the plugin's version, or the Angelia version for a
	// built-in; "" is unknown.
	Version string
	// ABI is the host ABI of a third-party plugin; "" for built-ins.
	ABI string
	// ConfigSchema is the JSON Schema of the kind's configuration, or nil.
	ConfigSchema json.RawMessage
	// AllowedHosts are the hosts a third-party plugin may reach.
	AllowedHosts []PluginHostInfo
}

// PluginHostInfo is one allowed host of a plugin.
type PluginHostInfo struct {
	Host            string
	Origin          string // "manifest" or "operator"
	CredentialPorts []int
}

// kindConfig returns the configuration schema the specification documents
// for a built-in sink kind (SinkConfig's x-angelia-kind-config), with its
// references inlined, or nil.
func (d *specDoc) kindConfig(kind string) map[string]any {
	kc, _ := d.lookup("#/components/schemas/SinkConfig/x-angelia-kind-config").(map[string]any)
	sch, ok := kc[kind]
	if !ok {
		return nil
	}
	m, _ := d.inline(sch, 0).(map[string]any)
	return m
}

// inline replaces every local $ref in v by a copy of what it points to.
func (d *specDoc) inline(v any, depth int) any {
	if depth > 32 {
		return nil
	}
	switch x := v.(type) {
	case map[string]any:
		if ref, ok := x["$ref"].(string); ok && strings.HasPrefix(ref, "#/") {
			target, _ := d.lookup(ref).(map[string]any)
			merged := map[string]any{}
			for k, e := range target {
				merged[k] = e
			}
			for k, e := range x { // siblings of $ref (description, default) win
				if k != "$ref" {
					merged[k] = e
				}
			}
			return d.inline(merged, depth+1)
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = d.inline(e, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = d.inline(e, depth+1)
		}
		return out
	}
	return v
}

func (s *Server) plugin(iface, kind string) (PluginInfo, bool) {
	for _, p := range s.opts.Plugins {
		if p.Interface == iface && p.Kind == kind {
			return p, true
		}
	}
	return PluginInfo{}, false
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func pluginJSONOf(p PluginInfo) pluginJSON {
	out := pluginJSON{
		Kind: p.Kind, Interface: p.Interface, Runtime: p.Runtime, Version: optString(p.Version), ABI: optString(p.ABI),
		ConfigSchema: p.ConfigSchema, AllowedHosts: []pluginHostJSON{},
	}
	for _, h := range p.AllowedHosts {
		ports := h.CredentialPorts
		if ports == nil {
			ports = []int{}
		}
		out.AllowedHosts = append(out.AllowedHosts, pluginHostJSON{Host: h.Host, Origin: h.Origin, CredentialPorts: ports})
	}
	return out
}

// sortedPlugins returns the catalogue in ascending kind order, then interface.
func (s *Server) sortedPlugins() []PluginInfo {
	ps := slices.Clone(s.opts.Plugins)
	slices.SortFunc(ps, func(a, b PluginInfo) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Interface, b.Interface))
	})
	return ps
}

// ListPlugins implements StrictServerInterface.
func (s *Server) ListPlugins(_ context.Context, req ListPluginsRequestObject) (ListPluginsResponseObject, error) {
	cur, p := decodeCursor(req.Params.Cursor, cursorPlugins)
	if p != nil {
		return nil, p
	}
	limit := pageLimit(req.Params.Limit)
	page := pageJSON[pluginJSON]{Items: []pluginJSON{}}
	for _, pl := range s.sortedPlugins() {
		if cur.N != "" && cmp.Or(cmp.Compare(pl.Kind, cur.N), cmp.Compare(pl.Interface, cur.I)) <= 0 {
			continue
		}
		if req.Params.Interface != nil && pl.Interface != string(*req.Params.Interface) {
			continue
		}
		if len(page.Items) == limit {
			last := page.Items[limit-1]
			page.NextCursor = encodeCursor(pageCursor{K: cursorPlugins, N: last.Kind, I: last.Interface})
			break
		}
		page.Items = append(page.Items, pluginJSONOf(pl))
	}
	return jsonOK(page), nil
}

// GetPlugin implements StrictServerInterface.
func (s *Server) GetPlugin(_ context.Context, req GetPluginRequestObject) (GetPluginResponseObject, error) {
	pl, ok := s.plugin(string(req.Interface), req.Kind)
	if !ok {
		return nil, problem(SlugNotFound, "no %s kind %s", req.Interface, req.Kind)
	}
	return jsonOK(pluginJSONOf(pl)), nil
}

// Role returns the api role of `angelia serve`: it mounts the API on the
// shared listener under /v1/ (and MCP at /mcp when Options.MCP is set) and, once shutdown starts, drains it (503 with
// Retry-After for new requests, open event streams closed).
func (s *Server) Role() supervisor.Role { return apiRole{s: s} }

type apiRole struct{ s *Server }

func (apiRole) Name() string { return "api" }

func (r apiRole) Run(ctx context.Context) error {
	<-ctx.Done()
	r.s.Drain()
	return nil
}

// Mount implements supervisor.HTTPRole.
func (r apiRole) Mount(mux *http.ServeMux) {
	mux.Handle("/v1/", r.s)
	if r.s.mcp != nil {
		mux.Handle(MCPPath, r.s)
	}
}
