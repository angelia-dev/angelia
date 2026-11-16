package main

import (
	"context"
	"errors"
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
	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/watchfile"
	"github.com/angelia-dev/angelia/internal/webauth"
	"github.com/angelia-dev/angelia/internal/webui"
)

// newAPIServer builds the REST API of the api role on the store st, which
// the caller opened and checked (openStore) and closes, with MCP over
// Streamable HTTP at /mcp (ADR 0029).
//
// Requests authenticate with the stored API tokens (ADR 0030): the api role
// runs the authenticator's last-use flush for its lifetime, with a final
// flush once it drains.
//
// With ui.enabled, browsers also authenticate with an admin UI session
// (ADR 0033): the api role serves the sign-in routes under /ui/auth/, runs
// the sessions' last-seen flush next to the tokens', and at start warns of
// active tokens whose creator ui.access no longer lists.
func newAPIServer(cfg *config.Config, st store.APIStore, sources *source.Registry, keys *secrets.Keyring, m *metrics.Metrics, drain time.Duration, log *slog.Logger) (*api.Server, error) {
	opts, err := apiOptions(cfg, st, sources, keys, m, drain, log)
	if err != nil {
		return nil, fmt.Errorf("api role: %w", err)
	}
	srv, err := api.New(opts)
	if err != nil {
		return nil, fmt.Errorf("api role: %w", err)
	}
	return srv, nil
}

// apiOptions is the api role's configuration; drain is serve's
// --drain-timeout (see apiDrainWait).
func apiOptions(cfg *config.Config, st store.APIStore, sources *source.Registry, keys *secrets.Keyring, m *metrics.Metrics, drain time.Duration, log *slog.Logger) (api.Options, error) {
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
	// GET /v1/access lists ui.access whether or not the UI is enabled.
	opts.UIAccess = cfg.UI.Access
	if m != nil { // a nil *Metrics in the interface field would panic
		opts.Metrics = m
	}
	if cfg.UI.Enabled {
		sessions, handler, err := uiAuth(cfg, st, m, log)
		if err != nil {
			return api.Options{}, err
		}
		opts.Sessions, opts.UIAuth = sessions, handler
		opts.SessionCookie, opts.PublicOrigin = webauth.SessionCookieName(cfg.UI), cfg.UI.PublicOrigin()
		opts.AuthLoop = func(ctx context.Context) error {
			// Tokens outlive their creator's access: say which ones, once.
			sessions.WarnOrphanTokens(ctx, st)
			return runBoth(ctx,
				func(ctx context.Context) error { return auth.Run(ctx, tokenFlushTick, tokenFlushTimeout) },        //nolint:wrapcheck // the api role wraps it
				func(ctx context.Context) error { return sessions.Run(ctx, webauth.FlushTick, tokenFlushTimeout) }, //nolint:wrapcheck // the api role wraps it
			)
		}
	}
	return opts, nil
}

// uiAuth builds the admin UI's sessions and sign-in handler (ADR 0033 §4):
// the GitHub OAuth App and the OIDC providers (#268), whose requests go
// through the netguard client.
func uiAuth(cfg *config.Config, st store.APIStore, m *metrics.Metrics, log *slog.Logger) (*webauth.Sessions, *webauth.Handler, error) {
	var obs netguard.Observer
	if m != nil { // a typed nil would panic in netguard
		obs = m
	}
	client, err := newNetClient(cfg, log, obs)
	if err != nil {
		return nil, nil, err
	}
	var providers []webauth.IdentityProvider
	if cfg.UI.GitHub.Configured() {
		secret, warning, err := cfg.UI.GitHub.ClientSecret()
		if err != nil {
			return nil, nil, fmt.Errorf("ui.github: %w", err)
		}
		if warning != "" {
			log.Warn(warning)
		}
		gh, err := webauth.NewGitHub(webauth.GitHubConfig{
			ClientID: cfg.UI.GitHub.ClientID, ClientSecret: secret,
			RedirectURL: webauth.RedirectURL(cfg.UI, webauth.GitHubName),
			WebURL:      cfg.UI.GitHub.WebURL, APIURL: cfg.UI.GitHub.APIURL, HTTP: client.HTTPClient(),
		})
		if err != nil {
			return nil, nil, fmt.Errorf("ui: %w", err)
		}
		providers = append(providers, gh)
	}
	for _, o := range cfg.UI.OIDC {
		secret, warning, err := o.ClientSecret()
		if err != nil {
			return nil, nil, fmt.Errorf("ui.oidc: %w", err)
		}
		if warning != "" {
			log.Warn(warning)
		}
		p, err := webauth.NewOIDC(webauth.OIDCConfig{
			Name: o.Name, Issuer: o.Issuer, ClientID: o.ClientID, ClientSecret: secret,
			RedirectURL: webauth.RedirectURL(cfg.UI, o.Name), Scopes: o.EffectiveScopes(), Algs: o.EffectiveAlgs(),
			HTTP: client.HTTPClient(),
		})
		if err != nil {
			return nil, nil, fmt.Errorf("ui: %w", err)
		}
		providers = append(providers, p)
	}
	sessions := webauth.NewSessions(st, cfg.UI, log)
	hopts := webauth.HandlerOptions{Providers: providers, Sessions: sessions, UI: cfg.UI, Logger: log}
	if m != nil {
		m.UIProviders(webauth.ProviderNames(cfg.UI)...)
		hopts.Metrics = m
	}
	h, err := webauth.NewHandler(hopts)
	if err != nil {
		return nil, nil, fmt.Errorf("ui: %w", err)
	}
	return sessions, h, nil
}

// runBoth runs a and b until both return, and returns the first error.
func runBoth(ctx context.Context, a, b func(context.Context) error) error {
	errs := make(chan error, 2)
	go func() { errs <- a(ctx) }()
	go func() { errs <- b(ctx) }()
	return errors.Join(<-errs, <-errs)
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
