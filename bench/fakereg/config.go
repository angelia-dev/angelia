// Package fakereg is a fake package registry for the Phase 0 scale
// benchmark (architecture.md section 6, benchmark 1). One HTTP server
// answers like PyPI, npm, crates.io, the GitHub releases API and an OCI
// registry's tag list, each under its own path prefix, with configurable
// latency, ETag behaviour, release rate and 429 responses. See README.md.
//
// Packages are synthetic ("pkg-00000" ...) and their versions are a pure
// function of the configuration and the time since the server started, so a
// run is reproducible and the server knows when each release was published.
// That lets it measure check lag: the time from a release's publication to
// the first response that contains it.
package fakereg

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"gopkg.in/yaml.v3"
)

// Ecosystems returns the simulated registries, in a new slice. Each is
// served under "/<name>/", so a source's base URL is "http://HOST/<name>".
func Ecosystems() []string { return []string{"pypi", "npm", "crates", "github", "oci"} }

// ETag modes.
const (
	ETagStrong = "strong" // `"hash"`, 304 on a matching If-None-Match
	ETagWeak   = "weak"   // `W/"hash"`, 304 on a weak match
	ETagNone   = "none"   // no ETag header; If-None-Match is ignored
	ETagIgnore = "ignore" // ETag sent but never honoured: always 200
)

// Behaviour is how one ecosystem answers. The zero value is not valid; use
// DefaultBehaviour as the starting point.
type Behaviour struct {
	// Packages is the number of packages, named pkg-00000 upwards.
	Packages int `yaml:"packages"`
	// History is the number of versions each package has at start.
	History int `yaml:"history"`
	// ReleasesPerHour is the release rate over the whole ecosystem. Every
	// package publishes at the same interval (Packages/ReleasesPerHour
	// hours), each with its own deterministic phase. 0 means no releases.
	ReleasesPerHour float64 `yaml:"releases_per_hour"`
	// Latency is added to every response, plus a uniform random part in
	// [0, LatencyJitter).
	Latency       time.Duration `yaml:"latency"`
	LatencyJitter time.Duration `yaml:"latency_jitter"`
	// ETag is one of strong, weak, none, ignore.
	ETag string `yaml:"etag"`
	// LastModified sends Last-Modified and honours If-Modified-Since.
	LastModified bool `yaml:"last_modified"`
	// RateLimit is a token bucket over the ecosystem; requests beyond it get
	// 429 with Retry-After. RPS 0 turns it off.
	RateLimit RateLimit `yaml:"rate_limit"`
	// Random429 is the probability that a request gets 429 regardless of
	// the rate limit.
	Random429 float64 `yaml:"random_429"`
	// BodyPadding adds this many bytes to every 200 response body, to model
	// real document sizes (an npm packument is often hundreds of KB).
	BodyPadding int `yaml:"body_padding"`
	// PrimaryLimit is GitHub's primary rate limit; github only.
	PrimaryLimit PrimaryLimit `yaml:"primary_limit"`
}

// PrimaryLimit models GitHub's primary rate limit: Requests per fixed
// Window, answered with 403 and x-ratelimit-remaining: 0 once used up.
// A 304 does not count, as on GitHub. Requests 0 turns it off.
type PrimaryLimit struct {
	Requests int           `yaml:"requests"`
	Window   time.Duration `yaml:"window"`
}

// RateLimit is a token bucket.
type RateLimit struct {
	RPS        float64       `yaml:"rps"`
	Burst      int           `yaml:"burst"`
	RetryAfter time.Duration `yaml:"retry_after"`
}

// Config is the whole fake registry configuration.
type Config struct {
	// Seed changes every package's release phase.
	Seed uint64
	// Ecosystems holds the behaviour of each name in Ecosystems.
	Ecosystems map[string]Behaviour
}

// DefaultBehaviour is used for every key not set in the file.
func DefaultBehaviour() Behaviour {
	return Behaviour{
		Packages:        2000,
		History:         20,
		ReleasesPerHour: 200,
		Latency:         30 * time.Millisecond,
		LatencyJitter:   70 * time.Millisecond,
		ETag:            ETagStrong,
		LastModified:    true,
		RateLimit:       RateLimit{RetryAfter: time.Second},
	}
}

// DefaultConfig returns DefaultBehaviour for every ecosystem.
func DefaultConfig() *Config {
	c := &Config{Seed: 1, Ecosystems: map[string]Behaviour{}}
	for _, e := range Ecosystems() {
		c.Ecosystems[e] = DefaultBehaviour()
	}
	return c
}

// LoadConfig reads a YAML file. See ParseConfig.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the operator names the file
	if err != nil {
		return nil, fmt.Errorf("fakereg config: %w", err)
	}
	c, err := ParseConfig(data)
	if err != nil {
		return nil, fmt.Errorf("fakereg config %s: %w", path, err)
	}
	return c, nil
}

// ParseConfig parses the YAML format documented in README.md: "seed",
// "defaults" (a Behaviour applied to every ecosystem) and "ecosystems"
// (per-ecosystem keys applied on top of the defaults). Unknown keys are
// errors.
func ParseConfig(data []byte) (*Config, error) {
	var raw struct {
		Seed       *uint64              `yaml:"seed"`
		Defaults   yaml.Node            `yaml:"defaults"`
		Ecosystems map[string]yaml.Node `yaml:"ecosystems"`
	}
	if err := decodeStrict(data, &raw); err != nil {
		return nil, err
	}
	base := DefaultBehaviour()
	if err := decodeNode(&raw.Defaults, &base); err != nil {
		return nil, fmt.Errorf("defaults: %w", err)
	}
	c := &Config{Seed: 1, Ecosystems: map[string]Behaviour{}}
	if raw.Seed != nil {
		c.Seed = *raw.Seed
	}
	for _, e := range Ecosystems() {
		c.Ecosystems[e] = base
	}
	for name, node := range raw.Ecosystems {
		b, ok := c.Ecosystems[name]
		if !ok {
			return nil, fmt.Errorf("ecosystems: unknown ecosystem %q (want one of %v)", name, Ecosystems())
		}
		if err := decodeNode(&node, &b); err != nil {
			return nil, fmt.Errorf("ecosystems.%s: %w", name, err)
		}
		c.Ecosystems[name] = b
	}
	return c, c.Validate()
}

// decodeNode decodes n on top of the values already in out, rejecting
// unknown keys (yaml.Node.Decode cannot do that itself).
func decodeNode(n *yaml.Node, out *Behaviour) error {
	if n.Kind == 0 {
		return nil
	}
	data, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	return decodeStrict(data, out)
}

func decodeStrict(data []byte, out any) error {
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// Validate checks every ecosystem's behaviour.
func (c *Config) Validate() error {
	var errs []error
	for _, name := range Ecosystems() {
		b, ok := c.Ecosystems[name]
		if !ok {
			errs = append(errs, fmt.Errorf("ecosystem %s: missing", name))
			continue
		}
		if err := b.validate(); err != nil {
			errs = append(errs, fmt.Errorf("ecosystem %s: %w", name, err))
		}
		if b.PrimaryLimit.Requests != 0 && name != "github" {
			errs = append(errs, fmt.Errorf("ecosystem %s: primary_limit is only supported for github", name))
		}
	}
	return errors.Join(errs...)
}

func (b Behaviour) validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if b.Packages < 1 || b.Packages > 99999 {
		bad("packages must be between 1 and 99999, got %d", b.Packages)
	}
	if b.History < 1 {
		bad("history must be at least 1, got %d", b.History)
	}
	if b.ReleasesPerHour < 0 {
		bad("releases_per_hour must not be negative")
	}
	if b.Latency < 0 || b.LatencyJitter < 0 {
		bad("latency and latency_jitter must not be negative")
	}
	if !slices.Contains([]string{ETagStrong, ETagWeak, ETagNone, ETagIgnore}, b.ETag) {
		bad("etag must be strong, weak, none or ignore, got %q", b.ETag)
	}
	if b.RateLimit.RPS < 0 || b.RateLimit.Burst < 0 || b.RateLimit.RetryAfter < 0 {
		bad("rate_limit values must not be negative")
	}
	if b.RateLimit.RPS > 0 && b.RateLimit.Burst < 1 {
		bad("rate_limit.burst must be at least 1 when rps is set")
	}
	if pl := b.PrimaryLimit; pl.Requests < 0 || (pl.Requests > 0 && pl.Window <= 0) {
		bad("primary_limit needs requests >= 0 and a positive window")
	}
	if b.Random429 < 0 || b.Random429 > 1 {
		bad("random_429 must be between 0 and 1, got %v", b.Random429)
	}
	if b.BodyPadding < 0 || b.BodyPadding > 16<<20 {
		bad("body_padding must be between 0 and 16 MiB, got %d", b.BodyPadding)
	}
	return errors.Join(errs...)
}
