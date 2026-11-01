package wasm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
)

// ErrInvalidManifest is wrapped by every manifest validation error.
var ErrInvalidManifest = errors.New("wasm: invalid plugin manifest")

// Manifest is the draft plugin manifest (architecture.md section 3.6). It is
// a prototype and not yet a public contract.
type Manifest struct {
	// Kind is the source kind the plugin registers, for example "crates".
	Kind string `json:"kind"`
	// ConfigSchema is the JSON Schema of the plugin config map (Options.Config).
	// The host only checks that every config key is one of its top-level
	// "properties"; full JSON Schema validation is not done yet.
	ConfigSchema json.RawMessage `json:"config_schema,omitempty"`
	// AllowedHosts lists the hostnames or IP literals the plugin may reach
	// through http_request. Matching is exact and case-insensitive, on every
	// hop including redirects, on any port. There are no wildcards.
	AllowedHosts []string `json:"allowed_hosts"`
}

var (
	kindRE  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	labelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// ParseManifest decodes and validates a JSON manifest. Unknown fields are
// rejected so that a typo cannot silently widen or drop a restriction.
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// Validate checks the manifest.
func (m Manifest) Validate() error {
	if !kindRE.MatchString(m.Kind) {
		return fmt.Errorf("%w: kind %q must match %s", ErrInvalidManifest, m.Kind, kindRE)
	}
	for _, h := range m.AllowedHosts {
		if _, err := normAllowedHost(h); err != nil {
			return fmt.Errorf("%w: allowed_hosts: %w", ErrInvalidManifest, err)
		}
	}
	if _, err := m.configKeys(); err != nil {
		return err
	}
	return nil
}

// normAllowedHost returns the canonical form of an allowed_hosts entry: a
// lower-case hostname without a trailing dot, or a canonical IP literal.
func normAllowedHost(h string) (string, error) {
	if addr, err := netip.ParseAddr(h); err == nil && addr.Zone() == "" {
		return addr.String(), nil
	}
	n := strings.TrimSuffix(strings.ToLower(h), ".")
	if n == "" || len(n) > 253 {
		return "", fmt.Errorf("%q is not a hostname", h)
	}
	for l := range strings.SplitSeq(n, ".") {
		if !labelRE.MatchString(l) {
			return "", fmt.Errorf("%q is not a hostname (no scheme, port, path or wildcard)", h)
		}
	}
	return n, nil
}

// normHost returns the form of a URL hostname compared with allowed_hosts.
func normHost(h string) string {
	if addr, err := netip.ParseAddr(h); err == nil {
		return addr.String()
	}
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// configKeys returns the top-level properties declared by ConfigSchema.
func (m Manifest) configKeys() (map[string]bool, error) {
	keys := map[string]bool{}
	if len(m.ConfigSchema) == 0 {
		return keys, nil
	}
	var s struct {
		Type       *string                    `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(m.ConfigSchema, &s); err != nil {
		return nil, fmt.Errorf("%w: config_schema must be a JSON Schema object: %w", ErrInvalidManifest, err)
	}
	if s.Type != nil && *s.Type != "object" {
		return nil, fmt.Errorf("%w: config_schema type must be \"object\", got %q", ErrInvalidManifest, *s.Type)
	}
	for k := range s.Properties {
		keys[k] = true
	}
	return keys, nil
}
