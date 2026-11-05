package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/angelia-dev/angelia/internal/buildinfo"
)

// Defaults for keys that are not set anywhere.
const (
	DefaultInstance  = "angelia"
	DefaultListen    = ":8080"
	DefaultDSN       = "sqlite:///var/lib/angelia/angelia.db"
	DefaultLogLevel  = "info"
	DefaultLogFormat = "json"
	// DefaultDataDir is the data directory when database.dsn is not a
	// sqlite file (whose directory is the data directory otherwise).
	DefaultDataDir = "/var/lib/angelia"
	envPrefix      = "ANGELIA_"
)

// Valid roles, matching `angelia serve --roles`.
var validRoles = map[string]bool{"all": true, "api": true, "scheduler": true, "worker": true, "dispatcher": true}

var (
	envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	kindRE    = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	// hostRE is a lower-case DNS name or IPv4 address (http.allow_http_hosts).
	hostRE    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)
	unknownRE = regexp.MustCompile(`^(line \d+): field (\S+) not found in type .*$`)
	// instanceRE is the <instance> part of the CloudEvents source
	// angelia://<instance>/watches/<watch_id> (spec envelope profile v1).
	instanceRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,62}$`)
	tokenStart = []string{"ghp_", "gho_", "ghs_", "github_pat_", "glpat-", "npm_", "pypi-", "whsec_"}
)

// secret holds a credential. It prints as [REDACTED] with every fmt verb and
// in slog, so a Config can be logged safely. Use string(s) to read it.
type secret string

const redacted = "[REDACTED]"

func (secret) String() string               { return redacted }
func (secret) GoString() string             { return redacted }
func (secret) LogValue() slog.Value         { return slog.StringValue(redacted) }
func (secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
func (secret) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, redacted) }

// Config is the typed instance configuration. Create it with Load or Default.
type Config struct {
	Instance Instance          `yaml:"instance"`
	Listen   string            `yaml:"listen"`
	Roles    []string          `yaml:"roles"`
	Database Database          `yaml:"database"`
	Secrets  Secrets           `yaml:"secrets"`
	Log      Log               `yaml:"log"`
	HTTP     HTTP              `yaml:"http"`
	Sources  map[string]Source `yaml:"sources"`
	API      API               `yaml:"api"`
	// PluginsDir holds one folder per installed plugin,
	// <plugins_dir>/<kind>/ with manifest.json, plugin.wasm and grants.yaml
	// (ADR 0025); read it with LoadPlugins. Load sets it to
	// <data dir>/plugins when it is not configured.
	PluginsDir string `yaml:"plugins_dir"`

	// Tokens holds the values read from each source's token_env variable,
	// keyed by kind. It is not part of the file format; use SourceToken.
	Tokens   map[string]secret `yaml:"-"`
	warnings []string
	// pluginsDirDefault is true when PluginsDir was not configured.
	pluginsDirDefault bool
}

// fileConfig is the instance file: the Config plus the keys that moved
// out of it and are rejected with a pointer to their new place.
type fileConfig struct {
	Config `yaml:",inline"`
	// Plugins is the plugins.<kind> map of #145, moved to
	// <plugins_dir>/<kind>/grants.yaml (ADR 0025).
	Plugins yaml.Node `yaml:"plugins"`
}

// Instance identifies this Angelia instance. Name is the <instance> part of
// the CloudEvents source attribute angelia://<instance>/watches/<watch_id>.
type Instance struct {
	Name string `yaml:"name"`
}

// Database configures the store.
type Database struct {
	DSN string `yaml:"dsn"`
}

// SQLitePath returns the database file of a sqlite DSN (sqlite:///abs/path.db;
// the scheme is case-insensitive). A DSN with a host, such as
// sqlite://./data/a.db, is rejected: the host would be silently dropped.
func (d Database) SQLitePath() (string, error) {
	u, err := url.Parse(d.DSN)
	if err != nil || !strings.EqualFold(u.Scheme, "sqlite") {
		return "", errors.New("database.dsn is not a sqlite DSN")
	}
	if u.Host != "" {
		return "", errors.New("database.dsn: a sqlite DSN has no host; write sqlite:///absolute/path.db (three slashes) for an absolute path")
	}
	if u.Path == "" {
		return "", errors.New("database.dsn: sqlite DSN needs a file path, e.g. sqlite:///var/lib/angelia/angelia.db")
	}
	return u.Path, nil
}

// Secrets configures the key file used for secrets (encryption is Phase 1).
type Secrets struct {
	KeyFile string `yaml:"key_file"`
}

// Log configures logging.
type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// HTTP configures outgoing HTTP requests, of sources and sinks alike.
type HTTP struct {
	UserAgent string `yaml:"user_agent"`
	// AllowNetworks lists CIDR prefixes the SSRF guard accepts although
	// they are private, loopback, link-local or otherwise reserved (ADR 0023
	// section 5). Empty by default. Only the instance configuration sets
	// it: the watch file and the API cannot widen it.
	AllowNetworks []string `yaml:"allow_networks"`
	// AllowHTTPHosts lists host names a webhook may reach over plain
	// http:// (decision D12). Empty by default: https only.
	AllowHTTPHosts []string `yaml:"allow_http_hosts"`
}

// AllowPrefixes returns AllowNetworks parsed, for netguard.Config.Allow.
// Validate has checked every entry; an invalid one is skipped.
func (h HTTP) AllowPrefixes() []netip.Prefix {
	out := make([]netip.Prefix, 0, len(h.AllowNetworks))
	for _, n := range h.AllowNetworks {
		if p, err := netip.ParsePrefix(n); err == nil {
			out = append(out, p.Masked())
		}
	}
	return out
}

// API configures the REST API.
type API struct {
	// AllowedEnv lists the environment variables a *_env key set through the
	// API may name (spec docs/api.md decision 25): exact names, empty by
	// default, so the API cannot name any. Names starting with ANGELIA_ are
	// always refused.
	AllowedEnv []string `yaml:"allowed_env"`
}

// AllowsEnv reports whether a *_env key written through the API may name the
// environment variable name.
func (a API) AllowsEnv(name string) bool {
	return !strings.HasPrefix(name, envPrefix) && slices.Contains(a.AllowedEnv, name)
}

// Source configures one source kind. There is deliberately no token field:
// the token is read from the environment variable named by TokenEnv.
type Source struct {
	BaseURL  string `yaml:"base_url"`
	TokenEnv string `yaml:"token_env"`
}

// Default returns the configuration used when nothing is configured.
func Default() *Config {
	return &Config{
		Instance:   Instance{Name: DefaultInstance},
		Listen:     DefaultListen,
		Roles:      []string{"all"},
		Database:   Database{DSN: DefaultDSN},
		Log:        Log{Level: DefaultLogLevel, Format: DefaultLogFormat},
		HTTP:       HTTP{UserAgent: "angelia/" + buildinfo.Version()},
		Sources:    map[string]Source{},
		PluginsDir: defaultPluginsDir(DefaultDSN),
		Tokens:     map[string]secret{},

		pluginsDirDefault: true,
	}
}

// PluginsDirIsDefault reports whether PluginsDir is the default, not set
// in the file or the environment.
func (c *Config) PluginsDirIsDefault() bool { return c.pluginsDirDefault }

// defaultPluginsDir is <data dir>/plugins: the data directory is the
// directory of a sqlite database file, else DefaultDataDir.
func defaultPluginsDir(dsn string) string {
	if p, err := (Database{DSN: dsn}).SQLitePath(); err == nil {
		return filepath.Join(filepath.Dir(p), "plugins")
	}
	return filepath.Join(DefaultDataDir, "plugins")
}

// Options controls Load.
type Options struct {
	// Path of the YAML file. Empty means no file; a non-empty path must exist.
	Path string
	// Environ is the environment as "KEY=value" pairs, like os.Environ.
	Environ []string
}

// Load builds the configuration from defaults, the optional file and the
// environment, then validates it. Errors name the file and line when known.
func Load(ctx context.Context, opts Options) (*Config, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	cfg := Default()
	cfg.PluginsDir = ""
	var moved *movedKey
	if opts.Path != "" {
		data, err := os.ReadFile(opts.Path)
		if err != nil {
			return nil, fmt.Errorf("read config file: %w", err)
		}
		if moved, err = decode(data, opts.Path, cfg); err != nil {
			return nil, err
		}
	}
	env := environMap(opts.Environ)
	if err := applyEnv(cfg, env); err != nil {
		return nil, err
	}
	cfg.pluginsDirDefault = cfg.PluginsDir == ""
	if cfg.pluginsDirDefault {
		cfg.PluginsDir = defaultPluginsDir(cfg.Database.DSN)
	} else if filepath.IsAbs(cfg.PluginsDir) {
		cfg.PluginsDir = filepath.Clean(cfg.PluginsDir)
	}
	if moved != nil {
		return nil, pluginsMovedError(opts.Path, moved, cfg.PluginsDir)
	}
	cfg.Roles = dedupe(cfg.Roles)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg.resolveTokens(env)
	return cfg, nil
}

// movedKey is a plugins key found in the instance file.
type movedKey struct {
	line  int
	kinds []string
}

// decode reads the instance file into cfg. It returns the plugins key,
// which moved to the plugin folders, so that Load can name the new
// location once plugins_dir is known.
func decode(data []byte, path string, cfg *Config) (*movedKey, error) {
	f := fileConfig{Config: *cfg}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		err = yamlError(path, err)
		// A typo elsewhere must not hide the move of the plugins key.
		if m := movedPlugins(data, &f.Plugins); m != nil {
			dir := f.PluginsDir
			if dir == "" {
				dir = "<plugins_dir>"
			}
			err = errors.Join(err, pluginsMovedError(path, m, dir))
		}
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: multiple YAML documents are not supported", path)
	}
	*cfg = f.Config
	if cfg.Sources == nil {
		cfg.Sources = map[string]Source{}
	}
	return movedPlugins(data, &f.Plugins), nil
}

// movedPlugins describes the plugins key n of the instance file data, or
// returns nil when there is none.
func movedPlugins(data []byte, n *yaml.Node) *movedKey {
	if n.Kind == 0 {
		return nil
	}
	m := &movedKey{line: n.Line}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err == nil && len(root.Content) == 1 && root.Content[0].Kind == yaml.MappingNode {
		top := root.Content[0].Content
		for i := 0; i+1 < len(top); i += 2 {
			if top[i].Value == "plugins" {
				m.line = top[i].Line
			}
		}
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			// Only valid kind names are echoed, never another value.
			if k := n.Content[i].Value; pluginKindRE.MatchString(k) {
				m.kinds = append(m.kinds, k)
			}
		}
	}
	return m
}

// yamlError names the file and line of a decoding error, and reports
// unknown keys without Go type names.
func yamlError(path string, err error) error {
	var te *yaml.TypeError
	if errors.As(err, &te) {
		errs := make([]error, 0, len(te.Errors))
		for _, m := range te.Errors {
			if g := unknownRE.FindStringSubmatch(m); g != nil {
				m = fmt.Sprintf("%s: unknown key %q", g[1], g[2])
			}
			errs = append(errs, fmt.Errorf("%s: %s", path, m))
		}
		return errors.Join(errs...)
	}
	return fmt.Errorf("%s: %w", path, err)
}

// pluginsMovedError refuses the plugins.<kind> keys of #145: they moved to
// <plugins_dir>/<kind>/grants.yaml (ADR 0025).
func pluginsMovedError(path string, m *movedKey, dir string) error {
	where := make([]string, 0, len(m.kinds))
	for _, k := range m.kinds {
		where = append(where, filepath.Join(dir, k, GrantsFile))
	}
	if len(where) == 0 {
		where = []string{filepath.Join(dir, "<kind>", GrantsFile)}
	}
	return fmt.Errorf("%s: line %d: plugins: moved to the plugin's folder: write allowed_hosts and credentials in %s (see docs/configuration.md, ADR 0025)",
		path, m.line, strings.Join(where, ", "))
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	var out []string
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func environMap(environ []string) map[string]string {
	m := make(map[string]string, len(environ))
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

func applyEnv(cfg *Config, env map[string]string) error {
	str := func(name string, dst *string) {
		if v, ok := env[envPrefix+name]; ok {
			*dst = v
		}
	}
	str("INSTANCE_NAME", &cfg.Instance.Name)
	str("LISTEN", &cfg.Listen)
	str("DATABASE_DSN", &cfg.Database.DSN)
	str("SECRETS_KEY_FILE", &cfg.Secrets.KeyFile)
	str("LOG_LEVEL", &cfg.Log.Level)
	str("LOG_FORMAT", &cfg.Log.Format)
	str("HTTP_USER_AGENT", &cfg.HTTP.UserAgent)
	str("PLUGINS_DIR", &cfg.PluginsDir)
	list := func(name string, dst *[]string) {
		if v, ok := env[envPrefix+name]; ok {
			*dst = nil
			for _, r := range strings.Split(v, ",") {
				if r = strings.TrimSpace(r); r != "" {
					*dst = append(*dst, r)
				}
			}
		}
	}
	list("ROLES", &cfg.Roles)
	list("HTTP_ALLOW_NETWORKS", &cfg.HTTP.AllowNetworks)
	list("HTTP_ALLOW_HTTP_HOSTS", &cfg.HTTP.AllowHTTPHosts)

	// ANGELIA_SOURCES_<KIND>_BASE_URL and _TOKEN_ENV; "-" in a kind is "_".
	keys := make([]string, 0)
	for k := range env {
		if strings.HasPrefix(k, envPrefix+"SOURCES_") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		rest := strings.TrimPrefix(k, envPrefix+"SOURCES_")
		var field string
		switch {
		case strings.HasSuffix(rest, "_BASE_URL"):
			field = "base_url"
			rest = strings.TrimSuffix(rest, "_BASE_URL")
		case strings.HasSuffix(rest, "_TOKEN_ENV"):
			field = "token_env"
			rest = strings.TrimSuffix(rest, "_TOKEN_ENV")
		default:
			continue // reserved prefix, but not a config key (e.g. a token_env name)
		}
		if rest == "" {
			return fmt.Errorf("environment variable %s: missing source kind", k)
		}
		kind := strings.ToLower(strings.ReplaceAll(rest, "_", "-"))
		s := cfg.Sources[kind]
		if field == "base_url" {
			s.BaseURL = env[k]
		} else {
			s.TokenEnv = env[k]
		}
		cfg.Sources[kind] = s
	}
	return nil
}

// Validate checks every value and reports all problems at once.
func (c *Config) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if !instanceRE.MatchString(c.Instance.Name) {
		bad("instance.name %q: must match ^[a-z0-9][a-z0-9.-]{0,62}$ (lower-case letters, digits, '.' and '-', 1-63 characters, starting with a letter or digit)", c.Instance.Name)
	}

	if _, port, err := net.SplitHostPort(c.Listen); err != nil {
		bad("listen %q: must be host:port", c.Listen)
	} else if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		bad("listen %q: invalid port", c.Listen)
	}

	if u, err := url.Parse(c.Database.DSN); err != nil || u.Scheme == "" {
		bad("database.dsn: must be a URL such as sqlite:///path or postgres://host/db")
	} else {
		switch strings.ToLower(u.Scheme) {
		case "sqlite":
			if u.Path == "" {
				bad("database.dsn: sqlite DSN needs a file path, e.g. sqlite:///var/lib/angelia/angelia.db")
			}
		case "postgres", "postgresql":
			if u.Host == "" {
				bad("database.dsn: postgres DSN needs a host")
			}
		default:
			bad("database.dsn: unsupported scheme %q (want sqlite or postgres)", u.Scheme)
		}
	}

	if len(c.Roles) == 0 {
		bad("roles: must not be empty")
	}
	if len(c.Roles) > 1 && slices.Contains(c.Roles, "all") {
		bad("roles: \"all\" cannot be combined with other roles")
	}
	for _, r := range c.Roles {
		if !validRoles[r] {
			bad("roles: unknown role %q (want api, scheduler, worker, dispatcher or all)", r)
		}
	}

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		bad("log.level %q: want debug, info, warn or error", c.Log.Level)
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		bad("log.format %q: want json or text", c.Log.Format)
	}

	if strings.TrimSpace(c.HTTP.UserAgent) == "" || strings.ContainsAny(c.HTTP.UserAgent, "\r\n") {
		bad("http.user_agent: must be non-empty and on one line")
	}
	for i, n := range c.HTTP.AllowNetworks {
		if _, err := netip.ParsePrefix(n); err != nil {
			bad("http.allow_networks[%d] %q: must be a CIDR prefix such as 10.0.0.0/8 or fd00::/8", i, n)
		}
	}
	for i, h := range c.HTTP.AllowHTTPHosts {
		if !hostRE.MatchString(h) {
			bad("http.allow_http_hosts[%d] %q: must be a lower-case host name or IPv4 address, without scheme or port", i, h)
		}
	}

	kinds := make([]string, 0, len(c.Sources))
	for k := range c.Sources {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		s := c.Sources[k]
		if !kindRE.MatchString(k) {
			bad("sources: invalid kind name (want lower-case letters, digits and single hyphens)")
			continue
		}
		if s.BaseURL != "" {
			u, err := url.Parse(s.BaseURL)
			switch {
			case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
				bad("sources.%s.base_url: must be an absolute http(s) URL", k)
			case u.User != nil:
				bad("sources.%s.base_url: credentials not allowed in base_url; use token_env", k)
			}
		}
		if s.TokenEnv != "" {
			switch {
			case hasTokenPrefix(s.TokenEnv):
				bad("sources.%s.token_env: looks like a token, not an environment variable name", k)
			case !envNameRE.MatchString(s.TokenEnv):
				bad("sources.%s.token_env: must be an environment variable name, not a token", k)
			}
		}
	}
	if !filepath.IsAbs(c.PluginsDir) {
		bad("plugins_dir %q: must be an absolute path", c.PluginsDir)
	}
	for i, n := range c.API.AllowedEnv {
		switch {
		case !envNameRE.MatchString(n):
			bad("api.allowed_env[%d]: must be an environment variable name", i)
		case strings.HasPrefix(n, envPrefix):
			bad("api.allowed_env[%d]: names starting with %s are always refused", i, envPrefix)
		}
	}
	return errors.Join(errs...)
}

// IsEnvName reports whether s is a valid environment variable name. Other
// files that reference a secret by variable name (the watch file) use it so
// the rule is the same everywhere.
func IsEnvName(s string) bool { return envNameRE.MatchString(s) }

// LooksLikeToken reports whether v starts with a well-known credential prefix.
// Callers must not echo v when it does.
func LooksLikeToken(v string) bool { return hasTokenPrefix(v) }

func hasTokenPrefix(v string) bool {
	for _, p := range tokenStart {
		if strings.HasPrefix(v, p) {
			return true
		}
	}
	return false
}

// resolveTokens runs after Validate, so every token_env here is a plain
// variable name that is safe to print. An empty variable counts as unset.
func (c *Config) resolveTokens(env map[string]string) {
	c.Tokens = map[string]secret{}
	c.warnings = nil
	for i, n := range c.HTTP.AllowNetworks {
		if p, err := netip.ParsePrefix(n); err == nil && p.Bits() == 0 {
			c.warnings = append(c.warnings, fmt.Sprintf("http.allow_networks[%d] %s allows every address: the SSRF guard is off for this address family", i, n))
		}
	}
	kinds := make([]string, 0, len(c.Sources))
	for k := range c.Sources {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		name := c.Sources[k].TokenEnv
		if name == "" {
			continue
		}
		if env[name] == "" {
			c.warnings = append(c.warnings, fmt.Sprintf("sources.%s.token_env: environment variable %s is not set or empty", k, name))
			continue
		}
		c.Tokens[k] = secret(env[name])
	}
}

// Warnings lists non-fatal findings, such as a token_env naming an unset variable.
func (c *Config) Warnings() []string { return slices.Clone(c.warnings) }

// SourceToken returns the token for a source kind, read from the environment
// variable named by its token_env. It is empty when none is configured or set.
func (c *Config) SourceToken(kind string) string { return string(c.Tokens[kind]) }
