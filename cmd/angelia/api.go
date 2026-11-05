package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/angelia-dev/angelia/internal/api"
	"github.com/angelia-dev/angelia/internal/buildinfo"
	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

// newAPIServer builds the REST API of the api role on the SQLite store named
// by the instance configuration. The returned function closes the store.
//
// Every request gets 401 for now: the authenticator is api.DenyAll until API
// tokens (#72, human-led) exist, so no token is accepted and nothing is
// exposed.
func newAPIServer(ctx context.Context, cfg *config.Config, keys *secrets.Keyring, log *slog.Logger) (*api.Server, func(), error) {
	sources, err := watchSources(cfg)
	if err != nil {
		return nil, nil, err
	}
	dbPath, err := cfg.Database.SQLitePath()
	if err != nil {
		return nil, nil, fmt.Errorf("api role: only sqlite databases are supported in this phase: %w", err)
	}
	st, err := sqlite.Open(ctx, dbPath, log)
	if err != nil {
		return nil, nil, fmt.Errorf("open store: %w", err)
	}
	srv, err := api.New(api.Options{
		Store: st, Auth: api.DenyAll{}, Sources: sources, Plugins: builtinPlugins(sources.Kinds()),
		API: cfg.API, Secrets: keys, Logger: log,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("api role: %w", errorsJoin(err, st.Close()))
	}
	if err := srv.CheckStoredSinks(ctx); err != nil {
		return nil, nil, errorsJoin(err, st.Close())
	}
	return srv, func() { _ = st.Close() }, nil
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
