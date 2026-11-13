package main

import (
	"io"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/angelia-dev/angelia/internal/buildinfo"
	"github.com/angelia-dev/angelia/internal/mcp"
)

// newMCPCmd is `angelia mcp`: an MCP server on stdio for an agent host, a
// client of the REST API with the CLI's connection (docs/mcp.md, ADR 0029).
// stdout carries only MCP frames; logs go to stderr.
func newMCPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve the MCP tools on stdio, as a client of an Angelia instance",
		Long: `Serve the MCP tools (list_watches, latest_version, events_since, add_watch,
ack_event) over stdio, for an agent host that starts this command. Every tool
call is a REST call to the instance, with the token of the CLI connection
(--server, --token, ANGELIA_SERVER, ANGELIA_TOKEN or the client config file).
It never opens the database. stdout carries only MCP messages; logs go to
stderr.`,
		Args: cobra.NoArgs,
		RunE: runMCP,
	}
	addConnectionFlags(cmd, false)
	return cmd
}

func runMCP(cmd *cobra.Command, _ []string) error {
	a, conn, err := connectFromFlags(cmd, "angelia-mcp")
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: slog.LevelInfo}))
	log.Info("serving MCP on stdio", "connection", conn.String())
	in, ok := cmd.InOrStdin().(io.ReadCloser)
	if !ok {
		in = io.NopCloser(cmd.InOrStdin())
	}
	return mcp.Serve(cmd.Context(), mcp.Options{API: a, Version: buildinfo.Version(), Logger: log}, in, nopWriteCloser{cmd.OutOrStdout()})
}

// nopWriteCloser keeps the MCP server from closing stdout.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
