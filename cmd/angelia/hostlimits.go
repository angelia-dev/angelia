package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/url"
	"slices"
	"strings"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/metrics"
	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/scheduler"
	"github.com/angelia-dev/angelia/internal/source/crates"
)

// builtinSourceHosts are the hosts of the built-in sources' default base
// URLs: the public registries of ADR 0027 §6.
func builtinSourceHosts() []string {
	urls := slices.Collect(maps.Values(publicRegistries))
	urls = append(urls, crates.DefaultAPIURL)
	var hosts []string
	for _, raw := range urls {
		if u, err := url.Parse(raw); err == nil && !slices.Contains(hosts, normHost(u.Hostname())) {
			hosts = append(hosts, normHost(u.Hostname()))
		}
	}
	slices.Sort(hosts)
	return hosts
}

func normHost(h string) string { return strings.TrimSuffix(strings.ToLower(h), ".") }

// hostLimitWarnings returns the warnings of ADR 0027 §6 on
// http.host_limits. A host counts as used when it is the host of a
// configured source's base_url, of a built-in source's default, or an
// allowed host of an installed plugin (manifest or grants).
func hostLimitWarnings(cfg *config.Config, plugins []config.InstalledPlugin) []string {
	public := builtinSourceHosts()
	used := slices.Clone(public)
	for _, s := range cfg.Sources {
		if u, err := url.Parse(s.BaseURL); err == nil && u.Hostname() != "" {
			used = append(used, normHost(u.Hostname()))
		}
	}
	for _, p := range plugins {
		for _, h := range slices.Concat(p.ManifestHosts, p.Grants.AllowedHosts) {
			used = append(used, normHost(h))
		}
	}
	return cfg.HTTP.HostLimits.Warnings(public, used)
}

// serveHostLimitWarnings is hostLimitWarnings for the serve start log. serve
// does not load plugins, so the plugin folders are read here only for their
// allowed hosts, and only when http.host_limits.hosts lists a host; a
// folder that cannot be read adds none.
func serveHostLimitWarnings(ctx context.Context, cfg *config.Config, environ []string) []string {
	if len(cfg.HTTP.HostLimits.Hosts) == 0 {
		// Only the hosts entries need the plugins' hosts.
		return cfg.HTTP.HostLimits.Warnings(nil, nil)
	}
	plugins, _, _ := config.LoadPlugins(ctx, cfg.PluginsDir, environ)
	return hostLimitWarnings(cfg, plugins)
}

// netguardLimits returns the netguard host limits and fallback of cfg.
func netguardLimits(cfg *config.Config) (map[string]netguard.HostLimit, netguard.HostLimit) {
	return cfg.HTTP.HostLimits.Netguard()
}

// printHostLimits lists the effective limits table, with the origin of
// each line (ADR 0027 §6), in the style of the plugin listing.
func printHostLimits(w io.Writer, cfg *config.Config) error {
	hosts, fallback := netguardLimits(cfg)
	table, err := netguard.EffectiveLimits(hosts, fallback)
	if err != nil {
		return fmt.Errorf("http.host_limits: %w", err)
	}
	var b strings.Builder
	b.WriteString("http host limits (per process):\n")
	for _, e := range table {
		name := e.Host
		if e.Fallback {
			name = "other hosts"
		}
		_, configured := hosts[e.Host]
		fmt.Fprintf(&b, "  %-24s %s (%s)\n", name, describeLimits(e), limitOrigin(e, configured))
	}
	_, err = io.WriteString(w, b.String())
	return err
}

func describeLimit(l netguard.Limit) string {
	return fmt.Sprintf("%s rps, burst %d", config.FormatRate(l.Rate), l.Burst)
}

func describeLimits(e netguard.EffectiveLimit) string {
	if e.Authenticated == e.Anonymous {
		return describeLimit(e.Anonymous)
	}
	return describeLimit(e.Authenticated) + " with a token; " + describeLimit(e.Anonymous) + " without"
}

// limitOrigin names where a line of the table comes from. configured is
// true when http.host_limits.hosts lists the host.
func limitOrigin(e netguard.EffectiveLimit, configured bool) string {
	switch {
	case e.Fallback && e.AnonymousOrigin == netguard.OriginDefault:
		return "config default"
	case e.Fallback:
		return "built-in default"
	case configured && (e.AuthenticatedOrigin == netguard.OriginBuiltin || e.AnonymousOrigin == netguard.OriginBuiltin):
		return "config, capped at built-in"
	case configured:
		return "config"
	}
	return "built-in"
}

// sourceHost returns the host the source called name (a kind or a named
// instance) sends its requests to, and whether it sends a credential, which
// selects the authenticated bucket of that host. ok is false when the host
// is unknown, as for a source that is not built in (a plugin).
func sourceHost(cfg *config.Config, name string) (host string, authenticated, ok bool) {
	kind := cfg.SourceKind(name)
	def, builtin := publicRegistries[kind]
	if !builtin {
		return "", false, false
	}
	u, err := url.Parse(baseURL(cfg, name, def))
	if err != nil || u.Hostname() == "" {
		return "", false, false
	}
	a := cfg.Sources[name].Auth
	authenticated = config.SendsCredential(kind) && (cfg.SourceToken(name) != "" || !a.IsZero())
	return normHost(u.Hostname()), authenticated, true
}

// newHostDemand returns the host demand estimate of the scheduler (story
// #220): the host of each source from the configuration, and the limit of
// each host from the client, so that the estimate and the rate limiter agree.
// m may be nil.
func newHostDemand(cfg *config.Config, client *netguard.Client, m *metrics.Metrics, st scheduler.DemandStore, log *slog.Logger) (*scheduler.Demand, error) {
	opts := scheduler.DemandOptions{
		Store:      st,
		SourceHost: func(name string) (string, bool, bool) { return sourceHost(cfg, name) },
		Limit: func(host string, authenticated bool) scheduler.HostLimitInfo {
			e := client.LimitFor(host)
			l := e.Anonymous
			if authenticated {
				l = e.Authenticated
			}
			return scheduler.HostLimitInfo{
				Label: e.Host, RPS: l.Rate, Burst: l.Burst, Fixed: netguard.IsBuiltinBudgetHost(host),
			}
		},
		Logger: log,
	}
	if m != nil {
		opts.Report = m.SetHostDemand
	}
	return scheduler.NewDemand(opts)
}
