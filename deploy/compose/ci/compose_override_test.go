package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type overlay struct {
	Services map[string]struct {
		Image       string            `yaml:"image"`
		PullPolicy  string            `yaml:"pull_policy"`
		DependsOn   map[string]any    `yaml:"depends_on"`
		Entrypoint  []string          `yaml:"entrypoint"`
		Environment map[string]string `yaml:"environment"`
		Ports       []string          `yaml:"ports"`
		Volumes     []string          `yaml:"volumes"`
	} `yaml:"services"`
	Networks struct {
		Default struct {
			IPAM struct {
				Config []struct {
					Subnet string `yaml:"subnet"`
				} `yaml:"config"`
			} `yaml:"ipam"`
		} `yaml:"default"`
	} `yaml:"networks"`
}

func decodeStrict(t *testing.T, path string, out any) {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // fixed fixture path of this package
	if err != nil {
		t.Fatal(err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// AC 3: the overlay adds the registry and the receiver, loopback-only on the
// host, pins the third-party image, and fixes the network to the one /24 that
// angelia.yaml lets the SSRF guard accept. Neither list is wider than that.
func TestOverlayAddsTheDoublesAndPinsTheNetworkToTheAllowedPrefix(t *testing.T) {
	var o overlay
	decodeStrict(t, "compose.override.yaml", &o)
	for name, port := range map[string]string{"registry": "127.0.0.1:8099:8099", "receiver": "127.0.0.1:9000:9000"} {
		s, ok := o.Services[name]
		if !ok {
			t.Fatalf("service %s missing", name)
		}
		if len(s.Ports) != 1 || s.Ports[0] != port {
			t.Errorf("%s ports = %v, want only %s", name, s.Ports, port)
		}
		if !strings.Contains(s.Image, "@sha256:") {
			t.Errorf("%s image %q is not pinned by digest", name, s.Image)
		}
		if len(s.Entrypoint) == 0 || s.Entrypoint[0] != "/ciharness" {
			t.Errorf("%s entrypoint = %v", name, s.Entrypoint)
		}
	}
	for _, name := range []string{"angelia", "receiver"} {
		if !strings.Contains(o.Services[name].Environment["CI_WEBHOOK_SECRET"], "CI_WEBHOOK_SECRET") {
			t.Errorf("%s does not get CI_WEBHOOK_SECRET from the environment", name)
		}
	}
	if o.Services["angelia"].Image == "" || len(o.Services["angelia"].Volumes) != 2 {
		t.Errorf("angelia overlay = %+v, want an image and the two replaced mounts", o.Services["angelia"])
	}

	var cfg struct {
		HTTP struct {
			AllowNetworks  []string `yaml:"allow_networks"`
			AllowHTTPHosts []string `yaml:"allow_http_hosts"`
		} `yaml:"http"`
	}
	raw, err := os.ReadFile("angelia.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if got := o.Services["angelia"].PullPolicy; got != "never" {
		t.Errorf("angelia pull_policy = %q, want never: the image is built from the checkout", got)
	}
	subnets := o.Networks.Default.IPAM.Config
	if len(subnets) != 1 || len(cfg.HTTP.AllowNetworks) != 1 || subnets[0].Subnet != cfg.HTTP.AllowNetworks[0] {
		t.Errorf("network subnets %v and http.allow_networks %v must be the same single prefix", subnets, cfg.HTTP.AllowNetworks)
	}
	if len(cfg.HTTP.AllowHTTPHosts) != 1 || cfg.HTTP.AllowHTTPHosts[0] != "receiver" {
		t.Errorf("http.allow_http_hosts = %v, want only the receiver", cfg.HTTP.AllowHTTPHosts)
	}
}
