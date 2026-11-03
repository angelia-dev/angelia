package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/metrics"
	"github.com/angelia-dev/angelia/internal/supervisor"
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
	if on, _ := cmd.Flags().GetBool("metrics"); on {
		// TODO: hand the *metrics.Metrics to the scheduler, dispatcher and
		// netguard client when those roles replace their stubs (ADR 0019).
		roles = append(roles, metrics.New(log).Role())
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
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}
