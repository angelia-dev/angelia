package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/angelia-dev/angelia/internal/buildinfo"
	"github.com/angelia-dev/angelia/internal/config"
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
	roles, err := supervisor.Select(roleNames, supervisor.NewStub)
	if err != nil {
		return fmt.Errorf("--roles: %w", err)
	}
	// TODO: pass tracerProvider as Options.TracerProvider of the scheduler and
	// dispatcher roles when serve wires them in place of the stubs.
	_ = tracerProvider
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
