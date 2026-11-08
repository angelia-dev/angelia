package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/url"
	"slices"
	"strings"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/netguard"
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
