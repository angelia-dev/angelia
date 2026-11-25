package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/angelia-dev/angelia/internal/manifest"
)

// `angelia import` (story #98, ADR 0036) reads dependency manifests and prints
// a watch file with one watch per registry dependency. It makes no network
// request and reads no client configuration: the output is applied with
// `angelia apply -f -` or loaded with `serve --watches`.

// osFS opens the paths the user typed, relative to the working directory or
// absolute, with the separators of the platform.
type osFS struct{}

func (osFS) Open(name string) (fs.File, error) { return os.Open(filepath.FromSlash(name)) } //nolint:gosec // the file the user named

type importFlags struct {
	dev      []string
	project  string
	labels   []string
	include  []string
	exclude  []string
	sources  []string
	interval string
	policy   string
	subscr   string
	format   string
	strict   bool
	verbose  bool
}

func newImportCmd() *cobra.Command {
	var f importFlags
	cmd := &cobra.Command{
		Use:   "import FILE... --project NAME [--dev FILE]... [-l key=value]...",
		Short: "Print a watch file for the dependencies of requirements.txt and package.json",
		Long: `Import reads dependency manifests and prints a watch file (docs/watch-file.md) with
one watch per registry dependency, named <project>-<kind>-<package> and labelled
with the owner labels and dependency=runtime|optional|dev|peer. It makes no
network request and needs no server: pipe the output to apply.

  angelia import requirements.txt --dev requirements-dev.txt --project api \
    | angelia apply -f - --prune -l project=api

Running it again on the same manifests prints the same bytes. Version pins and
ranges are ignored: every watch records the versions known at its first check
and reports later releases. Lines that are not registry dependencies (URLs,
VCS, local paths, editable installs, constraint files) are skipped with a
warning on stderr; --strict makes them an error. In package.json, dependencies,
optionalDependencies and devDependencies are read (peerDependencies with
--include peer). See docs/cli.md, "Import".`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runImport(cmd, args, f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.project, "project", "", "project name: the prefix of every watch name and the default owner label (required)")
	fl.StringArrayVar(&f.dev, "dev", nil, "requirements file whose dependencies are dev dependencies (repeatable)")
	fl.StringArrayVarP(&f.labels, "label", "l", nil, "owner label key=value, replacing project=<project> (repeatable; use the same pairs as apply --prune -l)")
	fl.StringSliceVar(&f.include, "include", nil, "dependency types to import: runtime,optional,dev,peer (default: all but peer)")
	fl.StringArrayVar(&f.exclude, "exclude", nil, "glob of package names to leave out (repeatable)")
	fl.StringArrayVar(&f.sources, "source", nil, "KIND=NAME or @SCOPE=NAME: use the named source instance for every watch of a kind (pypi=pypi-internal, npm=npm-mirror) or of an npm scope (@acme=npm-internal; wins over npm=); repeatable")
	fl.StringVar(&f.interval, "interval", "", "check interval of every watch, e.g. 6h (default: the server's default)")
	fl.StringVar(&f.policy, "policy", "", "YAML file with a watch policy block, copied into every watch")
	fl.StringVar(&f.subscr, "subscribe", "", "also print a subscription <project>-deps on this existing sink, for the owner labels")
	fl.StringVar(&f.format, "format", "auto", "manifest format: auto, "+strings.Join(manifest.Formats, ", "))
	fl.BoolVar(&f.strict, "strict", false, "fail when a line or dependency is skipped")
	fl.BoolVarP(&f.verbose, "verbose", "v", false, "print one line per dependency on stderr")
	_ = cmd.MarkFlagRequired("project")
	_ = cmd.MarkFlagFilename("dev")
	_ = cmd.MarkFlagFilename("policy", "yaml", "yml")
	return cmd
}

// manifestFile is one input with its format.
type manifestFile struct {
	name   string
	format string
	dev    bool
}

func runImport(cmd *cobra.Command, args []string, f importFlags) error {
	if len(args) == 0 && len(f.dev) == 0 {
		return errors.New("name at least one manifest file, for example: angelia import requirements.txt --project api")
	}
	files, err := importFiles(args, f)
	if err != nil {
		return err
	}
	opts := manifest.Options{
		Project: f.project, Include: f.include, Exclude: f.exclude,
		Interval: f.interval, Subscribe: f.subscr,
	}
	if opts.Labels, err = parseOwnerLabels(f.labels); err != nil {
		return err
	}
	if opts.Sources, err = manifest.ParseSourceMappings(f.sources); err != nil {
		return err
	}
	if f.policy != "" {
		b, err := os.ReadFile(f.policy)
		if err != nil {
			return fmt.Errorf("read %s: %w", f.policy, errors.Unwrap(err))
		}
		opts.Policy, opts.PolicyName = b, f.policy
	}

	var deps []manifest.Dependency
	var skips []manifest.Warning
	for _, mf := range files {
		opts.Inputs = append(opts.Inputs, mf.name)
		res, err := manifest.Parse(mf.format, osFS{}, filepath.ToSlash(mf.name), mf.dev)
		if err != nil {
			return err
		}
		deps = append(deps, res.Deps...)
		for _, w := range res.Warnings {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning: "+w.String())
			if w.Skip {
				skips = append(skips, w)
			}
		}
	}
	if f.strict && len(skips) > 0 {
		return fmt.Errorf("--strict: %d skipped, see the warnings above", len(skips))
	}
	out, err := manifest.Generate(deps, opts)
	if err != nil {
		return err
	}
	if f.verbose {
		for _, e := range out.Entries {
			target := e.Watch
			if e.Excluded {
				target = "(excluded by --exclude)"
			}
			if e.Dep.Pin != "" {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s %s -> %s\n", e.Dep.Name, e.Dep.Pin, target)
			} else {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s -> %s\n", e.Dep.Name, target)
			}
		}
	}
	if len(out.Entries) == 0 {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning: no dependencies to watch; apply --prune would refuse an empty file without --prune-allow-empty")
	}
	_, err = cmd.OutOrStdout().Write(out.YAML)
	return err
}

// importFiles chooses the format of each input.
func importFiles(args []string, f importFlags) ([]manifestFile, error) {
	if f.format != "auto" && !slices.Contains(manifest.Formats, f.format) {
		return nil, fmt.Errorf("--format %q: want auto or %s", f.format, strings.Join(manifest.Formats, ", "))
	}
	var out []manifestFile
	add := func(name string, dev bool) error {
		format := f.format
		if format == "auto" {
			if format = manifest.FormatOf(name); format == "" {
				return fmt.Errorf("%s: cannot tell the format from the file name (requirements*.txt, package.json); pass --format", name)
			}
		}
		out = append(out, manifestFile{name: name, format: format, dev: dev})
		return nil
	}
	for _, a := range args {
		if err := add(a, false); err != nil {
			return nil, err
		}
	}
	for _, d := range f.dev {
		if err := add(d, true); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// parseOwnerLabels reads -l key=value. The result is nil without -l (the
// generator then uses project=<project>); label syntax is checked by the
// watch-file parser, before any output.
func parseOwnerLabels(vals []string) (map[string]string, error) {
	if len(vals) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, v := range vals {
		k, val, ok := strings.Cut(v, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("-l %q: want key=value", v)
		}
		if prev, dup := out[k]; dup && prev != val {
			return nil, fmt.Errorf("-l: label %q is given twice with different values", k)
		}
		out[k] = val
	}
	return out, nil
}
