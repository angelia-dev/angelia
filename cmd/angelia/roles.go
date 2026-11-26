package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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
	execsink "github.com/angelia-dev/angelia/internal/sink/exec"
	"github.com/angelia-dev/angelia/internal/sink/githubdispatch"
	"github.com/angelia-dev/angelia/internal/sink/gitlabtrigger"
	"github.com/angelia-dev/angelia/internal/sink/mqtt"
	"github.com/angelia-dev/angelia/internal/sink/shoutrrr"
	"github.com/angelia-dev/angelia/internal/sink/webhook"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/crates"
	"github.com/angelia-dev/angelia/internal/source/githubrelease"
	"github.com/angelia-dev/angelia/internal/source/githubtag"
	"github.com/angelia-dev/angelia/internal/source/gitlabrelease"
	"github.com/angelia-dev/angelia/internal/source/goproxy"
	"github.com/angelia-dev/angelia/internal/source/httpjson"
	"github.com/angelia-dev/angelia/internal/source/httpregex"
	"github.com/angelia-dev/angelia/internal/source/maven"
	"github.com/angelia-dev/angelia/internal/source/npm"
	"github.com/angelia-dev/angelia/internal/source/oci"
	"github.com/angelia-dev/angelia/internal/source/pypi"
	"github.com/angelia-dev/angelia/internal/source/registryauth"
	"github.com/angelia-dev/angelia/internal/store/open"
)

// The composition root of the detect and deliver roles: one netguard client
// built from the instance configuration, shared by the sources (scheduler)
// and the sinks (dispatcher), so http.allow_networks applies to both.

// builtinKinds are the source kinds this binary implements. A named
// instance (sources.<name>.kind) may select any of them except the
// urlKinds.
var builtinKinds = []string{
	pypi.Kind, npm.Kind, githubrelease.Kind, githubtag.Kind, gitlabrelease.Kind, crates.Kind, oci.Kind,
	goproxy.Kind, maven.Kind, httpjson.Kind, httpregex.Kind,
}

// urlKinds are the built-in kinds whose URL is a watch coordinate (ADR
// 0038). They take no instance key and no named instance (decision D57 of
// #97; config.Validate refuses both too).
var urlKinds = []string{httpjson.Kind, httpregex.Kind}

// builtinSources registers the built-in source kinds with hc, and one more
// source for each named instance of sources.<name>.kind. A non-nil wrap
// decorates each source (metrics). log may be nil. The names are the ones the
// watch file and the API accept (watchSources), so every stored target can be
// checked. res resolves the stored secrets of sources.<name>.auth at fetch
// time (nil: such a source fails each fetch); the watch file check passes nil.
func builtinSources(cfg *config.Config, hc *http.Client, log *slog.Logger, wrap func(source.Source) source.Source, res registryauth.Resolver) (*source.Registry, error) {
	type entry struct{ name, kind string }
	var entries []entry
	for _, k := range builtinKinds {
		entries = append(entries, entry{k, k})
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Sources)) {
		kind := cfg.Sources[name].Kind
		switch {
		case kind == "" || kind == name:
			continue // a built-in kind (or an unknown key, ignored)
		case slices.Contains(builtinKinds, name):
			return nil, fmt.Errorf("sources.%s.kind: %s is a built-in kind and cannot select another; give the instance its own name", name, name)
		case slices.Contains(urlKinds, kind):
			return nil, fmt.Errorf("sources.%s.kind: %s takes its URL from each watch and has no named instances", name, kind)
		case !slices.Contains(builtinKinds, kind):
			return nil, fmt.Errorf("sources.%s.kind: unknown source kind %q (kinds: %s)", name, kind, strings.Join(builtinKinds, ", "))
		}
		entries = append(entries, entry{name, kind})
	}
	reg := source.NewRegistry()
	for _, e := range entries {
		s := newSource(cfg, e.name, e.kind, hc, log, res)
		if wrap != nil {
			s = wrap(s)
		}
		if err := reg.RegisterAs(e.name, s); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

// newSource builds the source of built-in kind for the configuration
// sources.<name>: its kind itself or an instance of it, with its own base
// URL, token and auth.
func newSource(cfg *config.Config, name, kind string, hc *http.Client, log *slog.Logger, res registryauth.Resolver) source.Source {
	// The URL of http-json and http-regex is the watch's, so they get the
	// netguard client itself: no base URL, no credential (D57).
	switch kind {
	case httpjson.Kind:
		opts := []httpjson.Option{httpjson.WithAllowHTTPHosts(cfg.HTTP.AllowHTTPHosts)}
		if log != nil {
			opts = append(opts, httpjson.WithLogger(log))
		}
		return httpjson.New(hc, opts...)
	case httpregex.Kind:
		opts := []httpregex.Option{httpregex.WithAllowHTTPHosts(cfg.HTTP.AllowHTTPHosts)}
		if log != nil {
			opts = append(opts, httpregex.WithLogger(log))
		}
		return httpregex.New(hc, opts...)
	}
	base := baseURL(cfg, name, publicRegistries[kind])
	c := sourceClient(cfg, name, kind, base, hc, res)
	tok := cfg.SourceToken(name)
	switch kind {
	case pypi.Kind:
		var opts []pypi.Option
		if api := cfg.Sources[name].API; api != "" {
			opts = append(opts, pypi.WithAPI(pypi.API(api)))
		}
		if log != nil {
			opts = append(opts, pypi.WithLogger(log))
		}
		return pypi.New(base, c, opts...)
	case npm.Kind:
		return npm.New(base, c)
	case maven.Kind:
		var opts []maven.Option
		if log != nil {
			opts = append(opts, maven.WithLogger(log))
		}
		return maven.New(base, c, opts...)
	case githubrelease.Kind:
		var opts []githubrelease.Option
		if tok != "" {
			opts = append(opts, githubrelease.WithToken(tok))
		}
		return githubrelease.New(base, c, opts...)
	case githubtag.Kind:
		var opts []githubtag.Option
		if tok != "" {
			opts = append(opts, githubtag.WithToken(tok))
		}
		return githubtag.New(base, c, opts...)
	case gitlabrelease.Kind:
		// token_env sends GitLab's own header, PRIVATE-TOKEN (a personal,
		// project or group access token); auth sends "Authorization:
		// Bearer", which GitLab accepts for the same tokens and for OAuth
		// tokens.
		var opts []gitlabrelease.Option
		if tok != "" {
			opts = append(opts, gitlabrelease.WithToken(tok))
		}
		return gitlabrelease.New(base, c, opts...)
	case oci.Kind:
		// oci has no single default registry: without base_url the source
		// fetches from the registry named in each image (anonymously);
		// with base_url it is pinned to that origin (ADR 0037). It sends no
		// credential yet, so it gets the plain client.
		return oci.New(cfg.Sources[name].BaseURL, hc, oci.WithAllowHTTPHosts(cfg.HTTP.AllowHTTPHosts...))
	case goproxy.Kind:
		var opts []goproxy.Option
		if log != nil {
			opts = append(opts, goproxy.WithLogger(log))
		}
		return goproxy.New(base, c, opts...)
	default: // crates.Kind
		// crates sends no credential: its index and API are public, and a
		// private cargo registry authenticates with a raw token that no
		// source option defines yet.
		return crates.New(base, hc)
	}
}

// sourceClient returns the HTTP client of the source configured as
// sources.<name>, an instance of kind (or the kind itself): hc, or hc adding
// the credential of its auth (stored secrets, bound to the base URL's host)
// or, for pypi, npm, go-proxy and maven, of token_env (github-release, github-tag and
// gitlab-release read their token themselves, each with its own header).
// base is the URL the source reads from.
func sourceClient(cfg *config.Config, name, kind, base string, hc *http.Client, res registryauth.Resolver) *http.Client {
	a := cfg.Sources[name].Auth
	c := registryauth.Config{Resolver: res}
	switch {
	case a.TokenRef != "":
		c.TokenRef = a.TokenRef
	case a.PasswordRef != "":
		c.PasswordRef, c.Username = a.PasswordRef, a.Username
	case kind == pypi.Kind || kind == npm.Kind || kind == goproxy.Kind || kind == maven.Kind:
		c.Token = secrets.NewValueString(cfg.SourceToken(name))
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
	var refs []registryauth.Ref
	for _, k := range slices.Sorted(maps.Keys(cfg.Sources)) {
		a := cfg.Sources[k].Auth
		kind := cfg.SourceKind(k)
		if a.IsZero() || !config.SendsCredential(kind) {
			continue
		}
		u, err := url.Parse(baseURL(cfg, k, publicRegistries[kind]))
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
	goproxy.Kind:       goproxy.DefaultBaseURL,
	maven.Kind:         maven.DefaultBaseURL,
}

// registryURL returns event.CheckInput.RegistryURL for a source (a kind, or
// the name of an instance): its configured base_url when that is not the
// public registry of its kind (for maven, either URL of Maven Central),
// else "".
func registryURL(cfg *config.Config) func(name string) string {
	return func(name string) string {
		b := strings.TrimRight(cfg.Sources[name].BaseURL, "/")
		kind := cfg.SourceKind(name)
		if b == "" || strings.EqualFold(b, strings.TrimRight(publicRegistries[kind], "/")) || (kind == maven.Kind && maven.IsCentral(b)) {
			return ""
		}
		return b
	}
}

// builtinSinks returns the built-in sinks, sending through the netguard
// client nc: HTTP sinks through nc.HTTPClient, the mqtt sink through
// nc.DialContext (ADR 0039) with roots, the instance pool (system roots
// plus http.ca_file). A new built-in kind is added here and to
// watchfile.SinkKinds (a test checks that both list the same kinds). The
// exec sink is added only when exec.commands defines commands (ADR 0042 §1).
func builtinSinks(cfg *config.Config, nc *netguard.Client, roots *x509.CertPool, log *slog.Logger) ([]sink.Sink, error) {
	hc := nc.HTTPClient()
	sinks := []sink.Sink{
		// http.allow_http_hosts is checked at every delivery (ADR 0023,
		// decided question 4 and D12).
		webhook.New(hc, webhook.Options{AllowHTTPHosts: cfg.HTTP.AllowHTTPHosts, Logger: log}),
		githubdispatch.New(hc, githubdispatch.Options{}),
		// Like the webhook, a plain-http base_url is delivered only to a host
		// of http.allow_http_hosts, checked at every delivery (#90, D72).
		gitlabtrigger.New(hc, gitlabtrigger.Options{AllowHTTPHosts: cfg.HTTP.AllowHTTPHosts}),
		// The dispatcher checks the origins it derives from the URL secret
		// (ADR 0026 §2); the sink sends only to them.
		shoutrrr.New(hc, shoutrrr.Options{}),
		// Plain mqtt:// only for hosts of http.allow_http_hosts (D78).
		mqtt.New(nc, mqtt.Options{RootCAs: roots, AllowPlaintextHosts: cfg.HTTP.AllowHTTPHosts}),
	}
	if len(cfg.Exec.Commands) == 0 {
		return sinks, nil
	}
	cmds := make(map[string]execsink.Command, len(cfg.Exec.Commands))
	for name, c := range cfg.Exec.Commands {
		cmds[name] = execsink.Command{Argv: c.Argv, Dir: c.Dir, Env: c.Env, Timeout: c.Timeout, MaxConcurrent: c.MaxConcurrent}
	}
	s, err := execsink.New(execsink.Options{Commands: cmds, Logger: log})
	if err != nil {
		return nil, err
	}
	return append(sinks, s), nil
}

// execSinkKinds is the exec kind when exec.commands defines commands, for
// the plugin catalogue.
func execSinkKinds(cfg *config.Config) []string {
	if len(cfg.Exec.Commands) == 0 {
		return nil
	}
	return []string{execsink.Kind}
}

// checkExecCommands fails when the program of a command of exec.commands
// is missing, not a regular file or not executable (ADR 0042 §2, §5):
// config check, and serve before it starts the dispatcher role, so that a
// node without the command fails fast.
func checkExecCommands(cfg *config.Config) error {
	var errs []error
	for _, name := range cfg.Exec.CommandNames() {
		if err := execsink.CheckCommand(cfg.Exec.Commands[name].Argv[0]); err != nil {
			errs = append(errs, fmt.Errorf("exec.commands.%s.argv[0]: %w", name, err))
		}
	}
	return errors.Join(errs...)
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
// SSRF guard's allowlists from http.allow_networks and
// http.allow_private_hosts (ADR 0040) and the per-host rate limits from
// http.host_limits (ADR 0027). obs may be nil.
func newNetClient(cfg *config.Config, log *slog.Logger, obs netguard.Observer) (*netguard.Client, error) {
	c, _, err := newNetClientRoots(cfg, log, obs)
	return c, err
}

// newNetClientRoots is newNetClient that also returns the CA pool it built
// (system roots plus http.ca_file), for the sinks that do their own TLS
// (mqtt), so the file is read once.
func newNetClientRoots(cfg *config.Config, log *slog.Logger, obs netguard.Observer) (*netguard.Client, *x509.CertPool, error) {
	roots, err := cfg.HTTP.LoadCAPool()
	if err != nil {
		return nil, nil, fmt.Errorf("http client: %w", err)
	}
	hosts, fallback := netguardLimits(cfg)
	c, err := netguard.New(netguard.Config{
		Allow: cfg.HTTP.AllowPrefixes(), AllowHosts: cfg.HTTP.AllowPrivateHosts, UserAgent: cfg.HTTP.UserAgent, Logger: log, Observer: obs, RootCAs: roots,
		Proxy: outgoingProxy, HostLimits: hosts, FallbackLimit: fallback,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("http client: %w", err)
	}
	return c, roots, nil
}

// pipelineDeps is what the scheduler and dispatcher roles are built from.
type pipelineDeps struct {
	cfg     *config.Config
	store   open.Store
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
	client, roots, err := newNetClientRoots(d.cfg, d.log, obs)
	if err != nil {
		return out, err
	}
	if d.metrics != nil {
		d.metrics.SetHostLimits(client.Limits())
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
		demand, err := newHostDemand(d.cfg, client, d.metrics, d.store, d.log)
		if err != nil {
			return out, err
		}
		out.scheduler, err = scheduler.New(d.store, checker, scheduler.Options{
			Logger: d.log, Metrics: schedM, TracerProvider: d.tracer, Demand: demand,
		})
		if err != nil {
			return out, err
		}
	}
	if withDispatcher {
		if err := checkExecCommands(d.cfg); err != nil {
			return out, err
		}
		sinks, err := builtinSinks(d.cfg, client, roots, d.log)
		if err != nil {
			return out, err
		}
		reg := sink.NewRegistry()
		for _, s := range sinks {
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
		selfDest, err := api.SinkSecretDestinations(d.plugins)
		if err != nil {
			return out, err
		}
		out.dispatcher, err = dispatch.New(d.store, reg, dispatch.Options{
			Logger: d.log, Metrics: dispM, TracerProvider: d.tracer,
			Secrets: &secrets.Resolver{Store: d.store, Keys: d.keys}, Destinations: dest,
			SecretDestinations: selfDest, URLCheck: client, AllowHTTPHosts: d.cfg.HTTP.AllowHTTPHosts,
		})
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// openStore opens the store named by the instance configuration (SQLite or
// Postgres, from database.dsn) and runs the stored-sinks check of ADR 0024 §6 before any role starts: every
// role that opens the store refuses one whose sinks hold literal secrets
// (ADR 0024 amendment).
func openStore(ctx context.Context, cfg *config.Config, plugins []api.PluginInfo, log *slog.Logger) (open.Store, error) {
	st, err := open.Open(ctx, cfg.Database.DSN, log)
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
	if log != nil {
		if stored, err := st.TargetSources(ctx); err == nil {
			if reg, err := watchSources(cfg); err == nil {
				var pluginKinds []string
				for _, p := range plugins {
					if p.Interface == "source" && p.Runtime != "builtin" {
						pluginKinds = append(pluginKinds, p.Kind)
					}
				}
				for _, w := range storedSourceWarnings(reg, stored, pluginKinds) {
					log.Warn(w)
				}
			}
		}
	}
	return st, nil
}
