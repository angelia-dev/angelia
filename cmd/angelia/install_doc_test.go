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
			cmd, rest, err := root.Find(args)
			// A typo in the last word resolves to the parent, which must not
			// accept it: require a runnable leaf with no words left over.
			if err != nil || cmd == root || !cmd.Runnable() || (cmd.HasSubCommands() && len(rest) > 0) {
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

// AC: both compose examples pin the same published or planned Angelia tag, and
// not the 0.1.0 that was never published.
func TestComposeExamplesPinTheSameAngeliaTag(t *testing.T) {
	re := regexp.MustCompile(`(?m)^\s*image: ghcr\.io/angelia-dev/angelia:(\S+)$`)
	var tags []string
	for _, dir := range []string{"sqlite", "full"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "compose", dir, "compose.yaml")) //nolint:gosec // fixed fixture paths
		if err != nil {
			t.Fatal(err)
		}
		m := re.FindStringSubmatch(string(raw))
		if m == nil {
			t.Fatalf("%s/compose.yaml pins no ghcr.io/angelia-dev/angelia tag", dir)
		}
		tags = append(tags, m[1])
	}
	if tags[0] != tags[1] {
		t.Errorf("compose examples pin different tags: %v", tags)
	}
	if tags[0] == "0.1.0" || tags[0] == "latest" {
		t.Errorf("compose tag %q was never published or is not a pin", tags[0])
	}
}

// The `docker run ghcr.io/angelia-dev/angelia:<tag> token generate` lines of
// the guides use the tag the compose files pin.
func TestTokenGenerateDocsUseThePinnedTag(t *testing.T) {
	pin := regexp.MustCompile(`(?m)^\s*image: ghcr\.io/angelia-dev/angelia:(\S+)$`)
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "compose", "sqlite", "compose.yaml")) //nolint:gosec // fixed fixture path
	if err != nil {
		t.Fatal(err)
	}
	m := pin.FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("sqlite/compose.yaml pins no tag")
	}
	use := regexp.MustCompile(`ghcr\.io/angelia-dev/angelia:(\S+) token generate`)
	seen := 0
	for _, f := range []string{"README.md", "docs/install.md", "deploy/compose/README.md"} {
		doc, err := os.ReadFile(filepath.Join("..", "..", f)) //nolint:gosec // fixed fixture paths
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range use.FindAllStringSubmatch(string(doc), -1) {
			seen++
			if u[1] != m[1] {
				t.Errorf("%s runs token generate with tag %s, compose pins %s", f, u[1], m[1])
			}
		}
	}
	if seen < 2 {
		t.Errorf("found %d token generate lines, want at least 2 (install.md and the compose README)", seen)
	}
}
