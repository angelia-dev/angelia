package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
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
	envPrefix        = "ANGELIA_"
)

// Valid roles, matching `angelia serve --roles`.
var validRoles = map[string]bool{"all": true, "api": true, "scheduler": true, "worker": true, "dispatcher": true}

var (
	envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	kindRE    = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
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
	Plugins  map[string]Plugin `yaml:"plugins"`

	// Tokens holds the values read from each source's token_env variable,
	// keyed by kind. It is not part of the file format; use SourceToken.
	Tokens map[string]secret `yaml:"-"`
	// PluginTokens holds the values of plugins.<kind>.credentials.<host>.token_env.
	// It is not part of the file format; use PluginToken.
	PluginTokens map[string]secret `yaml:"-"`
	warnings     []string
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

// HTTP configures outgoing HTTP requests.
type HTTP struct {
	UserAgent string `yaml:"user_agent"`
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
		Instance: Instance{Name: DefaultInstance},
		Listen:   DefaultListen,
		Roles:    []string{"all"},
		Database: Database{DSN: DefaultDSN},
		Log:      Log{Level: DefaultLogLevel, Format: DefaultLogFormat},
		HTTP:     HTTP{UserAgent: "angelia/" + buildinfo.Version()},
		Sources:  map[string]Source{},
		Tokens:   map[string]secret{},
	}
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
	if opts.Path != "" {
		data, err := os.ReadFile(opts.Path)
		if err != nil {
			return nil, fmt.Errorf("read config file: %w", err)
		}
		if err := decode(bytes.NewReader(data), opts.Path, cfg); err != nil {
			return nil, err
		}
	}
	env := environMap(opts.Environ)
	if err := applyEnv(cfg, env); err != nil {
		return nil, err
	}
	cfg.Roles = dedupe(cfg.Roles)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg.resolveTokens(env)
	return cfg, nil
}

func decode(r io.Reader, path string, cfg *Config) error {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
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
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: multiple YAML documents are not supported", path)
	}
	if cfg.Sources == nil {
		cfg.Sources = map[string]Source{}
	}
	return nil
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
	if v, ok := env[envPrefix+"ROLES"]; ok {
		cfg.Roles = nil
		for _, r := range strings.Split(v, ",") {
			if r = strings.TrimSpace(r); r != "" {
				cfg.Roles = append(cfg.Roles, r)
			}
		}
	}

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
	c.validatePlugins(bad)
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
	c.resolvePluginTokens(env)
}

// Warnings lists non-fatal findings, such as a token_env naming an unset variable.
func (c *Config) Warnings() []string { return slices.Clone(c.warnings) }

// SourceToken returns the token for a source kind, read from the environment
// variable named by its token_env. It is empty when none is configured or set.
func (c *Config) SourceToken(kind string) string { return string(c.Tokens[kind]) }
