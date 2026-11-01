package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/angelia-dev/angelia/bench/fakereg"
	"github.com/angelia-dev/angelia/internal/source"
)

// sourceKinds maps each fake registry ecosystem to the angelia source kind
// that reads it.
var sourceKinds = map[string]string{
	"pypi": "pypi", "npm": "npm", "crates": "crates", "github": "github-release", "oci": "oci",
}

// GenerateWatches returns n targets spread round robin over the five
// ecosystems, and the number of packages each ecosystem needs.
func GenerateWatches(n int) ([]source.Target, int) {
	eco := fakereg.Ecosystems
	per := (n + len(eco) - 1) / len(eco)
	out := make([]source.Target, 0, n)
	for i := range n {
		e, name := eco[i%len(eco)], fakereg.PackageName(i/len(eco))
		var coords map[string]string
		switch e {
		case "github":
			coords = map[string]string{"repo": "bench/" + name}
		case "oci":
			// Provisional: ADR 0006 leaves the oci coordinates open.
			coords = map[string]string{"repository": "bench/" + name}
		default:
			coords = map[string]string{"name": name}
		}
		out = append(out, source.Target{Kind: sourceKinds[e], Coordinates: coords})
	}
	return out, max(per, 1)
}

// SizeRegistry sets every ecosystem's package count to packages.
func SizeRegistry(c *fakereg.Config, packages int) {
	for name, b := range c.Ecosystems {
		b.Packages = packages
		c.Ecosystems[name] = b
	}
}

// ErrNoWatchLoader means this build has no way to give angelia the watches.
var ErrNoWatchLoader = errors.New("no watch loader: `angelia serve` cannot be given watches yet " +
	"(needs the watch file loader of #48 and real scheduler/worker roles; see bench/load/README.md)")

// WatchLoader is the seam between the load generator and angelia: it hands
// the generated watches to angelia. Prepare runs before `angelia serve`
// starts; it may write files into dir, which angelia sees as angeliaDir,
// and returns extra arguments for `angelia serve`.
//
// No implementation exists yet: the watch file format is a public contract
// owned by #48, and this harness must not define it.
type WatchLoader interface {
	Name() string
	Prepare(ctx context.Context, watches []source.Target, dir, angeliaDir string) (serveArgs []string, err error)
}

// missingLoader is the only loader today; it always fails with
// ErrNoWatchLoader, so the run measures an idle instance and the report
// says it is incomplete.
type missingLoader struct{}

func (missingLoader) Name() string { return "none" }

func (missingLoader) Prepare(context.Context, []source.Target, string, string) ([]string, error) {
	return nil, ErrNoWatchLoader
}

// angeliaConfig is the subset of the instance configuration
// (docs/configuration.md) the harness writes.
type angeliaConfig struct {
	Listen   string                       `yaml:"listen"`
	Roles    []string                     `yaml:"roles"`
	Database map[string]string            `yaml:"database"`
	Log      map[string]string            `yaml:"log"`
	Sources  map[string]map[string]string `yaml:"sources"`
}

// WriteAngeliaConfig writes the instance configuration to path: every
// source's base_url points at the fake registry at registry
// ("http://host:port"), and the database lives in dataDir.
func WriteAngeliaConfig(path, listen, registry, dataDir string) error {
	c := angeliaConfig{
		Listen:   listen,
		Roles:    []string{"all"},
		Database: map[string]string{"dsn": "sqlite://" + filepath.ToSlash(filepath.Join(dataDir, "angelia.db"))},
		Log:      map[string]string{"level": "info", "format": "json"},
		Sources:  map[string]map[string]string{},
	}
	for eco, kind := range sourceKinds {
		c.Sources[kind] = map[string]string{"base_url": registry + "/" + eco}
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write angelia config: %w", err)
	}
	return nil
}
