package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/angelia-dev/angelia/internal/buildinfo"
)

// Defaults for keys that are not set anywhere.
const (
	DefaultListen    = ":8080"
	DefaultDSN       = "sqlite:///var/lib/angelia/angelia.db"
	DefaultLogLevel  = "info"
	DefaultLogFormat = "json"
	envPrefix        = "ANGELIA_"
)

// Valid roles, matching `angelia serve --roles`.
var validRoles = map[string]bool{"all": true, "api": true, "scheduler": true, "worker": true, "dispatcher": true}

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Config is the typed instance configuration. Create it with Load or Default.
type Config struct {
	Listen   string            `yaml:"listen"`
	Roles    []string          `yaml:"roles"`
	Database Database          `yaml:"database"`
	Secrets  Secrets           `yaml:"secrets"`
	Log      Log               `yaml:"log"`
	HTTP     HTTP              `yaml:"http"`
	Sources  map[string]Source `yaml:"sources"`

	tokens map[string]string // resolved from token_env at load time
}

// Database configures the store.
type Database struct {
	DSN string `yaml:"dsn"`
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
		Listen:   DefaultListen,
		Roles:    []string{"all"},
		Database: Database{DSN: DefaultDSN},
		Log:      Log{Level: DefaultLogLevel, Format: DefaultLogFormat},
		HTTP:     HTTP{UserAgent: "angelia/" + buildinfo.Version()},
		Sources:  map[string]Source{},
		tokens:   map[string]string{},
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
				errs = append(errs, fmt.Errorf("%s: %s", path, m))
			}
			return errors.Join(errs...)
		}
		return fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Sources == nil {
		cfg.Sources = map[string]Source{}
	}
	return nil
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
			return fmt.Errorf("environment variable %s: unknown sources key (want _BASE_URL or _TOKEN_ENV)", k)
		}
		if rest == "" {
			return fmt.Errorf("environment variable %s: missing source kind", k)
		}
		kind := sourceKind(cfg, rest)
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

// sourceKind maps an env spelling (GITHUB_RELEASE) to the configured kind,
// or to the lower-case hyphenated form for a new one.
func sourceKind(cfg *Config, envKind string) string {
	for k := range cfg.Sources {
		if strings.ToUpper(strings.ReplaceAll(k, "-", "_")) == envKind {
			return k
		}
	}
	return strings.ToLower(strings.ReplaceAll(envKind, "_", "-"))
}

// Validate checks every value and reports all problems at once.
func (c *Config) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if _, port, err := net.SplitHostPort(c.Listen); err != nil {
		bad("listen %q: must be host:port", c.Listen)
	} else if p, err := strconv.Atoi(port); err != nil || p < 0 || p > 65535 {
		bad("listen %q: invalid port", c.Listen)
	}

	if u, err := url.Parse(c.Database.DSN); err != nil || u.Scheme == "" {
		bad("database.dsn: must be a URL such as sqlite:///path or postgres://host/db")
	} else if s := strings.ToLower(u.Scheme); s != "sqlite" && s != "postgres" && s != "postgresql" {
		bad("database.dsn: unsupported scheme %q (want sqlite or postgres)", u.Scheme)
	}

	if len(c.Roles) == 0 {
		bad("roles: must not be empty")
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
		if s.BaseURL != "" {
			if u, err := url.Parse(s.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				bad("sources.%s.base_url %q: must be an absolute http(s) URL", k, s.BaseURL)
			}
		}
		if s.TokenEnv != "" && !envNameRE.MatchString(s.TokenEnv) {
			bad("sources.%s.token_env %q: must be an environment variable name, not a token", k, s.TokenEnv)
		}
	}
	return errors.Join(errs...)
}

func (c *Config) resolveTokens(env map[string]string) {
	c.tokens = map[string]string{}
	for k, s := range c.Sources {
		if s.TokenEnv != "" {
			c.tokens[k] = env[s.TokenEnv]
		}
	}
}

// SourceToken returns the token for a source kind, read from the environment
// variable named by its token_env. It is empty when none is configured or set.
func (c *Config) SourceToken(kind string) string { return c.tokens[kind] }
