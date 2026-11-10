package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/angelia-dev/angelia/bench/fakereg"
)

// secretEnv is the variable that holds the webhook signing secret in
// angelia's environment. The value is made per run and never written down.
const secretEnv = "BENCH_WEBHOOK_SECRET" //nolint:gosec // a variable name, not a credential

// hostLimit is the per-host rate limit set for the registry and the
// receiver: the highest http.host_limits accepts, so that dispatch is
// limited by angelia, not by its pacing of one host.
var hostLimit = map[string]any{"rps": 100, "burst": 200}

// workload is what the harness asks of angelia.
type workload struct {
	Packages      int     `json:"packages"`       // pypi watches, one package each
	Subscriptions int     `json:"subscriptions"`  // webhook sinks, one subscription each, all matching every event
	ReleasesPerH  float64 `json:"releases_per_h"` // over all packages
	History       int     `json:"history"`        // versions each package has before the run (the baseline)
}

// registryConfig is the fake registry behaviour: only pypi has packages,
// with a low latency so that checks are quick.
func registryConfig(w workload) *fakereg.Config {
	c := fakereg.DefaultConfig()
	for name, b := range c.Ecosystems {
		b.Packages, b.ReleasesPerHour = 1, 0
		if name == "pypi" {
			b.Packages, b.ReleasesPerHour, b.History = w.Packages, w.ReleasesPerH, w.History
		}
		b.Latency, b.LatencyJitter = 2e6, 8e6 // 2 ms + [0, 8 ms)
		c.Ecosystems[name] = b
	}
	return c
}

func packageName(i int) string { return fmt.Sprintf("pkg-%05d", i) }

// sinkName is the name of sink i, also the last element of its URL path,
// which is how the receiver tells sinks apart.
func sinkName(i int) string { return fmt.Sprintf("hook-%d", i) }

type watchFile struct {
	Version       int            `yaml:"version"`
	Watches       []watchEntry   `yaml:"watches"`
	Sinks         []sinkEntry    `yaml:"sinks"`
	Subscriptions []subscription `yaml:"subscriptions"`
}

type watchEntry struct {
	Name        string            `yaml:"name"`
	Source      string            `yaml:"source"`
	Coordinates map[string]string `yaml:"coordinates"`
	Interval    string            `yaml:"interval"`
}

type sinkEntry struct {
	Name   string            `yaml:"name"`
	Kind   string            `yaml:"kind"`
	Config map[string]string `yaml:"config"`
}

type subscription struct {
	Name string `yaml:"name"`
	Sink string `yaml:"sink"`
}

// writeWatchFile writes the first n watches of the workload, one webhook
// sink per subscription pointing at receiver ("http://host:port"), and the
// subscriptions.
func writeWatchFile(path, receiver string, w workload, n int) error {
	f := watchFile{Version: 1}
	for i := range min(n, w.Packages) {
		f.Watches = append(f.Watches, watchEntry{
			Name: "pypi-" + packageName(i), Source: "pypi",
			Coordinates: map[string]string{"name": packageName(i)}, Interval: "5m",
		})
	}
	for i := range w.Subscriptions {
		f.Sinks = append(f.Sinks, sinkEntry{Name: sinkName(i), Kind: "webhook", Config: map[string]string{
			"url": receiver + "/hook/" + sinkName(i), "secret_env": secretEnv, "timeout": "5s",
		}})
		f.Subscriptions = append(f.Subscriptions, subscription{Name: fmt.Sprintf("sub-%d", i), Sink: sinkName(i)})
	}
	return writeYAML(path, f)
}

// writeInstanceConfig writes angelia.yaml: the database in dataDir, pypi
// at the fake registry, and the registry and the receiver allowed past the
// SSRF guard (plain http to the receiver's host only).
func writeInstanceConfig(path, listen, dataDir, registry, receiver string) error {
	reg, err := url.Parse(registry)
	if err != nil {
		return err
	}
	rcv, err := url.Parse(receiver)
	if err != nil {
		return err
	}
	c := map[string]any{
		"listen":   listen,
		"roles":    []string{"all"},
		"database": map[string]string{"dsn": "sqlite://" + filepath.ToSlash(filepath.Join(dataDir, "angelia.db"))},
		"log":      map[string]string{"level": "info", "format": "json"},
		"http": map[string]any{
			"allow_networks":   []string{reg.Hostname() + "/32", rcv.Hostname() + "/32"},
			"allow_http_hosts": []string{rcv.Hostname()},
			"host_limits": map[string]any{"hosts": map[string]any{
				reg.Hostname(): hostLimit, rcv.Hostname(): hostLimit,
			}},
		},
		"sources": map[string]any{"pypi": map[string]string{"base_url": registry + "/pypi"}},
	}
	return writeYAML(path, c)
}

func writeYAML(path string, v any) error {
	data, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
