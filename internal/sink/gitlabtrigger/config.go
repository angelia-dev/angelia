package gitlabtrigger

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/render"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/sink"
)

// Kind is the registry key of this sink.
const Kind = "gitlab-pipeline-trigger"

// Defaults and bounds of the configuration (spec
// GitlabPipelineTriggerSinkConfig).
const (
	DefaultBaseURL = "https://gitlab.com"
	DefaultTimeout = 10 * time.Second
	MinTimeout     = time.Second
	MaxTimeout     = 2 * time.Minute

	// MaxEntries bounds variables and inputs, each.
	MaxEntries = 50
	// MaxTemplate bounds a variable or input template, in bytes.
	MaxTemplate = 4096
	// MaxRefLen bounds ref and the names of variables and inputs.
	MaxRefLen = 255

	apiSuffix = "/api/v4"
)

// Pointers of the token references in sink.Request.Secrets: the JSON Pointer
// of token_env or token_ref in the configuration.
const (
	TokenPointer    = "/token_env"
	TokenRefPointer = "/token_ref"
)

var (
	// segmentRe is one path segment of a project path, as the gitlab-release
	// source reads it.
	segmentRe  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,254}$`)
	idRe       = regexp.MustCompile(`^[0-9]{1,18}$`)
	variableRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,255}$`)
	inputRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
)

// Config is the configuration of a gitlab-pipeline-trigger sink (spec
// GitlabPipelineTriggerSinkConfig, docs/gitlab-pipeline-trigger.md). It holds
// the name of the variable or of the stored secret with the trigger token,
// never the token.
type Config struct {
	Project   string            `json:"project"`
	Ref       string            `json:"ref"`
	BaseURL   string            `json:"base_url,omitempty"`
	TokenEnv  string            `json:"token_env,omitempty"`
	TokenRef  string            `json:"token_ref,omitempty"`
	Variables map[string]string `json:"variables,omitempty"`
	Inputs    map[string]string `json:"inputs,omitempty"`
	Timeout   string            `json:"timeout,omitempty"`
}

// CheckProject checks the project: a full path such as "group/sub/name" (2 to
// 20 segments) or a numeric id, the rules of the gitlab-release source. It
// becomes one URL-encoded path segment.
func CheckProject(s string) error {
	if idRe.MatchString(s) {
		return nil
	}
	segs := strings.Split(s, "/")
	if len(segs) < 2 || len(segs) > 20 || len(s) > 1024 {
		return errors.New("must be a path like group/project (subgroups allowed) or a numeric id")
	}
	for _, seg := range segs {
		if !segmentRe.MatchString(seg) || seg == ".." {
			return errors.New("must be a path like group/project (subgroups allowed) or a numeric id")
		}
	}
	return nil
}

// CheckRef checks the ref: a branch or tag, a literal of 1 to MaxRefLen
// characters with no control character.
func CheckRef(s string) error {
	if s == "" || len(s) > MaxRefLen || strings.IndexFunc(s, unicode.IsControl) >= 0 || !utf8.ValidString(s) {
		return fmt.Errorf("must be a branch or tag name of 1 to %d characters", MaxRefLen)
	}
	return nil
}

// ParseBaseURL checks base_url and returns it without a trailing "/" and
// "/api/v4": an absolute http(s) URL with no userinfo, query or fragment and
// no path segment that looks like a credential. http is accepted here; Deliver
// allows it only for a host in http.allow_http_hosts. The message never echoes
// the URL.
func ParseBaseURL(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	switch {
	case err != nil || u.Host == "" || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http"):
		return nil, errors.New("must be an absolute https:// URL")
	case u.User != nil:
		return nil, errors.New("credentials in the URL are not allowed")
	case u.RawQuery != "" || u.ForceQuery || strings.Contains(s, "?"):
		return nil, errors.New("the URL must not have a query")
	case u.Fragment != "" || u.RawFragment != "" || strings.Contains(s, "#"):
		return nil, errors.New("the URL must not have a fragment")
	case netguard.NonCanonicalIP(u.Hostname()):
		return nil, errors.New("the host is an IP address in a non-canonical form; write it as a.b.c.d")
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if config.LooksLikeToken(seg) {
			return nil, errors.New("a path segment looks like a credential; the token goes in token_env or token_ref")
		}
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if n := len(u.Path) - len(apiSuffix); n >= 0 && strings.EqualFold(u.Path[n:], apiSuffix) {
		u.Path = strings.TrimRight(u.Path[:n], "/")
	}
	u.RawPath = ""
	return u, nil
}

// CheckBaseURL is ParseBaseURL without the result.
func CheckBaseURL(s string) error {
	_, err := ParseBaseURL(s)
	return err
}

// CheckVariableName checks the name of a pipeline variable: GitLab's key rule
// and not ending in _env or _ref (see checkSuffix).
func CheckVariableName(k string) error {
	if !variableRe.MatchString(k) {
		return errors.New("variable names are 1 to 255 letters, digits and underscores")
	}
	return checkSuffix("a variable")(k)
}

// CheckInputName checks the name of a pipeline input.
func CheckInputName(k string) error {
	if !inputRe.MatchString(k) {
		return errors.New("input names are 1 to 255 letters, digits, underscores and hyphens")
	}
	return checkSuffix("an input")(k)
}

// checkSuffix refuses a name ending in _env or _ref: the core reads every
// such key of a sink configuration, at any depth, as the name of an
// environment variable or of a stored secret (ADR 0023 section 4, ADR 0024
// section 6).
func checkSuffix(what string) func(string) error {
	return func(k string) error {
		if strings.HasSuffix(k, "_env") || strings.HasSuffix(k, "_ref") {
			return fmt.Errorf("%s name must not end in _env or _ref: the core would read the value as the name of a variable or of a stored secret", what)
		}
		return nil
	}
}

// CheckTemplate compiles a variable or input template, the way a
// subscription template is checked at load: the error gives a position and a
// reason, never the template text.
func CheckTemplate(src string, limit int) error {
	if len(src) > limit {
		return fmt.Errorf("at most %d bytes", limit)
	}
	if !utf8.ValidString(src) {
		return errors.New("must be valid UTF-8")
	}
	if src == "" {
		return nil
	}
	t, err := render.Compile(src, render.Limits{})
	if err != nil {
		return fmt.Errorf("does not parse as a Go template: %w", err)
	}
	return t.Check()
}

// parsed is a validated Config with its defaults applied.
type parsed struct {
	Config
	base    *url.URL
	timeout time.Duration
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("gitlab-pipeline-trigger: invalid configuration: "+format, a...)
}

// parseConfig validates raw. deep also compiles the templates and renders
// them on the sample events (CheckTemplate), as Validate does when a sink is
// stored; Deliver does not repeat that: it compiles the templates it renders.
func parseConfig(raw sink.Config, deep bool) (parsed, error) {
	var p parsed
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p.Config); err != nil {
		return p, invalid("want an object with project, ref, token_env or token_ref, and optional base_url, variables, inputs and timeout")
	}
	if err := CheckProject(p.Project); err != nil {
		return p, invalid("project %v", err)
	}
	if err := CheckRef(p.Ref); err != nil {
		return p, invalid("ref %v", err)
	}
	if p.BaseURL == "" {
		p.BaseURL = DefaultBaseURL
	}
	var err error
	if p.base, err = ParseBaseURL(p.BaseURL); err != nil {
		return p, invalid("base_url %v", err)
	}
	switch {
	case p.TokenEnv != "" && p.TokenRef != "", p.TokenEnv == "" && p.TokenRef == "":
		return p, invalid("set exactly one of token_env and token_ref")
	case p.TokenEnv != "" && (!config.IsEnvName(p.TokenEnv) || config.LooksLikeToken(p.TokenEnv)):
		return p, invalid("token_env must be an environment variable name")
	case p.TokenRef != "" && !secrets.ValidName(p.TokenRef):
		return p, invalid("token_ref must be the name of a stored secret")
	}
	p.timeout = DefaultTimeout
	if p.Timeout != "" {
		d, err := time.ParseDuration(p.Timeout)
		if err != nil || d < MinTimeout || d > MaxTimeout {
			return p, invalid("timeout must be a duration from %v to %v", MinTimeout, MaxTimeout)
		}
		p.timeout = d
	}
	for _, m := range []struct {
		what  string
		vals  map[string]string
		check func(string) error
	}{{"variables", p.Variables, CheckVariableName}, {"inputs", p.Inputs, CheckInputName}} {
		if len(m.vals) > MaxEntries {
			return p, invalid("at most %d %s", MaxEntries, m.what)
		}
		for k, v := range m.vals {
			if err := m.check(k); err != nil {
				return p, invalid("%s: %v", m.what, err)
			}
			if err := checkValue(v, deep); err != nil {
				return p, invalid("a value of %s %v", m.what, err)
			}
		}
	}
	return p, nil
}

func checkValue(src string, deep bool) error {
	if deep {
		return CheckTemplate(src, MaxTemplate)
	}
	if len(src) > MaxTemplate {
		return fmt.Errorf("at most %d bytes", MaxTemplate)
	}
	return nil
}
