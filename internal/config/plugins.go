package config

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Plugin is the operator's configuration of an installed plugin, keyed by
// the plugin's kind (ADR 0011, amendment of #145). It holds no token:
// credentials name an environment variable, like sources.<kind>.token_env.
type Plugin struct {
	// AllowedHosts extends the manifest's allowed_hosts, for example with
	// a registry mirror. Hostnames (lower case, no trailing dot) or IP
	// literals; no scheme, port, path or wildcard.
	AllowedHosts []string `yaml:"allowed_hosts"`
	// Credentials, keyed by "host" (port 443) or "host:port", are injected
	// by the host as the Authorization header of https requests to that
	// host and port.
	Credentials map[string]PluginCredential `yaml:"credentials"`
}

// PluginCredential binds a token to one host of a plugin. The token is read
// from the environment variable named by TokenEnv. A secret-store
// reference is a later, additive key.
type PluginCredential struct {
	TokenEnv string `yaml:"token_env"`
	// Scheme, if set, is written before the token ("Bearer").
	Scheme string `yaml:"scheme"`
}

var (
	pluginHostLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	authSchemeRE      = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,31}$`)
)

// validPluginHost reports whether h is in the canonical form of an
// allowed_hosts entry: a lower-case hostname without a trailing dot, or a
// canonical IP literal.
func validPluginHost(h string) bool {
	if a, err := netip.ParseAddr(h); err == nil {
		return a.Zone() == "" && a.String() == h
	}
	if h == "" || len(h) > 253 {
		return false
	}
	for l := range strings.SplitSeq(h, ".") {
		if !pluginHostLabelRE.MatchString(l) {
			return false
		}
	}
	return true
}

// validCredentialKey reports whether k is host or host:port, with host in
// canonical form and port 1-65535 without leading zeros. An IPv6 literal
// with a port is written [addr]:port.
func validCredentialKey(k string) bool {
	if validPluginHost(k) {
		return true
	}
	h, port, err := net.SplitHostPort(k)
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535 && strconv.Itoa(n) == port && validPluginHost(h)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// validatePlugins checks plugins.*; values that may be tokens are never echoed.
func (c *Config) validatePlugins(bad func(string, ...any)) {
	for _, k := range sortedKeys(c.Plugins) {
		p := c.Plugins[k]
		if !kindRE.MatchString(k) {
			bad("plugins: invalid kind name (want lower-case letters, digits and single hyphens)")
			continue
		}
		for i, h := range p.AllowedHosts {
			if !validPluginHost(h) {
				bad("plugins.%s.allowed_hosts[%d]: must be a lower-case hostname or an IP literal (no scheme, port, path or wildcard)", k, i)
			}
		}
		for _, h := range sortedKeys(p.Credentials) {
			cr := p.Credentials[h]
			if !validCredentialKey(h) {
				bad("plugins.%s.credentials: key must be host or host:port, the host a lower-case hostname or an IP literal ([addr]:port for IPv6 with a port)", k)
				continue
			}
			switch {
			case cr.TokenEnv == "":
				bad("plugins.%s.credentials.%s.token_env: required", k, h)
			case hasTokenPrefix(cr.TokenEnv):
				bad("plugins.%s.credentials.%s.token_env: looks like a token, not an environment variable name", k, h)
			case !envNameRE.MatchString(cr.TokenEnv):
				bad("plugins.%s.credentials.%s.token_env: must be an environment variable name, not a token", k, h)
			}
			if cr.Scheme != "" && !authSchemeRE.MatchString(cr.Scheme) {
				bad("plugins.%s.credentials.%s.scheme: must be one word such as Bearer", k, h)
			}
		}
	}
}

// resolvePluginTokens runs after Validate. An empty variable counts as unset.
func (c *Config) resolvePluginTokens(env map[string]string) {
	c.PluginTokens = map[string]secret{}
	for _, k := range sortedKeys(c.Plugins) {
		creds := c.Plugins[k].Credentials
		for _, h := range sortedKeys(creds) {
			name := creds[h].TokenEnv
			if env[name] == "" {
				c.warnings = append(c.warnings, fmt.Sprintf("plugins.%s.credentials.%s.token_env: environment variable %s is not set or empty", k, h, name))
				continue
			}
			c.PluginTokens[k+"\x00"+h] = secret(env[name])
		}
	}
}

// PluginToken returns the token bound to host for the plugin kind, read
// from the environment variable named by its token_env. It is empty when
// none is configured or set. Hand it to the plugin host as a credential,
// never to the plugin.
func (c *Config) PluginToken(kind, host string) string {
	return string(c.PluginTokens[kind+"\x00"+host])
}
