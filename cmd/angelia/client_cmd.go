package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/angelia-dev/angelia/internal/apiclient"
)

// maxBodyBytes bounds a resource file read by `put`: the server accepts 1 MiB.
const maxBodyBytes = 1 << 20

// cliEnv is what a client command needs: an API client, the output format and
// the streams. It is the unit the commands of #73 (apply) and #115 (replay)
// reuse.
type cliEnv struct {
	API    *apiclient.API
	Format string
	Out    io.Writer
	Err    io.Writer
}

// addClientFlags adds --server, --token and -o to a command group.
func addClientFlags(cmd *cobra.Command) {
	pf := cmd.PersistentFlags()
	pf.String("server", "", "base URL of the instance; env "+envServer+", then the client config file, default "+DefaultServer)
	pf.String("token", "", "API token; env "+envToken+", then the client config file (a flag is visible in process lists)")
	pf.StringP("output", "o", formatTable, "output format: table or json")
	_ = cmd.RegisterFlagCompletionFunc("output", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{formatTable, formatJSON}, cobra.ShellCompDirectiveNoFileComp
	})
}

// newCLIEnv resolves the connection (flags, environment, client config file)
// and builds the API client for a command added with addClientFlags.
func newCLIEnv(cmd *cobra.Command) (*cliEnv, error) {
	fl := cmd.Flags()
	server, err := fl.GetString("server")
	if err != nil {
		return nil, err
	}
	token, err := fl.GetString("token")
	if err != nil {
		return nil, err
	}
	of, err := fl.GetString("output")
	if err != nil {
		return nil, err
	}
	format, err := parseFormat(of)
	if err != nil {
		return nil, err
	}
	conn, err := resolveConnection(server, token, os.Getenv)
	if err != nil {
		return nil, err
	}
	a, err := apiclient.New(apiclient.Options{Server: conn.Server, Token: conn.Token, UserAgent: "angelia-cli"})
	if err != nil {
		return nil, err
	}
	return &cliEnv{API: a, Format: format, Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr()}, nil
}

// fetchPage gets one page: its items and next cursor.
type fetchPage[T any] func(ctx context.Context, cursor *string) ([]T, *string, error)

// collect runs fetch from cursor (empty for the first page). With all it
// follows next_cursor to the end; otherwise it returns the first page.
func collect[T any](ctx context.Context, fetch fetchPage[T], cursor string, all bool) (page[T], error) {
	out := page[T]{Items: []T{}}
	var cur *string
	if cursor != "" {
		cur = &cursor
	}
	for {
		items, next, err := fetch(ctx, cur)
		if err != nil {
			return page[T]{}, err
		}
		out.Items = append(out.Items, items...)
		out.NextCursor = next
		if !all || next == nil {
			return out, nil
		}
		cur = next
	}
}

// listFlags are the paging flags of the list commands.
type listFlags struct {
	limit  int
	cursor string
	all    bool
}

func (l *listFlags) add(cmd *cobra.Command) {
	cmd.Flags().IntVar(&l.limit, "limit", 0, "page size (the server's default when 0)")
	cmd.Flags().StringVar(&l.cursor, "cursor", "", "continue after this next_cursor")
	cmd.Flags().BoolVar(&l.all, "all", false, "follow the cursor and list everything")
}

func (l *listFlags) limitPtr() *int {
	if l.limit <= 0 {
		return nil
	}
	v := l.limit
	return &v
}

// printList prints a collected page. The cursor to continue goes to stderr so
// that the table stays clean to pipe.
func (e *cliEnv) printList(p any, next *string, t table) error {
	if err := render(e.Out, e.Format, p, t); err != nil {
		return err
	}
	if e.Format == formatTable && next != nil {
		_, _ = fmt.Fprintf(e.Err, "more results: --cursor %s (or --all)\n", *next)
	}
	return nil
}

// readResourceFile reads the JSON or YAML document of a `put` (a file, or "-"
// for stdin) and returns it as JSON. Anything but an object is refused.
func readResourceFile(in io.Reader, path string) ([]byte, error) {
	var r io.Reader
	if path == "-" {
		r = in
	} else {
		f, err := os.Open(path) //nolint:gosec // the file the user named
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(raw) > maxBodyBytes {
		return nil, fmt.Errorf("%s is larger than the server accepts (1 MiB)", path)
	}
	return toJSONObject(raw)
}

// toJSONObject turns a YAML or JSON document into a JSON object. YAML is a
// superset of JSON, so one decoder reads both.
func toJSONObject(raw []byte) ([]byte, error) {
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse resource file: %w", err)
	}
	obj, ok := doc.(map[string]any)
	if !ok {
		return nil, errors.New("the resource file must hold one object (JSON or YAML)")
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("convert resource file to JSON: %w", err)
	}
	return b, nil
}

// putFlags are the flags of a put command.
type putFlags struct {
	file   string
	dryRun bool
}

func (p *putFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringVarP(&p.file, "file", "f", "", "JSON or YAML file with the resource, or - for stdin")
	cmd.Flags().BoolVar(&p.dryRun, "dry-run", false, "validate and show the result without writing")
	_ = cmd.MarkFlagRequired("file")
	_ = cmd.MarkFlagFilename("file", "json", "yaml", "yml")
}

func (p *putFlags) body(cmd *cobra.Command) (io.Reader, error) {
	b, err := readResourceFile(cmd.InOrStdin(), p.file)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(b), nil
}

// reportChange says on stderr what a write did, from the Angelia-Change header.
func (e *cliEnv) reportChange(kind, name string, res apiclient.Result, dryRun bool) {
	change := res.Change
	if change == "" {
		change = "written"
	}
	suffix := ""
	if dryRun {
		suffix = " (dry run)"
	}
	_, _ = fmt.Fprintf(e.Err, "%s %q: %s%s\n", kind, name, change, suffix)
}

func dryRunPtr(b bool) *bool {
	if !b {
		return nil
	}
	return &b
}

// deleteReport says on stderr that a delete worked.
func (e *cliEnv) deleteReport(kind, name string, dryRun bool) {
	if dryRun {
		_, _ = fmt.Fprintf(e.Err, "%s %q: would be deleted (dry run)\n", kind, name)
		return
	}
	_, _ = fmt.Fprintf(e.Err, "%s %q: deleted\n", kind, name)
}

// jsonContentType is the content type of every request body.
const jsonContentType = "application/json"
