package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/store"
)

// Checks of the source names of sources.<name> (named instances, #205)
// against what the process knows: installed plugins, built-in kinds and the
// targets already stored.

// checkInstancePluginClash fails when a named instance (a sources key with
// kind set) has the kind of an installed plugin: a watch's source would be
// ambiguous between the plugin and the instance. Built-in kinds that
// instances may not take are refused earlier, by builtinSources.
func checkInstancePluginClash(cfg *config.Config, plugins []config.InstalledPlugin) error {
	var bad []string
	for _, name := range sortedSourceNames(cfg) {
		if k := cfg.Sources[name].Kind; k == "" || k == name {
			continue
		}
		if slices.ContainsFunc(plugins, func(p config.InstalledPlugin) bool { return p.Kind == name }) {
			bad = append(bad, fmt.Sprintf("sources.%s: a plugin of kind %s is installed; rename the instance", name, name))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("invalid configuration: %s", strings.Join(bad, "; "))
	}
	return nil
}

// sourceKeyWarnings warns about a sources key that is neither a built-in
// kind, an installed plugin's kind nor a named instance (no kind): nothing
// reads it, which is usually a typo in the name.
func sourceKeyWarnings(cfg *config.Config, plugins []config.InstalledPlugin) []string {
	var out []string
	for _, name := range sortedSourceNames(cfg) {
		if cfg.Sources[name].Kind != "" || slices.Contains(builtinKinds, name) ||
			slices.ContainsFunc(plugins, func(p config.InstalledPlugin) bool { return p.Kind == name }) {
			continue
		}
		out = append(out, fmt.Sprintf("sources.%s: not a built-in source kind or an installed plugin, and no kind is set; the key is ignored (to define an instance, set sources.%s.kind)", name, name))
	}
	return out
}

// storedSourceWarnings warns about stored targets whose source cannot be
// checked as they were created: the name is no longer configured, or it is
// now of another kind. Sources of plugins (pluginKinds) are not built in and
// are not checked here.
func storedSourceWarnings(reg *source.Registry, stored []store.TargetSource, pluginKinds []string) []string {
	var out []string
	for _, ts := range stored {
		if slices.Contains(pluginKinds, ts.Name) {
			continue
		}
		want := ts.Kind
		if want == "" {
			want = ts.Name
		}
		src, err := reg.Get(ts.Name)
		switch {
		case err != nil:
			out = append(out, fmt.Sprintf("%d stored target%s use the source %q, which is not configured; their checks fail until it is configured again", ts.Targets, pluralS(ts.Targets), ts.Name))
		case src.Kind() != want:
			out = append(out, fmt.Sprintf("%d stored target%s of the source %q were created for kind %s, but it is now of kind %s; their checks are refused (give the source a new name)", ts.Targets, pluralS(ts.Targets), ts.Name, want, src.Kind()))
		}
	}
	return out
}

func pluralS(n int64) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func sortedSourceNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Sources))
	for n := range cfg.Sources {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}
