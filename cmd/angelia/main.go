// Command angelia is the single Angelia binary: server, workers and CLI.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/angelia-dev/angelia/internal/buildinfo"
)

func main() {
	os.Exit(run())
}

// run executes the root command. SIGINT and SIGTERM cancel the context
// passed to subcommands so they can shut down cleanly.
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		return 1
	}
	return 0
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:          "angelia",
		Short:        "The self-hosted release event bus",
		SilenceUsage: true,
	}
	root.PersistentFlags().String("config", "", "path to the instance config file (default $ANGELIA_CONFIG)")
	root.AddCommand(newVersionCmd(), newConfigCmd())
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
