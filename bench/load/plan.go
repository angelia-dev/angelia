package main

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/angelia-dev/angelia/bench/fakereg"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

// sourceKinds maps each fake registry ecosystem to the built-in angelia
// source kind that reads it. An ecosystem without one ("") gets no watches
// and is reported as not measured: oci has no built-in source yet.
func sourceKinds() map[string]string {
	return map[string]string{"pypi": "pypi", "npm": "npm", "crates": "crates", "github": "github-release", "oci": ""}
}

// watchedEcosystems are the ecosystems that get watches, in the order of
// fakereg.Ecosystems.
func watchedEcosystems() []string {
	kinds := sourceKinds()
	var out []string
	for _, e := range fakereg.Ecosystems() {
		if kinds[e] != "" {
			out = append(out, e)
		}
	}
	return out
}

// unmeasuredEcosystems are the fake registry's ecosystems without a source.
func unmeasuredEcosystems() []string {
	kinds := sourceKinds()
	var out []string
	for _, e := range fakereg.Ecosystems() {
		if kinds[e] == "" {
			out = append(out, e)
		}
	}
	return out
}

// GenerateWatches returns n targets spread round robin over the ecosystems
// that have a source, and the number of watches (one package each) per
// ecosystem.
func GenerateWatches(n int) ([]source.Target, map[string]int) {
	eco, kinds := watchedEcosystems(), sourceKinds()
	per := map[string]int{}
	out := make([]source.Target, 0, n)
	for i := range n {
		e, name := eco[i%len(eco)], fakereg.PackageName(i/len(eco))
		per[e]++
		coords := map[string]string{"name": name}
		if e == "github" {
			coords = map[string]string{"repo": "bench/" + name}
		}
		out = append(out, source.Target{Kind: kinds[e], Coordinates: coords})
	}
	return out, per
}

// SizeRegistry gives each ecosystem exactly one package per watch, so that
// every published release belongs to a watched package and counts for lag.
// An ecosystem without watches keeps one package (the minimum) that never
// releases.
func SizeRegistry(c *fakereg.Config, perEco map[string]int) {
	for name, b := range c.Ecosystems {
		b.Packages = max(perEco[name], 1)
		if perEco[name] == 0 {
			b.ReleasesPerHour = 0
		}
		c.Ecosystems[name] = b
	}
}

// WatchLoader is the seam between the load generator and angelia: it hands
// the generated watches to angelia. Prepare runs before `angelia serve`
// starts; it may write files into dir, which angelia sees as angeliaDir,
// and returns extra arguments for `angelia serve`.
type WatchLoader interface {
	Name() string
	Prepare(ctx context.Context, watches []source.Target, dir, angeliaDir string) (serveArgs []string, err error)
}

// watchFileLoader writes the watches as a watch file (docs/watch-file.md),
// which `angelia serve --watches` applies before the scheduler starts. Every
// watch has the same interval and the default policy, so the first check of
// each target records its history as the baseline, without events.
type watchFileLoader struct {
	interval time.Duration
}

// watchFileName is the file the loader writes into the output directory.
const watchFileName = "watches.yaml"

func (watchFileLoader) Name() string { return "watch-file" }

type watchFileDoc struct {
	Version int              `yaml:"version"`
	Watches []watchFileWatch `yaml:"watches"`
}

type watchFileWatch struct {
	Name        string            `yaml:"name"`
	Source      string            `yaml:"source"`
	Coordinates map[string]string `yaml:"coordinates"`
	Interval    string            `yaml:"interval"`
}

func (l watchFileLoader) Prepare(_ context.Context, watches []source.Target, dir, angeliaDir string) ([]string, error) {
	if l.interval < watchfile.MinInterval || l.interval > watchfile.MaxInterval {
		return nil, fmt.Errorf("check interval %v: the watch file accepts %v to %v", l.interval, watchfile.MinInterval, watchfile.MaxInterval)
	}
	doc := watchFileDoc{Version: watchfile.Version, Watches: make([]watchFileWatch, len(watches))}
	for i, t := range watches {
		id := t.Coordinates["name"]
		if id == "" {
			_, id, _ = strings.Cut(t.Coordinates["repo"], "/")
		}
		doc.Watches[i] = watchFileWatch{Name: t.Kind + "-" + id, Source: t.Kind, Coordinates: t.Coordinates, Interval: l.interval.String()}
	}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	if len(data) > watchfile.MaxFileSize {
		return nil, fmt.Errorf("%d watches make a watch file of %d bytes, above its limit of %d", len(watches), len(data), watchfile.MaxFileSize)
	}
	if err := os.WriteFile(filepath.Join(dir, watchFileName), data, 0o600); err != nil {
		return nil, fmt.Errorf("write watch file: %w", err)
	}
	return []string{"--watches", path.Join(filepath.ToSlash(angeliaDir), watchFileName)}, nil
}

// angeliaConfig is the subset of the instance configuration
// (docs/configuration.md) the harness writes.
type angeliaConfig struct {
	Listen   string                       `yaml:"listen"`
	Roles    []string                     `yaml:"roles"`
	Database map[string]string            `yaml:"database"`
	Log      map[string]string            `yaml:"log"`
	HTTP     map[string]any               `yaml:"http"`
	Sources  map[string]map[string]string `yaml:"sources"`
}

// HostLimit is the per-host rate limit of the fake registry's host
// (http.host_limits, ADR 0027 §9).
type HostLimit struct {
	RPS   float64
	Burst int // 0: ceil(RPS), as angelia defaults it
}

// DefaultHostLimit is 1.5 times the 33.3 rps demand of 10,000 targets at
// the 5-minute interval, all on one host, under the fake GitHub limit of
// 80 rps. The result of a run holds only with this limit; BENCH_HOST_RPS=5
// runs at angelia's default limit to show the cap.
var DefaultHostLimit = HostLimit{RPS: 50, Burst: 50}

// effective fills in the default burst.
func (l HostLimit) effective() HostLimit {
	if l.Burst == 0 {
		l.Burst = max(1, int(math.Ceil(l.RPS)))
	}
	return l
}

// registryHostLimit describes the limit WriteAngeliaConfig sets for the
// registry's host, for the report: the host the instance's requests go to.
func registryHostLimit(registry string, l HostLimit) string {
	l = l.effective()
	return fmt.Sprintf("%s %s rps, burst %d (http.host_limits)", registryHost(registry), strconv.FormatFloat(l.RPS, 'f', -1, 64), l.Burst)
}

// registryHost is the host name of the registry URL: the key of the
// instance's rate-limit bucket.
func registryHost(registry string) string {
	u, err := url.Parse(registry)
	if err != nil {
		return registry
	}
	return u.Hostname()
}

// WriteAngeliaConfig writes the instance configuration to path: every
// source's base_url points at the fake registry at registry
// ("http://host:port"), and the database lives in dataDir. allowNetwork
// holds, comma-separated, the prefixes the SSRF guard accepts although
// they are loopback or private (http.allow_networks): the fake registry's
// addresses and no other.
// The registry's host gets the rate limit limit (http.host_limits).
func WriteAngeliaConfig(path, listen, registry, dataDir, allowNetwork string, limit HostLimit) error {
	limit = limit.effective()
	c := angeliaConfig{
		Listen:   listen,
		Roles:    []string{"all"},
		Database: map[string]string{"dsn": "sqlite://" + filepath.ToSlash(filepath.Join(dataDir, "angelia.db"))},
		Log:      map[string]string{"level": "info", "format": "json"},
		HTTP: map[string]any{
			"allow_networks": strings.Split(allowNetwork, ","),
			"host_limits": map[string]any{"hosts": map[string]any{
				registryHost(registry): map[string]any{"rps": limit.RPS, "burst": limit.Burst},
			}},
		},
		Sources: map[string]map[string]string{},
	}
	for _, eco := range watchedEcosystems() {
		c.Sources[sourceKinds()[eco]] = map[string]string{"base_url": registry + "/" + eco}
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
