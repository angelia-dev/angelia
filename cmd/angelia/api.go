package main

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/angelia-dev/angelia/internal/api"
	"github.com/angelia-dev/angelia/internal/buildinfo"
	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/mcp"
	"github.com/angelia-dev/angelia/internal/metrics"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

// newAPIServer builds the REST API of the api role on the store st, which
// the caller opened and checked (openStore) and closes, with MCP over
// Streamable HTTP at /mcp (ADR 0029).
//
// Every request gets 401 for now: the authenticator is api.DenyAll until API
// tokens (#72, human-led) exist, so no token is accepted and nothing is
// exposed.
func newAPIServer(cfg *config.Config, st *sqlite.Store, sources *source.Registry, keys *secrets.Keyring, m *metrics.Metrics, log *slog.Logger) (*api.Server, error) {
	opts := api.Options{
		Store: st, Auth: api.DenyAll{}, Sources: sources, Plugins: builtinPlugins(sources.Kinds()),
		API: cfg.API, AllowHTTPHosts: cfg.HTTP.AllowHTTPHosts, Secrets: keys, Logger: log,
	}
	opts.MCP = func(rest http.RoundTripper) (http.Handler, error) {
		return mcp.NewHTTPHandler(mcp.HTTPOptions{REST: rest, Version: buildinfo.Version(), Logger: log}) //nolint:wrapcheck // api.New wraps it
	}
	if m != nil { // a nil *Metrics in the interface field would panic
		opts.Metrics = m
	}
	srv, err := api.New(opts)
	if err != nil {
		return nil, fmt.Errorf("api role: %w", err)
	}
	return srv, nil
}

// builtinPlugins is the plugin catalogue of the built-in kinds. WASM plugins
// join it once serve loads them.
func builtinPlugins(sourceKinds []string) []api.PluginInfo {
	var out []api.PluginInfo
	for _, k := range sourceKinds {
		out = append(out, api.PluginInfo{Kind: k, Interface: "source", Runtime: "builtin", Version: buildinfo.Version()})
	}
	for _, k := range watchfile.SinkKinds {
		out = append(out, api.PluginInfo{Kind: k, Interface: "sink", Runtime: "builtin", Version: buildinfo.Version()})
	}
	return out
}

func errorsJoin(a, b error) error {
	if b == nil {
		return a
	}
	return fmt.Errorf("%w (closing the store: %w)", a, b)
}
