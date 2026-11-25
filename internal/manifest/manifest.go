// Package manifest reads dependency manifests (requirements.txt, package.json)
// and generates a watch file with one watch per registry dependency
// (`angelia import`, story #98, ADR 0036). It does no network I/O and reads
// only the files it is given, through an [fs.FS].
package manifest

import (
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// Dependency types, in the order of precedence when a package appears with
// several (docs/cli.md, "Import").
const (
	TypeRuntime  = "runtime"
	TypeOptional = "optional"
	TypeDev      = "dev"
	TypePeer     = "peer"
)

// Types lists the dependency types by precedence.
var Types = []string{TypeRuntime, TypeOptional, TypeDev, TypePeer}

// Source kinds of the generated watches.
const (
	KindPyPI = "pypi"
	KindNPM  = "npm"
)

// Origin is where a dependency was declared.
type Origin struct {
	File string
	Line int
}

func (o Origin) String() string {
	if o.Line > 0 {
		return fmt.Sprintf("%s:%d", o.File, o.Line)
	}
	return o.File
}

// Dependency is one registry dependency of a manifest.
type Dependency struct {
	Name   string // as written in the manifest
	Kind   string // source kind: KindPyPI
	Type   string // one of Types
	Pin    string // exact pinned version, empty when the manifest gives none
	Origin Origin
}

// Warning is a finding that does not stop the import.
type Warning struct {
	Origin Origin
	Msg    string
	// Skip is true when a dependency or a whole line was left out of the
	// result; --strict turns these into errors.
	Skip bool
}

func (w Warning) String() string { return fmt.Sprintf("%s: %s", w.Origin, w.Msg) }

// Result is what a parser returns.
type Result struct {
	Deps     []Dependency
	Warnings []Warning
}

// precedence returns the rank of a type: lower wins.
func precedence(t string) int {
	for i, x := range Types {
		if x == t {
			return i
		}
	}
	return len(Types)
}

// readFile reads name from fsys.
func readFile(fsys fs.FS, name string) ([]byte, error) {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, unwrapPath(err))
	}
	return b, nil
}

func unwrapPath(err error) error {
	if pe, ok := err.(*fs.PathError); ok {
		return pe.Err
	}
	return err
}

// Format names.
const (
	FormatRequirements = "requirements"
	FormatPackageJSON  = "package-json"
)

// FormatOf returns the format of a manifest file name: "requirements" for
// requirements*.txt and *.requirements.txt, "package-json" for package.json,
// or "" when it is not recognised.
func FormatOf(name string) string {
	base := path.Base(name)
	if base == "package.json" {
		return FormatPackageJSON
	}
	if strings.HasSuffix(base, ".txt") && (strings.HasPrefix(base, "requirements") || strings.HasSuffix(base, ".requirements.txt")) {
		return FormatRequirements
	}
	return ""
}

// Formats lists the format names Parse knows.
var Formats = []string{FormatRequirements, FormatPackageJSON}

// Parse reads the manifest name of the given format. dev marks the
// dependencies of a requirements file as dev dependencies; it is an error
// for a package.json.
func Parse(format string, fsys fs.FS, name string, dev bool) (*Result, error) {
	switch format {
	case FormatRequirements:
		return ParseRequirements(fsys, name, dev)
	case FormatPackageJSON:
		if dev {
			return nil, fmt.Errorf("%s: --dev applies to requirements files; package.json lists its own devDependencies", name)
		}
		return ParsePackageJSON(fsys, name)
	}
	return nil, fmt.Errorf("unknown format %q", format)
}
