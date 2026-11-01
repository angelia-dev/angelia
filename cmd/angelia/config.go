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
	return &cobra.Command{
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
			if _, err := config.Load(cmd.Context(), config.Options{Path: path, Environ: os.Environ()}); err != nil {
				return fmt.Errorf("invalid configuration: %w", err)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "configuration OK")
			return err
		},
	}
}
