package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/angelia-dev/angelia/internal/config"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect the instance configuration",
	}
	cmd.AddCommand(newConfigCheckCmd())
	return cmd
}

func newConfigCheckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Validate the instance configuration and exit non-zero if invalid",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
			for _, w := range cfg.Warnings() {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w)
			}
			if _, err = fmt.Fprintln(cmd.OutOrStdout(), "configuration OK"); err != nil {
				return err
			}
			watches, err := watchesPath(cmd)
			if err != nil || watches == "" {
				return err
			}
			f, err := parseWatchFile(cfg, watches)
			if err != nil {
				return err
			}
			for _, w := range f.Warnings {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "watch file OK: %d sinks, %d watches, %d subscriptions\n", len(f.Sinks), len(f.Watches), len(f.Subscriptions))
			return err
		},
	}
	cmd.Flags().String("watches", "", watchesFlagHelp)
	return cmd
}
