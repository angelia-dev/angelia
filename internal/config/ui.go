package config

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"slices"
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
	// GitHub is the GitHub OAuth App people sign in with. It is optional
	// when OIDC lists a provider.
	GitHub UIGitHub `yaml:"github"`
	// OIDC lists the OpenID Connect providers people sign in with (#268):
	// Authentik, Keycloak, GitLab, Google and others.
	OIDC []UIOIDC `yaml:"oidc"`
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

// UIOIDC is an OpenID Connect provider: authorization code flow with PKCE,
// state and nonce, the ID token verified against the issuer's keys. As for
// GitHub, there is deliberately no client_secret key.
type UIOIDC struct {
	// Name names the provider in the sign-in routes
	// (/ui/auth/login/{name}), in ui.access entries, in the principal ids
	// ("<name>:<sub>") and in the metrics: lower-case letters, digits and
	// hyphens, starting with a letter, at most 32. "github" is reserved.
	Name string `yaml:"name"`
	// DisplayName is the label of the sign-in button; empty: Name.
	DisplayName string `yaml:"display_name"`
	// Issuer is the issuer URL, exactly as the provider's discovery
	// document states it: https, or http on a loopback host.
	Issuer           string `yaml:"issuer"`
	ClientID         string `yaml:"client_id"`
	ClientSecretEnv  string `yaml:"client_secret_env"`
	ClientSecretFile string `yaml:"client_secret_file"`
	// Scopes default to DefaultOIDCScopes and must contain openid.
	Scopes []string `yaml:"scopes"`
	// AllowedAlgs are the ID token signature algorithms accepted; empty:
	// OIDCAlgs, every asymmetric algorithm. Symmetric (HS*) and none are
	// refused.
	AllowedAlgs []string `yaml:"allowed_algs"`

	clientSecretEnv secret
}

// DefaultOIDCScopes are the scopes of an OIDC provider without scopes.
var DefaultOIDCScopes = []string{"openid", "email", "profile"}

// OIDCAlgs are the ID token signature algorithms an OIDC provider may
// accept: asymmetric only, so that no key shared with the client (the
// client secret, in HS256) can sign an ID token.
func OIDCAlgs() []string {
	return []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA"}
}

// EffectiveScopes returns Scopes, or DefaultOIDCScopes when empty.
func (o UIOIDC) EffectiveScopes() []string {
	if len(o.Scopes) == 0 {
		return append([]string(nil), DefaultOIDCScopes...)
	}
	return append([]string(nil), o.Scopes...)
}

// EffectiveAlgs returns AllowedAlgs, or OIDCAlgs when empty.
func (o UIOIDC) EffectiveAlgs() []string {
	if len(o.AllowedAlgs) == 0 {
		return OIDCAlgs()
	}
	return append([]string(nil), o.AllowedAlgs...)
}

// Label returns DisplayName, or Name when empty.
func (o UIOIDC) Label() string {
	if o.DisplayName == "" {
		return o.Name
	}
	return o.DisplayName
}

// UIAccess is one entry of ui.access and a role. A GitHub entry sets
// exactly one of GitHubUser (a login, matched case-insensitively and pinned
// to the id seen at the first sign-in) and GitHubID (the numeric user id,
// preferred). An OIDC entry sets Provider, the name of a ui.oidc provider,
// and exactly one of Subject (the ID token's sub, preferred) and Email (a
// verified email address, matched case-insensitively and pinned to the
// subject seen at the first sign-in).
type UIAccess struct {
	GitHubUser string `yaml:"github_user"`
	GitHubID   int64  `yaml:"github_id"`
	Provider   string `yaml:"provider"`
	Subject    string `yaml:"subject"`
	Email      string `yaml:"email"`
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
	// MinUIIdleTimeout is the shortest idle timeout: well above the up to
	// 6 minutes (the last-seen write interval plus the flush tick) that a
	// use on another replica takes to reach the store.
	MinUIIdleTimeout     = 15 * time.Minute
	DefaultUIMaxLifetime = 7 * 24 * time.Hour
)

// Configured reports whether any key of ui.github is set: GitHub sign-in is
// on. With no key set, ui.oidc must list a provider.
func (g UIGitHub) Configured() bool {
	return g.ClientID != "" || g.ClientSecretEnv != "" || g.ClientSecretFile != "" || g.APIURL != "" || g.WebURL != ""
}

// oidcNameRE is a provider name: the names of the sign-in routes.
var oidcNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// oidcSubjectRE is an OIDC subject that fits a principal id: printable
// ASCII without space, at most 255 (the API's PrincipalId).
var oidcSubjectRE = regexp.MustCompile(`^[!-~]{1,255}$`)

// scopeTokenRE is an OAuth scope token (RFC 6749 §3.3).
var scopeTokenRE = regexp.MustCompile(`^[!#-\[\]-~]{1,128}$`)

// emailRE is a plausible email address: one @, no space or control
// character, at most 254.
var emailRE = regexp.MustCompile(`^[^@\s\x00-\x1f\x7f]{1,64}@[^@\s\x00-\x1f\x7f]{1,189}$`)

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
	if !u.GitHub.Configured() && len(u.OIDC) == 0 {
		bad("ui: configure ui.github or at least one ui.oidc provider for the sign-in")
	}
	if u.GitHub.Configured() {
		u.GitHub.validate(bad)
	}
	oidcNames := map[string]bool{}
	for i, o := range u.OIDC {
		o.validate(i, bad)
		if oidcNames[o.Name] {
			bad("ui.oidc[%d].name %q: listed twice", i, o.Name)
		}
		oidcNames[o.Name] = true
	}
	admins := 0
	for i, a := range u.Access {
		a.validate(i, u.GitHub.Configured(), oidcNames, bad)
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
	case s.IdleTimeout < MinUIIdleTimeout:
		bad("ui.session.idle_timeout: must be at least 15m (a use on another replica reaches the store after up to 6m)")
	case s.MaxLifetime < time.Minute:
		bad("ui.session.max_lifetime: must be at least 1m")
	case s.IdleTimeout > s.MaxLifetime:
		bad("ui.session.idle_timeout (%s) is longer than ui.session.max_lifetime (%s)", s.IdleTimeout, s.MaxLifetime)
	}
}

func (g UIGitHub) validate(bad func(string, ...any)) {
	switch {
	case g.ClientID == "":
		bad("ui.github.client_id: required when ui.enabled is true (the client id of a GitHub OAuth App)")
	case strings.ContainsAny(g.ClientID, " \t\r\n") || len(g.ClientID) > 255:
		bad("ui.github.client_id: must be one word")
	}
	validateSecretKeys("ui.github", "the OAuth App's client secret", g.ClientSecretEnv, g.ClientSecretFile, bad)
	for _, k := range []struct{ key, v string }{{"api_url", g.APIURL}, {"web_url", g.WebURL}} {
		if k.v == "" {
			continue
		}
		if p, err := url.Parse(k.v); err != nil || p.Scheme != "https" || p.Host == "" || p.User != nil || p.RawQuery != "" || p.Fragment != "" {
			bad("ui.github.%s: must be an absolute https URL without credentials, query or fragment", k.key)
		}
	}
}

// validateSecretKeys checks the client_secret_env and client_secret_file
// keys of a provider. A value is never echoed: a secret pasted in the wrong
// key must not reach the log.
func validateSecretKeys(key, what, env, file string, bad func(string, ...any)) {
	switch {
	case env == "" && file == "":
		bad("%s: set client_secret_env (an environment variable) or client_secret_file (a file) to %s", key, what)
	case env != "" && file != "":
		bad("%s: set one of client_secret_env and client_secret_file, not both", key)
	case env != "" && !envNameRE.MatchString(env):
		bad("%s.client_secret_env: must be an environment variable name, not the secret", key)
	case strings.ContainsAny(file, "\r\n"):
		bad("%s.client_secret_file: must be a single-line path", key)
	}
}

// key names the provider in messages: ui.oidc[i].
func (o UIOIDC) key(i int) string { return fmt.Sprintf("ui.oidc[%d]", i) }

func (o UIOIDC) validate(i int, bad func(string, ...any)) {
	k := o.key(i)
	switch {
	case o.Name == "":
		bad("%s.name: required (a slug such as authentik, used in /ui/auth/login/{name} and in ui.access)", k)
	case !oidcNameRE.MatchString(o.Name):
		bad("%s.name %q: must be lower-case letters, digits and hyphens, starting with a letter, at most 32", k, o.Name)
	case o.Name == "github":
		bad("%s.name: github is reserved for ui.github", k)
	}
	if len(o.DisplayName) > 64 || strings.ContainsFunc(o.DisplayName, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		bad("%s.display_name: at most 64 characters, without control characters", k)
	}
	switch p, err := url.Parse(o.Issuer); {
	case o.Issuer == "":
		bad("%s.issuer: required (the issuer URL of the provider's discovery document)", k)
	case err != nil || p.Host == "" || (p.Scheme != "https" && p.Scheme != "http"):
		bad("%s.issuer: must be an absolute https URL", k)
	case p.User != nil || p.RawQuery != "" || p.Fragment != "":
		bad("%s.issuer: must not hold credentials, a query or a fragment", k)
	case p.Scheme == "http" && !isLoopbackHost(p.Hostname()):
		bad("%s.issuer: http:// is accepted for a loopback host only (local development); use https://", k)
	}
	switch {
	case o.ClientID == "":
		bad("%s.client_id: required (the client id registered with the provider)", k)
	case strings.ContainsAny(o.ClientID, " \t\r\n") || len(o.ClientID) > 255:
		bad("%s.client_id: must be one word", k)
	}
	validateSecretKeys(k, "the client secret", o.ClientSecretEnv, o.ClientSecretFile, bad)
	if len(o.Scopes) > 0 {
		openid := false
		for _, sc := range o.Scopes {
			if !scopeTokenRE.MatchString(sc) {
				bad("%s.scopes: %q is not a scope token", k, sc)
			}
			openid = openid || sc == "openid"
		}
		if !openid {
			bad("%s.scopes: must contain openid", k)
		}
	}
	for _, a := range o.AllowedAlgs {
		if !slices.Contains(OIDCAlgs(), a) {
			bad("%s.allowed_algs: %q is not accepted; want asymmetric algorithms among %s (none and HS* are refused)", k, a, strings.Join(OIDCAlgs(), ", "))
		}
	}
}

func (a UIAccess) validate(i int, github bool, oidc map[string]bool, bad func(string, ...any)) {
	gh := a.GitHubUser != "" || a.GitHubID != 0
	switch {
	case a.Provider != "" && gh:
		bad("ui.access[%d]: an entry is either a GitHub entry (github_user or github_id) or an OIDC entry (provider), not both", i)
	case a.Provider != "":
		switch {
		case a.Provider == "github":
			bad("ui.access[%d].provider: GitHub entries use github_user or github_id", i)
		case !oidc[a.Provider]:
			bad("ui.access[%d].provider %q: no ui.oidc provider has this name", i, a.Provider)
		}
		switch {
		case a.Subject != "" && a.Email != "":
			bad("ui.access[%d]: set one of subject and email, not both", i)
		case a.Subject == "" && a.Email == "":
			bad("ui.access[%d]: set subject (the ID token's sub) or email (a verified email address)", i)
		case a.Subject != "" && !oidcSubjectRE.MatchString(a.Subject):
			bad("ui.access[%d].subject: must be printable ASCII without spaces, at most 255", i)
		case a.Email != "" && !emailRE.MatchString(a.Email):
			bad("ui.access[%d].email: not an email address", i)
		}
	case a.Subject != "" || a.Email != "":
		bad("ui.access[%d]: subject and email need provider, the name of a ui.oidc provider", i)
	case a.GitHubUser != "" && a.GitHubID != 0:
		bad("ui.access[%d]: set one of github_user and github_id, not both", i)
	case !gh:
		bad("ui.access[%d]: set github_user (a login) or github_id (a numeric user id), or provider with subject or email", i)
	case !github:
		bad("ui.access[%d]: a GitHub entry needs ui.github", i)
	case a.GitHubUser != "" && !githubLoginRE.MatchString(a.GitHubUser):
		bad("ui.access[%d].github_user: not a GitHub login (letters, digits and single hyphens, at most 39)", i)
	case a.GitHubID < 0:
		bad("ui.access[%d].github_id: must be a positive number", i)
	}
}

// resolve reads the client secret's environment variable, after Validate.
func (u *UI) resolve(env map[string]string) []string {
	read := func(key, name string, dst *secret) []string {
		*dst = secret{}
		if !u.Enabled || name == "" {
			return nil
		}
		v := env[name]
		if v == "" {
			return []string{fmt.Sprintf("%s.client_secret_env: environment variable %s is not set or empty; the api role will not start", key, name)}
		}
		*dst = newSecret(v)
		return nil
	}
	warnings := read("ui.github", u.GitHub.ClientSecretEnv, &u.GitHub.clientSecretEnv)
	for i := range u.OIDC {
		o := &u.OIDC[i]
		warnings = append(warnings, read(o.key(i), o.ClientSecretEnv, &o.clientSecretEnv)...)
	}
	return warnings
}

// ClientSecret returns the OAuth App's client secret: the value of
// ClientSecretEnv's variable as Load read it, or the content of
// ClientSecretFile (one line; a trailing newline is dropped). The file must
// be a regular file that group and others cannot write; one they can read is
// accepted with a warning, returned for the caller to log. No error or
// warning holds the secret.
func (g UIGitHub) ClientSecret() (v secrets.Value, warning string, err error) {
	return clientSecret("ui.github", g.ClientSecretEnv, g.clientSecretEnv, g.ClientSecretFile)
}

// ClientSecret returns the provider's client secret, as
// [UIGitHub.ClientSecret] does. key names the provider in errors
// (ui.oidc[<name>]).
func (o UIOIDC) ClientSecret() (v secrets.Value, warning string, err error) {
	return clientSecret("ui.oidc["+o.Name+"]", o.ClientSecretEnv, o.clientSecretEnv, o.ClientSecretFile)
}

func clientSecret(key, env string, envValue secret, file string) (secrets.Value, string, error) {
	if env != "" {
		if envValue.IsZero() {
			return secrets.Value{}, "", fmt.Errorf("%s.client_secret_env: environment variable %s is not set or empty", key, env)
		}
		return envValue, "", nil
	}
	if file == "" {
		return secrets.Value{}, "", fmt.Errorf("%s: no client secret configured", key)
	}
	return readSecretFile(key, file)
}

func readSecretFile(key, path string) (secrets.Value, string, error) {
	fail := func(format string, a ...any) (secrets.Value, string, error) {
		return secrets.Value{}, "", fmt.Errorf("%s.client_secret_file %s: "+format, append([]any{key, path}, a...)...)
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
			warning = fmt.Sprintf("%s.client_secret_file %s is readable by group or others (mode %04o); it holds the OAuth client secret: chmod 600 it", key, path, perm)
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

// inlineClientSecret returns the line and the key of a client_secret key of
// ui.github or of an entry of ui.oidc in the instance file, or 0. The key is
// refused with its own message, which never echoes the value: a secret
// belongs in an environment variable or a file.
func inlineClientSecret(data []byte) (int, string) {
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil || len(doc.Content) == 0 {
		return 0, ""
	}
	ui := mappingValue(doc.Content[0], "ui")
	if line := secretKeyLine(mappingValue(ui, "github")); line != 0 {
		return line, "ui.github"
	}
	if list := mappingValue(ui, "oidc"); list != nil && list.Kind == yaml.SequenceNode {
		for i, n := range list.Content {
			if line := secretKeyLine(n); line != 0 {
				return line, fmt.Sprintf("ui.oidc[%d]", i)
			}
		}
	}
	return 0, ""
}

func secretKeyLine(n *yaml.Node) int {
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
