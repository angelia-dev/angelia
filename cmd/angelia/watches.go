package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

const watchesFlagHelp = "path of the watch file (docs/watch-file.md); env ANGELIA_WATCHES"

// watchesPath returns the --watches flag, falling back to ANGELIA_WATCHES.
func watchesPath(cmd *cobra.Command) (string, error) {
	p, err := cmd.Flags().GetString("watches")
	if err != nil {
		return "", err
	}
	if p == "" {
		p = os.Getenv("ANGELIA_WATCHES")
	}
	return p, nil
}

// watchSources registers the source kinds a watch file may name. Only
// Source.Validate runs on them (no I/O), so the plain client is never used
// for a request; the scheduler role registers them again on the netguard client
// (newPipelineRoles).
func watchSources(cfg *config.Config) (*source.Registry, error) {
	return builtinSources(cfg, &http.Client{}, nil, nil, nil)
}

func baseURL(cfg *config.Config, kind, def string) string {
	if b := cfg.Sources[kind].BaseURL; b != "" {
		return b
	}
	return def
}

// parseWatchFile validates the watch file with no database and no network.
func parseWatchFile(cfg *config.Config, path string) (*watchfile.File, error) {
	reg, err := watchSources(cfg)
	if err != nil {
		return nil, err
	}
	// http:// is accepted only for the hosts of http.allow_http_hosts
	// (decision D12, ADR 0023 decided question 4).
	f, err := watchfile.Load(path, watchfile.Options{Sources: reg, AllowHTTPHosts: cfg.HTTP.AllowHTTPHosts})
	if err != nil {
		return nil, fmt.Errorf("invalid watch file:\n%w", err)
	}
	return f, nil
}

// applyWatchFile reconciles the watch file into the SQLite store named by the
// instance configuration, before any role starts.
func applyWatchFile(ctx context.Context, cfg *config.Config, path string, log *slog.Logger) error {
	f, err := parseWatchFile(cfg, path)
	if err != nil {
		return err
	}
	for _, w := range f.Warnings {
		log.Warn(w)
	}
	dbPath, err := cfg.Database.SQLitePath()
	if err != nil {
		return fmt.Errorf("--watches: only sqlite databases are supported in this phase: %w", err)
	}
	st, err := sqlite.Open(ctx, dbPath, log)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()
	plan, err := watchfile.Apply(ctx, st, f, watchfile.ApplyOptions{})
	if err != nil {
		return err
	}
	for _, w := range missingSecretRefs(ctx, st, f) {
		log.Warn(w)
	}
	for _, c := range plan.Changes {
		if c.Action != watchfile.Unchanged {
			log.Info("watch file applied", "action", string(c.Action), "kind", c.Kind, "name", c.Name, "detail", c.Detail)
		}
	}
	log.Info("watch file applied", "file", path, "changes", len(plan.Changes), "sinks", len(f.Sinks), "watches", len(f.Watches), "subscriptions", len(f.Subscriptions))
	return nil
}

// hasAPIRole reports whether the process runs the role that owns the file.
func hasAPIRole(roles []string) bool {
	return slices.Contains(roles, "api") || slices.Contains(roles, "all")
}

// missingSecretRefs warns about each *_ref of the watch file's sinks that
// names a secret the store does not hold, as the parser warns about an
// unset *_env variable: deliveries fail, retried, until it is stored.
func missingSecretRefs(ctx context.Context, st secrets.Lookup, f *watchfile.File) []string {
	var out []string
	for _, k := range f.Sinks {
		var cfg map[string]any
		if json.Unmarshal(k.Config, &cfg) != nil {
			continue
		}
		for _, key := range slices.Sorted(maps.Keys(cfg)) {
			name, ok := cfg[key].(string)
			if !ok || !strings.HasSuffix(key, "_ref") {
				continue
			}
			if _, err := st.SecretByName(ctx, name); errors.Is(err, store.ErrNotFound) {
				out = append(out, fmt.Sprintf("line %d: sink %s: config.%s: no stored secret named %s; deliveries fail until `angelia secret set %s` stores it",
					k.Line, k.Name, key, name, name))
			}
		}
	}
	return out
}
