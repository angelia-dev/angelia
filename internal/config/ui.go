package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/angelia-dev/angelia/internal/secrets"
)

// UI configures the admin web UI (ADR 0033): its sign-in with GitHub, the
// access list that gives each person a role, and the sessions (story #262).
type UI struct {
	// Enabled serves the UI and its sign-in routes on the api role. Off by
	// default.
	Enabled bool `yaml:"enabled"`
	// PublicURL is the URL people open, such as https://angelia.example.com:
	// the OAuth redirect URI is built from it, and the CSRF rule compares a
	// request's Origin with its origin. https, or http on a loopback host
	// for local development only.
	PublicURL string `yaml:"public_url"`
	// GitHub is the GitHub OAuth App people sign in with.
	GitHub UIGitHub `yaml:"github"`
	// Access lists who may sign in, and with which role. It is read at
	// start and evaluated on every request (ADR 0033 §3).
	Access []UIAccess `yaml:"access"`
	// Session bounds the life of a browser session.
	Session UISession `yaml:"session"`
}

// UIGitHub is a GitHub OAuth App (not a GitHub App). There is deliberately no
// client_secret key: the secret is read from the environment variable named
// by ClientSecretEnv or from the file named by ClientSecretFile.
type UIGitHub struct {
	ClientID         string `yaml:"client_id"`
	ClientSecretEnv  string `yaml:"client_secret_env"`
	ClientSecretFile string `yaml:"client_secret_file"`
	// APIURL and WebURL point at a GitHub Enterprise Server instead of
	// GitHub.com (https://HOST/api/v3 and https://HOST). Empty: GitHub.com.
	APIURL string `yaml:"api_url"`
	WebURL string `yaml:"web_url"`

	// clientSecretEnv is the value of ClientSecretEnv's variable, read by
	// Load. It is not part of the file format.
	clientSecretEnv secret
}

// UIAccess is one entry of ui.access: exactly one of GitHubUser (a login,
// matched case-insensitively and pinned to the id seen at the first
// sign-in) and GitHubID (the numeric user id, preferred), and a role.
type UIAccess struct {
	GitHubUser string `yaml:"github_user"`
	GitHubID   int64  `yaml:"github_id"`
	Role       string `yaml:"role"`
}

// UISession bounds a browser session: it ends after IdleTimeout without a
// request, and MaxLifetime after the sign-in whatever its use.
type UISession struct {
	IdleTimeout time.Duration `yaml:"idle_timeout"`
	MaxLifetime time.Duration `yaml:"max_lifetime"`
}

// The roles of ui.access (ADR 0033 §2): viewer has the read scope, editor
// write, admin admin.
const (
	UIRoleViewer = "viewer"
	UIRoleEditor = "editor"
	UIRoleAdmin  = "admin"
)

// Session defaults (ADR 0033 §3).
const (
	DefaultUIIdleTimeout = 12 * time.Hour
	DefaultUIMaxLifetime = 7 * 24 * time.Hour
)

// githubLoginRE is a GitHub login: 1 to 39 letters, digits and single
// hyphens, not starting or ending with a hyphen.
var githubLoginRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)

// maxSecretFile bounds the read of the client secret file.
const maxSecretFile = 4096

// PublicOrigin returns the origin of PublicURL (scheme://host[:port], lower
// case, without a default port), or "" when it is not a valid URL.
func (u UI) PublicOrigin() string {
	p, err := url.Parse(u.PublicURL)
	if err != nil || p.Host == "" {
		return ""
	}
	scheme, host, port := strings.ToLower(p.Scheme), strings.ToLower(p.Hostname()), p.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		return scheme + "://" + net.JoinHostPort(host, port)
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host
}

// LocalDev reports whether PublicURL is a loopback http:// URL, served for
// local development only: the cookies then lose their Secure attribute and
// the __Host- prefix (ADR 0033 §5).
func (u UI) LocalDev() bool {
	p, err := url.Parse(u.PublicURL)
	return err == nil && strings.EqualFold(p.Scheme, "http") && isLoopbackHost(p.Hostname())
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// validate checks the ui section. An inline client secret is refused before
// this runs (inlineClientSecret). The other keys are checked only when the
// UI is enabled, so a disabled section may be left half-written.
func (u UI) validate(bad func(string, ...any)) {
	if !u.Enabled {
		return
	}
	switch p, err := url.Parse(u.PublicURL); {
	case u.PublicURL == "":
		bad("ui.public_url: required when ui.enabled is true (the URL people open, such as https://angelia.example.com)")
	case err != nil || p.Host == "" || (p.Scheme != "https" && p.Scheme != "http"):
		bad("ui.public_url: must be an absolute https URL such as https://angelia.example.com")
	case p.User != nil || p.RawQuery != "" || p.Fragment != "" || (p.Path != "" && p.Path != "/"):
		bad("ui.public_url: must be scheme://host[:port] only, without a path, query, fragment or credentials (the UI is served under /ui/)")
	case p.Scheme == "http" && !isLoopbackHost(p.Hostname()):
		bad("ui.public_url: http:// is accepted for a loopback host only (local development); use https://")
	}
	g := u.GitHub
	switch {
	case g.ClientID == "":
		bad("ui.github.client_id: required when ui.enabled is true (the client id of a GitHub OAuth App)")
	case strings.ContainsAny(g.ClientID, " \t\r\n") || len(g.ClientID) > 255:
		bad("ui.github.client_id: must be one word")
	}
	switch {
	case g.ClientSecretEnv == "" && g.ClientSecretFile == "":
		bad("ui.github: set client_secret_env (an environment variable) or client_secret_file (a file) to the OAuth App's client secret")
	case g.ClientSecretEnv != "" && g.ClientSecretFile != "":
		bad("ui.github: set one of client_secret_env and client_secret_file, not both")
	case g.ClientSecretEnv != "" && !envNameRE.MatchString(g.ClientSecretEnv):
		// Never echoed: a secret pasted here must not reach the log.
		bad("ui.github.client_secret_env: must be an environment variable name, not the secret")
	case strings.ContainsAny(g.ClientSecretFile, "\r\n"):
		bad("ui.github.client_secret_file: must be a single-line path")
	}
	for _, k := range []struct{ key, v string }{{"api_url", g.APIURL}, {"web_url", g.WebURL}} {
		if k.v == "" {
			continue
		}
		if p, err := url.Parse(k.v); err != nil || p.Scheme != "https" || p.Host == "" || p.User != nil || p.RawQuery != "" || p.Fragment != "" {
			bad("ui.github.%s: must be an absolute https URL without credentials, query or fragment", k.key)
		}
	}
	admins := 0
	for i, a := range u.Access {
		switch {
		case a.GitHubUser != "" && a.GitHubID != 0:
			bad("ui.access[%d]: set one of github_user and github_id, not both", i)
		case a.GitHubUser == "" && a.GitHubID == 0:
			bad("ui.access[%d]: set github_user (a login) or github_id (a numeric user id)", i)
		case a.GitHubUser != "" && !githubLoginRE.MatchString(a.GitHubUser):
			bad("ui.access[%d].github_user: not a GitHub login (letters, digits and single hyphens, at most 39)", i)
		case a.GitHubID < 0:
			bad("ui.access[%d].github_id: must be a positive number", i)
		}
		switch a.Role {
		case UIRoleAdmin:
			admins++
		case UIRoleViewer, UIRoleEditor:
		default:
			bad("ui.access[%d].role %q: want viewer, editor or admin", i, a.Role)
		}
	}
	if admins == 0 {
		bad("ui.access: list at least one entry with role admin")
	}
	s := u.Session
	switch {
	case s.IdleTimeout < time.Minute:
		bad("ui.session.idle_timeout: must be at least 1m")
	case s.MaxLifetime < time.Minute:
		bad("ui.session.max_lifetime: must be at least 1m")
	case s.IdleTimeout > s.MaxLifetime:
		bad("ui.session.idle_timeout (%s) is longer than ui.session.max_lifetime (%s)", s.IdleTimeout, s.MaxLifetime)
	}
}

// resolve reads the client secret's environment variable, after Validate.
func (u *UI) resolve(env map[string]string) []string {
	u.GitHub.clientSecretEnv = secret{}
	if !u.Enabled || u.GitHub.ClientSecretEnv == "" {
		return nil
	}
	v := env[u.GitHub.ClientSecretEnv]
	if v == "" {
		return []string{fmt.Sprintf("ui.github.client_secret_env: environment variable %s is not set or empty; the api role will not start", u.GitHub.ClientSecretEnv)}
	}
	u.GitHub.clientSecretEnv = newSecret(v)
	return nil
}

// ClientSecret returns the OAuth App's client secret: the value of
// ClientSecretEnv's variable as Load read it, or the content of
// ClientSecretFile (one line; a trailing newline is dropped). The file must
// be a regular file that group and others cannot write; one they can read is
// accepted with a warning, returned for the caller to log. No error or
// warning holds the secret.
func (g UIGitHub) ClientSecret() (v secrets.Value, warning string, err error) {
	if g.ClientSecretEnv != "" {
		if g.clientSecretEnv.IsZero() {
			return secrets.Value{}, "", fmt.Errorf("ui.github.client_secret_env: environment variable %s is not set or empty", g.ClientSecretEnv)
		}
		return g.clientSecretEnv, "", nil
	}
	if g.ClientSecretFile == "" {
		return secrets.Value{}, "", errors.New("ui.github: no client secret configured")
	}
	return readSecretFile(g.ClientSecretFile)
}

func readSecretFile(path string) (secrets.Value, string, error) {
	fail := func(format string, a ...any) (secrets.Value, string, error) {
		return secrets.Value{}, "", fmt.Errorf("ui.github.client_secret_file %s: "+format, append([]any{path}, a...)...)
	}
	before, err := os.Stat(path)
	if err != nil {
		return fail("%w", err)
	}
	if !before.Mode().IsRegular() {
		return fail("not a regular file")
	}
	f, err := os.Open(path) //nolint:gosec // G304: the operator's secret file.
	if err != nil {
		return fail("%w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return fail("%w", err)
	}
	if !os.SameFile(before, info) {
		return fail("the file changed while it was being opened; retry")
	}
	var warning string
	if runtime.GOOS != "windows" {
		perm := info.Mode().Perm()
		switch {
		case perm&0o022 != 0:
			return fail("is writable by group or others (mode %04o); run: chmod 600 on it", perm)
		case perm&0o044 != 0:
			warning = fmt.Sprintf("ui.github.client_secret_file %s is readable by group or others (mode %04o); it holds the OAuth client secret: chmod 600 it", path, perm)
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSecretFile+1))
	if err != nil {
		return fail("%w", err)
	}
	if len(data) > maxSecretFile {
		return fail("is larger than %d bytes", maxSecretFile)
	}
	data = bytes.TrimSuffix(bytes.TrimSuffix(data, []byte("\n")), []byte("\r"))
	if len(data) == 0 || bytes.ContainsAny(data, "\r\n") {
		return fail("must hold the secret on one line")
	}
	return secrets.NewValue(data), warning, nil
}

// inlineClientSecret returns the line of a ui.github.client_secret key in the
// instance file, or 0. The key is refused with its own message, which never
// echoes the value: a secret belongs in an environment variable or a file.
func inlineClientSecret(data []byte) int {
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil || len(doc.Content) == 0 {
		return 0
	}
	n := mappingValue(doc.Content[0], "ui")
	n = mappingValue(n, "github")
	if n == nil || n.Kind != yaml.MappingNode {
		return 0
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == "client_secret" {
			return n.Content[i].Line
		}
	}
	return 0
}

func mappingValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}
