package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/angelia-dev/angelia/internal/api"
	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/detect"
	"github.com/angelia-dev/angelia/internal/dispatch"
	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/metrics"
	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/scheduler"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/sink"
	"github.com/angelia-dev/angelia/internal/sink/githubdispatch"
	"github.com/angelia-dev/angelia/internal/sink/webhook"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/crates"
	"github.com/angelia-dev/angelia/internal/source/githubrelease"
	"github.com/angelia-dev/angelia/internal/source/githubtag"
	"github.com/angelia-dev/angelia/internal/source/gitlabrelease"
	"github.com/angelia-dev/angelia/internal/source/npm"
	"github.com/angelia-dev/angelia/internal/source/pypi"
	"github.com/angelia-dev/angelia/internal/source/registryauth"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
)

// The composition root of the detect and deliver roles: one netguard client
// built from the instance configuration, shared by the sources (scheduler)
// and the sinks (dispatcher), so http.allow_networks applies to both.

// builtinSources registers the built-in source kinds with hc. A non-nil wrap
// decorates each source (metrics). log may be nil. The kinds are the ones the watch file
// and the API accept (watchSources), so every stored target can be checked.
// res resolves the stored secrets of sources.<kind>.auth at fetch time (nil:
// such a source fails each fetch); the watch file check passes nil.
func builtinSources(cfg *config.Config, hc *http.Client, log *slog.Logger, wrap func(source.Source) source.Source, res registryauth.Resolver) (*source.Registry, error) {
	var ghOpts []githubrelease.Option
	if tok := cfg.SourceToken(githubrelease.Kind); tok != "" {
		ghOpts = append(ghOpts, githubrelease.WithToken(tok))
	}
	var tagOpts []githubtag.Option
	if tok := cfg.SourceToken(githubtag.Kind); tok != "" {
		tagOpts = append(tagOpts, githubtag.WithToken(tok))
	}
	// token_env sends GitLab's own header, PRIVATE-TOKEN (a personal, project
	// or group access token); auth sends "Authorization: Bearer", which GitLab
	// accepts for the same tokens and for OAuth tokens.
	var glOpts []gitlabrelease.Option
	if tok := cfg.SourceToken(gitlabrelease.Kind); tok != "" {
		glOpts = append(glOpts, gitlabrelease.WithToken(tok))
	}
	var pypiOpts []pypi.Option
	if log != nil {
		pypiOpts = append(pypiOpts, pypi.WithLogger(log))
	}
	pypiURL := baseURL(cfg, pypi.Kind, pypi.DefaultBaseURL)
	npmURL := baseURL(cfg, npm.Kind, "")
	ghURL := baseURL(cfg, githubrelease.Kind, githubrelease.DefaultBaseURL)
	tagURL := baseURL(cfg, githubtag.Kind, githubtag.DefaultBaseURL)
	glURL := baseURL(cfg, gitlabrelease.Kind, gitlabrelease.DefaultBaseURL)
	cratesURL := baseURL(cfg, crates.Kind, crates.DefaultIndexURL)
	reg := source.NewRegistry()
	for _, s := range []source.Source{
		pypi.New(pypiURL, sourceClient(cfg, pypi.Kind, pypiURL, hc, res), pypiOpts...),
		npm.New(npmURL, sourceClient(cfg, npm.Kind, npmURL, hc, res)),
		githubrelease.New(ghURL, sourceClient(cfg, githubrelease.Kind, ghURL, hc, res), ghOpts...),
		githubtag.New(tagURL, sourceClient(cfg, githubtag.Kind, tagURL, hc, res), tagOpts...),
		gitlabrelease.New(glURL, sourceClient(cfg, gitlabrelease.Kind, glURL, hc, res), glOpts...),
		// crates sends no credential: its index and API are public, and a
		// private cargo registry authenticates with a raw token that no
		// source option defines yet.
		crates.New(cratesURL, hc),
	} {
		if wrap != nil {
			s = wrap(s)
		}
		if err := reg.Register(s); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

// sourceClient returns the HTTP client of source kind: hc, or hc adding the
// credential of sources.<kind>.auth (stored secrets, bound to the base URL's
// host) or, for pypi and npm, of token_env (github-release, github-tag and
// gitlab-release read their token themselves, each with its own header).
// base is the URL the source reads from.
func sourceClient(cfg *config.Config, kind, base string, hc *http.Client, res registryauth.Resolver) *http.Client {
	a := cfg.Sources[kind].Auth
	c := registryauth.Config{Resolver: res}
	switch {
	case a.TokenRef != "":
		c.TokenRef = a.TokenRef
	case a.PasswordRef != "":
		c.PasswordRef, c.Username = a.PasswordRef, a.Username
	case kind == pypi.Kind || kind == npm.Kind:
		c.Token = secrets.NewValueString(cfg.SourceToken(kind))
	}
	if base == "" {
		base = npm.DefaultBaseURL
	}
	c.BaseURL, _ = url.Parse(base) // validated by config.Validate
	c.AllowHTTPHosts = cfg.HTTP.AllowHTTPHosts
	c.Proxy = outgoingProxy // the transport's own function: see newNetClient
	return registryauth.Wrap(hc, c)
}

// outgoingProxy chooses the proxy of every outgoing request, from HTTPS_PROXY,
// HTTP_PROXY and NO_PROXY (read once). It is passed both to the netguard
// transport (newNetClient) and to registryauth, whose rule "no credential
// over plain http through a proxy" is only sound when both see the same
// route; do not give either another function.
func outgoingProxy(r *http.Request) (*url.URL, error) { return http.ProxyFromEnvironment(r) }

// sourceRefs lists the stored secrets the sources of cfg name, with the
// registry URL each is sent to.
func sourceRefs(cfg *config.Config) []registryauth.Ref {
	kinds := make([]string, 0, len(cfg.Sources))
	for k := range cfg.Sources {
		kinds = append(kinds, k)
	}
	slices.Sort(kinds)
	var refs []registryauth.Ref
	for _, k := range kinds {
		a := cfg.Sources[k].Auth
		if a.IsZero() || !config.SendsCredential(k) {
			continue
		}
		u, err := url.Parse(baseURL(cfg, k, publicRegistries[k]))
		if err != nil {
			continue // config.Validate has reported it
		}
		for _, r := range []struct{ key, name string }{{"token_ref", a.TokenRef}, {"password_ref", a.PasswordRef}} {
			if r.name != "" {
				refs = append(refs, registryauth.Ref{Key: "sources." + k + ".auth." + r.key, Name: r.name, URL: u})
			}
		}
	}
	return refs
}

// checkSourceSecrets fails when a secret named by sources.<kind>.auth does
// not exist or is not bound to its registry's host, from metadata only
// (like api.CheckStoredSinks, so a typo stops the start instead of failing
// every fetch).
func checkSourceSecrets(cfg *config.Config, hosts map[string][]string) error {
	return errors.Join(registryauth.CheckRefs(sourceRefs(cfg), hosts)...)
}

// publicRegistries are the base URLs that leave an event's purl without a
// repository_url qualifier.
var publicRegistries = map[string]string{
	pypi.Kind:          pypi.DefaultBaseURL,
	npm.Kind:           npm.DefaultBaseURL,
	githubrelease.Kind: githubrelease.DefaultBaseURL,
	githubtag.Kind:     githubtag.DefaultBaseURL,
	gitlabrelease.Kind: gitlabrelease.DefaultBaseURL,
	crates.Kind:        crates.DefaultIndexURL,
}

// registryURL returns event.CheckInput.RegistryURL for a kind: its
// configured base_url when that is not the public registry, else "".
func registryURL(cfg *config.Config) func(kind string) string {
	return func(kind string) string {
		b := strings.TrimRight(cfg.Sources[kind].BaseURL, "/")
		if b == "" || strings.EqualFold(b, strings.TrimRight(publicRegistries[kind], "/")) {
			return ""
		}
		return b
	}
}

// builtinSinks returns the built-in sinks, sending through hc. A new
// built-in kind is added here and to watchfile.SinkKinds (a test checks
// that both list the same kinds).
func builtinSinks(cfg *config.Config, hc *http.Client, log *slog.Logger) []sink.Sink {
	return []sink.Sink{
		// http.allow_http_hosts is checked at every delivery (ADR 0023,
		// decided question 4 and D12).
		webhook.New(hc, webhook.Options{AllowHTTPHosts: cfg.HTTP.AllowHTTPHosts, Logger: log}),
		githubdispatch.New(hc, githubdispatch.Options{}),
	}
}

// warnDrainBudget warns when the drain timeout (after the trace flush is
// taken out of it) does not exceed one delivery attempt plus its final
// store write: an attempt in progress at shutdown may then go unrecorded,
// and it is sent again once its lease expires (a likely duplicate).
func warnDrainBudget(log *slog.Logger, drain time.Duration) {
	need := dispatch.DefaultDeliverTimeout + dispatch.WriteTimeout
	if drain <= need {
		log.Warn("drain timeout too short for the dispatcher: an attempt in progress at shutdown may not be recorded and may be sent again",
			"drain_timeout", drain.String(), "needed_above", need.String())
	}
}

// newNetClient builds the one outgoing HTTP client of the process, with the
// SSRF guard's allowlist from http.allow_networks. obs may be nil.
func newNetClient(cfg *config.Config, log *slog.Logger, obs netguard.Observer) (*netguard.Client, error) {
	roots, err := cfg.HTTP.LoadCAPool()
	if err != nil {
		return nil, fmt.Errorf("http client: %w", err)
	}
	c, err := netguard.New(netguard.Config{
		Allow: cfg.HTTP.AllowPrefixes(), UserAgent: cfg.HTTP.UserAgent, Logger: log, Observer: obs, RootCAs: roots,
		Proxy: outgoingProxy,
	})
	if err != nil {
		return nil, fmt.Errorf("http client: %w", err)
	}
	return c, nil
}

// pipelineDeps is what the scheduler and dispatcher roles are built from.
type pipelineDeps struct {
	cfg     *config.Config
	store   *sqlite.Store
	keys    *secrets.Keyring
	plugins []api.PluginInfo
	tracer  trace.TracerProvider
	metrics *metrics.Metrics // nil: --metrics=false
	log     *slog.Logger
}

// pipelineRoles holds the scheduler and dispatcher roles of a process; a
// role not selected is nil.
type pipelineRoles struct {
	scheduler  *scheduler.Scheduler
	dispatcher *dispatch.Dispatcher
}

// newPipelineRoles builds the selected scheduler and dispatcher roles on
// one netguard client.
func newPipelineRoles(d pipelineDeps, withScheduler, withDispatcher bool) (pipelineRoles, error) {
	var out pipelineRoles
	if !withScheduler && !withDispatcher {
		return out, nil
	}
	// Each hook is an interface: a nil *metrics.Metrics must stay a nil
	// interface, never a typed nil (netguard.Observer says why).
	var (
		obs     netguard.Observer
		schedM  scheduler.Metrics
		dispM   dispatch.Metrics
		wrapSrc func(source.Source) source.Source
	)
	if d.metrics != nil {
		obs, schedM, dispM = d.metrics, d.metrics, d.metrics
		wrapSrc = d.metrics.Source
	}
	client, err := newNetClient(d.cfg, d.log, obs)
	if err != nil {
		return out, err
	}
	hc := client.HTTPClient()
	if withScheduler {
		sources, err := builtinSources(d.cfg, hc, d.log, wrapSrc, &secrets.Resolver{Store: d.store, Keys: d.keys})
		if err != nil {
			return out, err
		}
		checker := &detect.Checker{
			Store: d.store, Sources: sources, Instance: d.cfg.Instance.Name,
			RegistryURL: registryURL(d.cfg), Matcher: event.NewMatcherCache(), Logger: d.log,
		}
		out.scheduler, err = scheduler.New(d.store, checker, scheduler.Options{
			Logger: d.log, Metrics: schedM, TracerProvider: d.tracer,
		})
		if err != nil {
			return out, err
		}
	}
	if withDispatcher {
		reg := sink.NewRegistry()
		for _, s := range builtinSinks(d.cfg, hc, d.log) {
			if err := reg.Register(s); err != nil {
				return out, err
			}
		}
		// Without the destinations every bound secret is refused, and
		// without the resolver every *_ref key fails (ADR 0024 §6).
		dest, err := api.SinkDestinations(d.plugins)
		if err != nil {
			return out, err
		}
		out.dispatcher, err = dispatch.New(d.store, reg, dispatch.Options{
			Logger: d.log, Metrics: dispM, TracerProvider: d.tracer,
			Secrets: &secrets.Resolver{Store: d.store, Keys: d.keys}, Destinations: dest,
		})
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// openStore opens the SQLite store named by the instance configuration and
// runs the stored-sinks check of ADR 0024 §6 before any role starts: every
// role that opens the store refuses one whose sinks hold literal secrets
// (ADR 0024 amendment).
func openStore(ctx context.Context, cfg *config.Config, plugins []api.PluginInfo, log *slog.Logger) (*sqlite.Store, error) {
	dbPath, err := cfg.Database.SQLitePath()
	if err != nil {
		return nil, fmt.Errorf("only sqlite databases are supported in this phase: %w", err)
	}
	st, err := sqlite.Open(ctx, dbPath, log)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	if err := api.CheckStoredSinks(ctx, st, plugins); err != nil {
		return nil, errorsJoin(err, st.Close())
	}
	if len(sourceRefs(cfg)) > 0 {
		rows, err := st.ListSecrets(ctx)
		if err != nil {
			return nil, errorsJoin(fmt.Errorf("list secrets: %w", err), st.Close())
		}
		hosts := make(map[string][]string, len(rows))
		for _, r := range rows {
			hosts[r.Name] = r.Hosts
		}
		if err := checkSourceSecrets(cfg, hosts); err != nil {
			return nil, errorsJoin(fmt.Errorf("source credentials: %w", err), st.Close())
		}
	}
	return st, nil
}
