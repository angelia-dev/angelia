package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/angelia-dev/angelia/internal/api"
	"github.com/angelia-dev/angelia/internal/apitoken"
	"github.com/angelia-dev/angelia/internal/buildinfo"
	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/mcp"
	"github.com/angelia-dev/angelia/internal/metrics"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/watchfile"
	"github.com/angelia-dev/angelia/internal/webui"
)

// newAPIServer builds the REST API of the api role on the store st, which
// the caller opened and checked (openStore) and closes, with MCP over
// Streamable HTTP at /mcp (ADR 0029).
//
// Requests authenticate with the stored API tokens (ADR 0030): the api role
// runs the authenticator's last-use flush for its lifetime, with a final
// flush once it drains.
func newAPIServer(cfg *config.Config, st store.APIStore, sources *source.Registry, keys *secrets.Keyring, m *metrics.Metrics, drain time.Duration, log *slog.Logger) (*api.Server, error) {
	srv, err := api.New(apiOptions(cfg, st, sources, keys, m, drain, log))
	if err != nil {
		return nil, fmt.Errorf("api role: %w", err)
	}
	return srv, nil
}

// apiOptions is the api role's configuration; drain is serve's
// --drain-timeout (see apiDrainWait).
func apiOptions(cfg *config.Config, st store.APIStore, sources *source.Registry, keys *secrets.Keyring, m *metrics.Metrics, drain time.Duration, log *slog.Logger) api.Options {
	auth := apitoken.NewAuthenticator(st, log)
	opts := api.Options{
		Store: st, Auth: auth, Sources: sources, Plugins: builtinPlugins(sources.Kinds()),
		API: cfg.API, DrainWait: apiDrainWait(drain), AllowHTTPHosts: cfg.HTTP.AllowHTTPHosts, Secrets: keys, Logger: log,
		AuthLoop: func(ctx context.Context) error {
			return auth.Run(ctx, tokenFlushTick, tokenFlushTimeout) //nolint:wrapcheck // the api role wraps it
		},
	}
	opts.MCP = func(rest http.RoundTripper) (http.Handler, error) {
		return mcp.NewHTTPHandler(mcp.HTTPOptions{REST: rest, Version: buildinfo.Version(), Logger: log}) //nolint:wrapcheck // api.New wraps it
	}
	if cfg.UI.Enabled {
		opts.UI = webui.Handler()
	}
	if m != nil { // a nil *Metrics in the interface field would panic
		opts.Metrics = m
	}
	return opts
}

// apiDrainWait is how long the api role waits for in-flight requests once it
// drains: serve's --drain-timeout less the final token flush's own bound
// (tokenFlushTimeout), so that a stuck request cannot use up the budget that
// the flush needs. A timeout of at most twice that bound is used whole; zero
// stays zero, which api.Options reads as api.DefaultDrainWait (10 s).
func apiDrainWait(drain time.Duration) time.Duration {
	if drain > 2*tokenFlushTimeout {
		return drain - tokenFlushTimeout
	}
	return drain // 0 stays 0: api.DefaultDrainWait
}

// The api role's last-use flush (ADR 0030 T6): a tick well below
// apitoken.TouchInterval, which bounds the writes per token, and a short
// bound on the final flush, which comes out of the drain budget.
const (
	tokenFlushTick    = time.Minute
	tokenFlushTimeout = 5 * time.Second
)

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
