package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"

	"github.com/spf13/cobra"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/githubrelease"
	"github.com/angelia-dev/angelia/internal/source/npm"
	"github.com/angelia-dev/angelia/internal/source/pypi"
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
// Source.Validate runs on them (no I/O), so the plain client is never used for
// a request; the scheduler story wires the real, netguarded clients.
func watchSources(cfg *config.Config) (*source.Registry, error) {
	hc := &http.Client{}
	reg := source.NewRegistry()
	for _, s := range []source.Source{
		pypi.New(baseURL(cfg, pypi.Kind, pypi.DefaultBaseURL), hc),
		npm.New(baseURL(cfg, npm.Kind, ""), hc),
		githubrelease.New(baseURL(cfg, githubrelease.Kind, githubrelease.DefaultBaseURL), hc),
	} {
		if err := reg.Register(s); err != nil {
			return nil, err
		}
	}
	return reg, nil
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
	// AllowHTTPHosts stays empty: the instance configuration has no webhook
	// allowlist yet, so webhooks are https only (decision D12).
	f, err := watchfile.Load(path, watchfile.Options{Sources: reg})
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
