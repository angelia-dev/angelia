// Package compose checks the Docker Compose examples of deploy/compose without
// starting anything: the YAML is decoded with unknown keys rejected, the
// services, volumes, mounts and healthchecks are checked, and, when a docker
// CLI is on PATH, `docker compose config` validates each file. No network.
package compose

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type composeFile struct {
	Name     string             `yaml:"name"`
	Services map[string]service `yaml:"services"`
	Volumes  map[string]any     `yaml:"volumes"`
}

type service struct {
	Image           string               `yaml:"image"`
	Command         []string             `yaml:"command"`
	Profiles        []string             `yaml:"profiles"`
	Ports           []string             `yaml:"ports"`
	Volumes         []string             `yaml:"volumes"`
	Environment     map[string]string    `yaml:"environment"`
	DependsOn       map[string]dependsOn `yaml:"depends_on"`
	Restart         string               `yaml:"restart"`
	StopGracePeriod string               `yaml:"stop_grace_period"`
	Healthcheck     *healthcheck         `yaml:"healthcheck"`
}

type dependsOn struct {
	Condition string `yaml:"condition"`
}

type healthcheck struct {
	Test        []string `yaml:"test"`
	Interval    string   `yaml:"interval"`
	Timeout     string   `yaml:"timeout"`
	StartPeriod string   `yaml:"start_period"`
	Retries     int      `yaml:"retries"`
}

// load decodes a compose file strictly: an unknown or misspelled key is an error.
func load(t *testing.T, path string) composeFile {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // fixed fixture paths of this package
	if err != nil {
		t.Fatal(err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	var f composeFile
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return f
}

// mounts returns the target of every volume entry, keyed by its source.
func mounts(s service) map[string]string {
	out := map[string]string{}
	for _, v := range s.Volumes {
		parts := strings.Split(v, ":")
		if len(parts) >= 2 {
			out[parts[0]] = parts[1]
		}
	}
	return out
}

// targets returns the container paths of the mounts of a service.
func targets(s service) map[string]bool {
	out := map[string]bool{}
	for _, dst := range mounts(s) {
		out[dst] = true
	}
	return out
}

// argAfter returns the value that follows flag in a command, or "".
func argAfter(cmd []string, flag string) string {
	for i := 0; i+1 < len(cmd); i++ {
		if cmd[i] == flag {
			return cmd[i+1]
		}
	}
	return ""
}

// AC: the image healthcheck is the binary's own healthcheck subcommand, since
// the distroless image has no shell or curl.
var wantHealthcheck = []string{"CMD", "/usr/local/bin/angelia", "healthcheck"}

func requireHealthcheck(t *testing.T, name string, s service) {
	t.Helper()
	if s.Healthcheck == nil {
		t.Fatalf("%s: no healthcheck", name)
	}
	if strings.Join(s.Healthcheck.Test, " ") != strings.Join(wantHealthcheck, " ") {
		t.Fatalf("%s: healthcheck test = %q, want %q", name, s.Healthcheck.Test, wantHealthcheck)
	}
}

func TestSQLiteComposeRunsOneAngeliaContainerOnANamedVolume(t *testing.T) {
	f := load(t, "sqlite/compose.yaml")
	angelia, ok := f.Services["angelia"]
	if !ok {
		t.Fatal("sqlite: no angelia service")
	}
	if !strings.HasPrefix(angelia.Image, "ghcr.io/angelia-dev/angelia:") || strings.HasSuffix(angelia.Image, ":latest") {
		t.Fatalf("image = %q, want a pinned ghcr.io/angelia-dev/angelia tag", angelia.Image)
	}
	if len(angelia.Command) == 0 || angelia.Command[0] != "serve" {
		t.Fatalf("command = %q, want serve", angelia.Command)
	}
	if _, ok := f.Volumes["angelia-data"]; !ok {
		t.Fatal("top-level volumes lack angelia-data")
	}
	if mounts(angelia)["angelia-data"] != "/var/lib/angelia" {
		t.Fatalf("angelia-data is not mounted on /var/lib/angelia: %v", mounts(angelia))
	}
	if len(angelia.Ports) == 0 || !strings.HasPrefix(angelia.Ports[0], "127.0.0.1:") {
		t.Fatalf("ports = %q, want the loopback address only", angelia.Ports)
	}
	requireHealthcheck(t, "sqlite angelia", angelia)
	if angelia.StopGracePeriod == "" {
		t.Fatal("no stop_grace_period: the drain timeout (25s) needs more than docker's 10s default")
	}
}

// The named volume is created owned by root, and the image runs as uid 65532:
// a one-shot job must hand the volume over before the server starts.
func TestSQLiteComposeGivesTheVolumeToTheAngeliaUser(t *testing.T) {
	f := load(t, "sqlite/compose.yaml")
	init, ok := f.Services["volume-init"]
	if !ok {
		t.Fatal("sqlite: no volume-init service")
	}
	if !strings.Contains(strings.Join(init.Command, " "), "chown -R 65532:65532 /var/lib/angelia") {
		t.Fatalf("volume-init command = %q, want chown -R 65532:65532 /var/lib/angelia", init.Command)
	}
	if mounts(init)["angelia-data"] != "/var/lib/angelia" {
		t.Fatalf("volume-init does not mount angelia-data: %v", mounts(init))
	}
	dep := f.Services["angelia"].DependsOn["volume-init"]
	if dep.Condition != "service_completed_successfully" {
		t.Fatalf("angelia does not wait for volume-init: depends_on = %v", f.Services["angelia"].DependsOn)
	}
}

// Every bind mount exists, and the config and watch file the command names are
// mounted where the command expects them.
func TestSQLiteComposeMountsItsConfigAndWatchFile(t *testing.T) {
	f := load(t, "sqlite/compose.yaml")
	angelia := f.Services["angelia"]
	for src := range mounts(angelia) {
		if !strings.HasPrefix(src, "./") {
			continue
		}
		if _, err := os.Stat(filepath.Join("sqlite", src)); err != nil {
			t.Errorf("bind mount source %s: %v", src, err)
		}
	}
	for _, flag := range []string{"--config", "--watches"} {
		p := argAfter(angelia.Command, flag)
		if p == "" {
			t.Fatalf("command %q has no %s", angelia.Command, flag)
		}
		if !targets(angelia)[p] {
			t.Errorf("%s %s is not a mount target: %v", flag, p, mounts(angelia))
		}
	}
}

// The full stack is a placeholder: angelia is gated behind a profile because
// the store has no Postgres engine yet (#85), and apprise is not wired (#120).
func TestFullComposeMarksThePlaceholdersAndKeepsTheDefaultUpSafe(t *testing.T) {
	f := load(t, "full/compose.yaml")

	pg, ok := f.Services["postgres"]
	if !ok {
		t.Fatal("full: no postgres service")
	}
	if !strings.HasPrefix(pg.Image, "postgres:") {
		t.Fatalf("postgres image = %q", pg.Image)
	}
	if pg.Healthcheck == nil {
		t.Error("postgres: no healthcheck")
	}

	angelia := f.Services["angelia"]
	if strings.Join(angelia.Profiles, ",") != "placeholder" {
		t.Fatalf("angelia profiles = %q, want the placeholder profile", angelia.Profiles)
	}
	if !strings.HasPrefix(angelia.Environment["ANGELIA_DATABASE_DSN"], "postgres://") {
		t.Fatalf("ANGELIA_DATABASE_DSN = %q, want a postgres:// DSN", angelia.Environment["ANGELIA_DATABASE_DSN"])
	}
	requireHealthcheck(t, "full angelia", angelia)
	if _, ok := angelia.DependsOn["postgres"]; !ok {
		t.Error("angelia does not depend on postgres")
	}

	apprise, ok := f.Services["apprise"]
	if !ok {
		t.Fatal("full: no apprise service")
	}
	if strings.Join(apprise.Profiles, ",") != "not-wired" {
		t.Fatalf("apprise profiles = %q, want the not-wired profile", apprise.Profiles)
	}
	if !strings.HasPrefix(apprise.Image, "caronc/apprise:") {
		t.Fatalf("apprise image = %q", apprise.Image)
	}
	for name, s := range f.Services {
		if _, ok := s.DependsOn["apprise"]; ok {
			t.Errorf("%s depends on apprise, which must not be wired yet", name)
		}
		if strings.Contains(strings.Join(s.Command, " "), "apprise") {
			t.Errorf("%s runs apprise, which must not be wired yet", name)
		}
	}
	if strings.Contains(angelia.Environment["ANGELIA_DATABASE_DSN"], "apprise") {
		t.Error("angelia refers to apprise")
	}
}

// Images are pinned by tag and digest (as the Dockerfile pins its base image),
// except the Angelia image, which has no published digest yet.
func TestComposeThirdPartyImagesArePinnedByDigest(t *testing.T) {
	for _, file := range []string{"sqlite/compose.yaml", "full/compose.yaml"} {
		for name, s := range load(t, file).Services {
			if strings.HasPrefix(s.Image, "ghcr.io/angelia-dev/angelia:") {
				continue
			}
			if !strings.Contains(s.Image, "@sha256:") {
				t.Errorf("%s %s: image %q is not pinned by digest", file, name, s.Image)
			}
		}
	}
}

// AC (validation): when docker is available, `docker compose config` accepts
// every file. The check runs offline: it needs no daemon and pulls nothing.
func TestComposeFilesValidateWithDockerWhenAvailable(t *testing.T) {
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker not on PATH")
	}
	// The full stack refuses to render without a password.
	t.Setenv("POSTGRES_PASSWORD", "test-only-not-a-secret")
	for _, file := range []string{"sqlite/compose.yaml", "full/compose.yaml"} {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		out, err := exec.CommandContext(ctx, docker, "compose", "-f", file, "config", "--quiet").CombinedOutput() //nolint:gosec // docker from PATH, fixed fixture paths
		cancel()
		if err != nil {
			t.Errorf("docker compose config %s: %v\n%s", file, err, out)
		}
	}
}
