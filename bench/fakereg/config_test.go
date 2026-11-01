package fakereg

import (
	"strings"
	"testing"
	"time"
)

func TestParseConfigMergesDefaultsAndEcosystems(t *testing.T) {
	c, err := ParseConfig([]byte(`
seed: 7
defaults:
  packages: 50
  latency: 5ms
  rate_limit: {retry_after: 3s}
ecosystems:
  github:
    etag: weak
    rate_limit: {rps: 10, burst: 20}
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Seed != 7 {
		t.Errorf("seed = %d, want 7", c.Seed)
	}
	gh, npm := c.Ecosystems["github"], c.Ecosystems["npm"]
	if gh.Packages != 50 || gh.Latency != 5*time.Millisecond || gh.ETag != ETagWeak {
		t.Errorf("github = %+v: defaults not applied under the override", gh)
	}
	// A partial nested override keeps the keys it does not set.
	if gh.RateLimit != (RateLimit{RPS: 10, Burst: 20, RetryAfter: 3 * time.Second}) {
		t.Errorf("github rate_limit = %+v", gh.RateLimit)
	}
	if npm.ETag != ETagStrong || npm.History != DefaultBehaviour().History || npm.RateLimit.RPS != 0 {
		t.Errorf("npm = %+v, want defaults", npm)
	}
	if len(c.Ecosystems) != len(Ecosystems) {
		t.Errorf("got %d ecosystems, want %d", len(c.Ecosystems), len(Ecosystems))
	}
}

func TestParseConfigEmptyIsDefault(t *testing.T) {
	c, err := ParseConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range Ecosystems {
		if c.Ecosystems[e] != DefaultBehaviour() {
			t.Errorf("%s = %+v, want DefaultBehaviour", e, c.Ecosystems[e])
		}
	}
}

func TestParseConfigErrors(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"unknown top key":       {"sed: 1", "field sed not found"},
		"unknown behaviour key": {"defaults: {latncy: 1s}", "field latncy not found"},
		"unknown eco key":       {"ecosystems: {npm: {etagg: none}}", "ecosystems.npm"},
		"unknown ecosystem":     {"ecosystems: {maven: {}}", `unknown ecosystem "maven"`},
		"bad etag":              {"defaults: {etag: sometimes}", "etag must be"},
		"bad probability":       {"ecosystems: {oci: {random_429: 1.5}}", "ecosystem oci: random_429"},
		"burst missing":         {"defaults: {rate_limit: {rps: 5}}", "burst must be at least 1"},
		"no packages":           {"defaults: {packages: 0}", "packages must be"},
		"no history":            {"defaults: {history: 0}", "history must be"},
		"negative latency":      {"defaults: {latency: -1s}", "must not be negative"},
		"negative rate":         {"defaults: {releases_per_hour: -1}", "releases_per_hour"},
		"huge padding":          {"defaults: {body_padding: 999999999}", "body_padding"},
		"bad duration":          {"defaults: {latency: soon}", "defaults: yaml"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// The documented example file must stay valid.
func TestExampleConfigLoads(t *testing.T) {
	c, err := LoadConfig("fakereg.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.Ecosystems["github"].RateLimit.RPS == 0 {
		t.Error("example should show a GitHub rate limit")
	}
	if _, err := LoadConfig("does-not-exist.yaml"); err == nil {
		t.Error("missing file: want error")
	}
}
