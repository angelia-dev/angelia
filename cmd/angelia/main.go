// Command angelia is the single Angelia binary: server, workers and CLI.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/angelia-dev/angelia/internal/buildinfo"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:          "angelia",
		Short:        "The self-hosted release event bus",
		SilenceUsage: true,
	}
	root.AddCommand(newVersionCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version, commit and build date",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "angelia %s\n", buildinfo.String())
			return err
		},
	}
}
