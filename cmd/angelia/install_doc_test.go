package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/config"
)

const installDoc = "../../docs/install.md"

var (
	markedBlockRE = regexp.MustCompile("(?s)<!-- install-check: ([a-z-]+) -->\n```[a-z]*\n(.*?)```")
	shellBlockRE  = regexp.MustCompile("(?s)```sh\n(.*?)```")
)

// installBlocks returns the fenced blocks of docs/install.md that carry an
// `install-check` marker, keyed by the marker.
func installBlocks(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(installDoc)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, m := range markedBlockRE.FindAllStringSubmatch(string(raw), -1) {
		out[m[1]] = m[2]
	}
	for _, k := range []string{"config", "watches", "unit", "first-event-config", "first-event-watches"} {
		if out[k] == "" {
			t.Fatalf("docs/install.md has no block marked %q", k)
		}
	}
	return out
}

func loadDocPair(t *testing.T, cfgText, watchesText string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "angelia.yaml")
	watchesPath := filepath.Join(dir, "watches.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(watchesPath, []byte(watchesText), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(context.Background(), config.Options{Path: cfgPath, Environ: []string{}})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if _, err := parseWatchFile(cfg, watchesPath); err != nil {
		t.Fatalf("watch file: %v", err)
	}
}

// AC: the example config and watch file of install.md load with the real loaders.
func TestInstallDocExamplesLoadWithTheRealLoaders(t *testing.T) {
	b := installBlocks(t)
	t.Run("minimal", func(t *testing.T) { loadDocPair(t, b["config"], b["watches"]) })
	t.Run("first event", func(t *testing.T) { loadDocPair(t, b["first-event-config"], b["first-event-watches"]) })
}

// The systemd unit runs the serve command with the flags and paths the guide uses.
func TestInstallDocUnitRunsServeWithTheDocumentedFiles(t *testing.T) {
	unit := installBlocks(t)["unit"]
	want := "ExecStart=/usr/local/bin/angelia serve --config /etc/angelia/angelia.yaml --watches /etc/angelia/watches.yaml"
	if !strings.Contains(unit, want) {
		t.Fatalf("unit lacks %q", want)
	}
}

// AC: every `angelia ...` command of install.md names a real subcommand and
// real flags of the CLI.
func TestInstallDocCommandsExistInTheCLI(t *testing.T) {
	raw, err := os.ReadFile(installDoc)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, blk := range shellBlockRE.FindAllStringSubmatch(string(raw), -1) {
		for _, line := range strings.Split(strings.ReplaceAll(blk[1], "\\\n", " "), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 || fields[0] != "angelia" {
				continue
			}
			var args, flags []string
			for _, f := range fields[1:] {
				switch {
				case strings.HasPrefix(f, "#"):
				case strings.HasPrefix(f, "--"):
					flags = append(flags, strings.SplitN(f, "=", 2)[0])
				default:
					args = append(args, f)
				}
				if strings.HasPrefix(f, "#") {
					break
				}
			}
			root := newRootCmd()
			cmd, _, err := root.Find(args)
			if err != nil || cmd == root {
				t.Errorf("%q: not a command of the CLI", line)
				continue
			}
			seen++
			for _, fl := range flags {
				name := strings.TrimPrefix(fl, "--")
				if cmd.Flags().Lookup(name) == nil && cmd.InheritedFlags().Lookup(name) == nil {
					t.Errorf("%q: %s has no flag %s", line, cmd.CommandPath(), fl)
				}
			}
		}
	}
	if seen < 4 {
		t.Fatalf("checked only %d commands; the extraction is broken", seen)
	}
}

// AC: the compose examples pin the planned 0.2.0, the first image with Postgres.
func TestComposeExamplesPinTheNextRelease(t *testing.T) {
	for _, dir := range []string{"sqlite", "full"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "compose", dir, "compose.yaml")) //nolint:gosec // fixed fixture paths
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "image: ghcr.io/angelia-dev/angelia:0.2.0\n") {
			t.Errorf("%s/compose.yaml does not pin ghcr.io/angelia-dev/angelia:0.2.0", dir)
		}
	}
}
