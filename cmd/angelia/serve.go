package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/angelia-dev/angelia/internal/buildinfo"
	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/metrics"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
	"github.com/angelia-dev/angelia/internal/supervisor"
	"github.com/angelia-dev/angelia/internal/telemetry"
)

func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the selected roles in this process",
		Args:  cobra.NoArgs,
		RunE:  runServe,
	}
	cmd.Flags().String("roles", "", "comma-separated roles: api,scheduler,worker,dispatcher or all (default from config, else all)")
	cmd.Flags().String("watches", "", watchesFlagHelp)
	cmd.Flags().Bool("metrics", true, "serve Prometheus metrics on /metrics of the listen address")
	cmd.Flags().Duration("drain-timeout", supervisor.DefaultDrainTimeout, "how long to wait for in-flight work on shutdown")
	return cmd
}

func runServe(cmd *cobra.Command, _ []string) error {
	path, err := cmd.Flags().GetString("config")
	if err != nil {
		return err
	}
	if path == "" {
		path = os.Getenv("ANGELIA_CONFIG")
	}
	cfg, err := config.Load(cmd.Context(), config.Options{Path: path, Environ: os.Environ()})
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	roleNames := cfg.Roles
	if cmd.Flags().Changed("roles") {
		v, _ := cmd.Flags().GetString("roles")
		roleNames = splitRoles(v)
	}
	drain, _ := cmd.Flags().GetDuration("drain-timeout")

	log := newLogger(cmd.ErrOrStderr(), cfg.Log)
	// Tracing is off unless the standard OTEL_* variables enable an OTLP
	// exporter (ADR 0020). It is not part of angelia.yaml.
	tracerProvider, stopTracing, err := telemetry.Setup(cmd.Context(), os.Getenv, buildinfo.Version(), log)
	if err != nil {
		return fmt.Errorf("tracing: %w", err)
	}
	defer func() {
		if err := stopTracing(cmd.Context()); err != nil {
			log.Warn("flush traces", "error", err)
		}
	}()
	// The final flush comes out of the drain budget, so that drain plus flush
	// stays within --drain-timeout (and the Kubernetes grace period).
	if telemetry.Enabled(os.Getenv) && drain > 2*telemetry.FlushTimeout {
		drain -= telemetry.FlushTimeout
	}
	for _, w := range cfg.Warnings() {
		log.Warn(w)
	}
	keys, err := loadKeys(cfg, log)
	if err != nil {
		return err
	}
	// Every role shares the store, so every role refuses to start while
	// stored secrets cannot be read: no plaintext fallback (ADR 0024 §4).
	if err := checkSecretsAtStartup(cmd.Context(), cfg, keys); err != nil {
		return err
	}
	watches, err := watchesPath(cmd)
	if err != nil {
		return err
	}
	if watches != "" {
		if !hasAPIRole(roleNames) {
			return fmt.Errorf("--watches: the watch file is applied by the api role, but this process runs %s", strings.Join(roleNames, ","))
		}
		if err := applyWatchFile(cmd.Context(), cfg, watches, log); err != nil {
			return err
		}
	}
	// Expand and check the role names before anything is opened.
	selected, err := supervisor.Select(roleNames, supervisor.NewStub)
	if err != nil {
		return fmt.Errorf("--roles: %w", err)
	}
	want := map[string]bool{}
	for _, r := range selected {
		want[r.Name()] = true
	}
	var m *metrics.Metrics
	if on, _ := cmd.Flags().GetBool("metrics"); on {
		m = metrics.New(log)
	}
	built := map[string]supervisor.Role{}
	if want["api"] || want["scheduler"] || want["dispatcher"] {
		sources, err := watchSources(cfg)
		if err != nil {
			return err
		}
		plugins := builtinPlugins(sources.Kinds())
		// Every role that opens the store runs the stored-sinks check of
		// ADR 0024 §6 here, before any role starts.
		st, err := openStore(cmd.Context(), cfg, plugins, log)
		if err != nil {
			return err
		}
		defer func() { _ = st.Close() }()
		if m != nil {
			m.RegisterDeliveryCounts(st)
		}
		if want["api"] {
			srv, err := newAPIServer(cfg, st, sources, keys, log)
			if err != nil {
				return err
			}
			built["api"] = srv.Role()
		}
		p, err := newPipelineRoles(pipelineDeps{
			cfg: cfg, store: st, keys: keys, plugins: plugins, tracer: tracerProvider, metrics: m, log: log,
		}, want["scheduler"], want["dispatcher"])
		if err != nil {
			return err
		}
		if p.scheduler != nil {
			built["scheduler"] = p.scheduler
		}
		if p.dispatcher != nil {
			built["dispatcher"] = p.dispatcher
		}
	}
	// The worker role has no work of its own yet: the scheduler checks
	// targets on its own worker pool (architecture.md §3.2).
	roles, err := supervisor.Select(roleNames, func(name string) supervisor.Role {
		if r, ok := built[name]; ok {
			return r
		}
		return supervisor.NewStub(name)
	})
	if err != nil {
		return fmt.Errorf("--roles: %w", err)
	}
	if m != nil {
		roles = append(roles, m.Role())
	}
	s := &supervisor.Supervisor{
		Roles:        roles,
		Listen:       cfg.Listen,
		DrainTimeout: drain,
		Logger:       log,
		// TODO(#46): replace with the store's "migrations applied" check.
		Ready: supervisor.ReadyFunc(func(context.Context) error { return nil }),
	}
	return s.Run(cmd.Context())
}

func splitRoles(v string) []string {
	var out []string
	for _, r := range strings.Split(v, ",") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}

func newLogger(w io.Writer, c config.Log) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(c.Level)); err != nil {
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if c.Format == "text" {
		return slog.New(telemetry.NewLogHandler(slog.NewTextHandler(w, opts)))
	}
	return slog.New(telemetry.NewLogHandler(slog.NewJSONHandler(w, opts)))
}

// checkSecretsAtStartup counts the stored secrets per key, read-only, and
// runs secrets.CheckCounts: with stored secrets and no key, or rows under a
// key that is not configured, the instance does not start. A database that
// does not exist yet holds no secret.
func checkSecretsAtStartup(ctx context.Context, cfg *config.Config, keys *secrets.Keyring) error {
	dbPath, err := cfg.Database.SQLitePath()
	if err != nil {
		// TODO(postgres, #85): check the Postgres store too; no role runs on
		// it in this phase.
		return nil //nolint:nilerr // only sqlite stores exist in this phase
	}
	counts, err := sqlite.SecretKeyCounts(ctx, dbPath)
	if errors.Is(err, sqlite.ErrNoDatabase) {
		return nil
	}
	if err != nil {
		return err
	}
	return secrets.CheckCounts(counts, keys)
}
