package githubdispatch

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
const Kind = "github-dispatch"

// The two GitHub APIs the sink can call (spec GithubDispatchSinkConfig.mode).
const (
	ModeRepositoryDispatch = "repository_dispatch"
	ModeWorkflowDispatch   = "workflow_dispatch"
)

// Defaults and bounds of the configuration (spec GithubDispatchSinkConfig).
const (
	DefaultAPIBaseURL = "https://api.github.com"
	DefaultEventType  = "angelia.release.published"
	DefaultTimeout    = 10 * time.Second
	MinTimeout        = time.Second
	MaxTimeout        = 2 * time.Minute

	// MaxEventTypeTemplate and MaxInputTemplate bound the templates, in bytes.
	MaxEventTypeTemplate = 1024
	MaxInputTemplate     = 4096
	// MaxInputs is the number of workflow inputs GitHub accepts.
	MaxInputs = 25
	// MaxEventTypeChars is GitHub's limit on a rendered event_type.
	MaxEventTypeChars = 100
	// MaxFieldLen bounds workflow and ref (spec maxLength).
	MaxFieldLen = 255
)

// Pointers of the token references in sink.Request.Secrets: the JSON Pointer
// of token_env or token_ref in the configuration.
const (
	TokenPointer    = "/token_env"
	TokenRefPointer = "/token_ref"
)

var (
	repoRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)
	workflowID = regexp.MustCompile(`^[0-9]+$`)
)

// Config is the configuration of a github-dispatch sink (spec
// GithubDispatchSinkConfig, docs/github-dispatch.md). It holds the name of
// the variable or of the stored secret with the token, never the token.
type Config struct {
	Mode       string            `json:"mode,omitempty"`
	Repo       string            `json:"repo"`
	APIBaseURL string            `json:"api_base_url,omitempty"`
	TokenEnv   string            `json:"token_env,omitempty"`
	TokenRef   string            `json:"token_ref,omitempty"`
	EventType  string            `json:"event_type,omitempty"`
	Workflow   string            `json:"workflow,omitempty"`
	Ref        string            `json:"ref,omitempty"`
	Inputs     map[string]string `json:"inputs,omitempty"`
	Timeout    string            `json:"timeout,omitempty"`
}

// ValidRepo reports whether s is an owner/name repository. The name may not
// be "." or "..", which would climb out of the repository's API path.
func ValidRepo(s string) bool {
	if !repoRE.MatchString(s) {
		return false
	}
	_, name, _ := strings.Cut(s, "/")
	return name != "." && name != ".."
}

// CheckAPIBaseURL checks api_base_url: an absolute https URL with no
// userinfo, query or fragment. The token is sent there and nowhere else.
// The message never echoes the URL.
func CheckAPIBaseURL(s string) error {
	u, err := url.Parse(s)
	switch {
	case err != nil || u.Host == "" || u.Hostname() == "":
		return errors.New("must be an absolute https:// URL")
	case u.Scheme != "https":
		return errors.New("must be an https:// URL: the token is sent there")
	case u.User != nil:
		return errors.New("credentials in the URL are not allowed")
	case u.RawQuery != "" || u.ForceQuery || strings.Contains(s, "?"):
		return errors.New("the URL must not have a query")
	case u.Fragment != "" || u.RawFragment != "" || strings.Contains(s, "#"):
		return errors.New("the URL must not have a fragment")
	case netguard.NonCanonicalIP(u.Hostname()):
		return errors.New("the host is an IP address in a non-canonical form; write it as a.b.c.d")
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if config.LooksLikeToken(seg) {
			return errors.New("a path segment looks like a credential; the token goes in token_env or token_ref")
		}
	}
	return nil
}

// CheckWorkflow checks the workflow of a workflow_dispatch sink: a numeric
// id or a file name such as release.yml. It becomes one path segment.
func CheckWorkflow(s string) error {
	switch {
	case s == "" || len(s) > MaxFieldLen:
		return fmt.Errorf("must be 1 to %d characters", MaxFieldLen)
	case workflowID.MatchString(s):
		return nil
	case s == "." || s == ".." || strings.ContainsAny(s, "/\\?#%") || strings.IndexFunc(s, unicode.IsControl) >= 0:
		return errors.New("must be a workflow file name such as release.yml, or its numeric id")
	}
	return nil
}

// CheckRef checks the ref of a workflow_dispatch sink: a branch or tag.
func CheckRef(s string) error {
	if s == "" || len(s) > MaxFieldLen || strings.IndexFunc(s, unicode.IsControl) >= 0 || !utf8.ValidString(s) {
		return fmt.Errorf("must be a branch or tag name of 1 to %d characters", MaxFieldLen)
	}
	return nil
}

// CheckInputName checks the name of a workflow input. A name ending in
// _env or _ref is refused: the core reads every such key of a sink
// configuration, at any depth, as the name of an environment variable or of
// a stored secret (ADR 0023 section 4, ADR 0024 section 6).
func CheckInputName(k string) error {
	switch {
	case k == "" || len(k) > MaxFieldLen || strings.IndexFunc(k, unicode.IsControl) >= 0 || !utf8.ValidString(k):
		return fmt.Errorf("input names are 1 to %d characters", MaxFieldLen)
	case strings.HasSuffix(k, "_env") || strings.HasSuffix(k, "_ref"):
		return errors.New("an input name must not end in _env or _ref: the core would read the value as the name of a variable or of a stored secret")
	}
	return nil
}

// CheckTemplate compiles an event_type or input template, the way a
// subscription template is checked at load: the error gives a position and a
// reason, never the template text. A template with no action is a literal;
// its length is the caller's concern.
func CheckTemplate(src string, limit int) error {
	if len(src) > limit {
		return fmt.Errorf("at most %d bytes", limit)
	}
	if !utf8.ValidString(src) {
		return errors.New("must be valid UTF-8")
	}
	if src == "" {
		return nil // an empty input value; event_type is checked on its own
	}
	t, err := render.Compile(src, render.Limits{})
	if err != nil {
		return fmt.Errorf("does not parse as a Go template: %w", err)
	}
	return t.Check()
}

// IsLiteral reports whether a template has no action, so that its output is
// itself.
func IsLiteral(src string) bool { return !strings.Contains(src, "{{") }

// CheckEventType checks a rendered event_type: 1 to MaxEventTypeChars
// characters, not blank.
func CheckEventType(s string) error {
	n := utf8.RuneCountInString(s)
	if n < 1 || n > MaxEventTypeChars || strings.TrimSpace(s) == "" || !utf8.ValidString(s) || strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return fmt.Errorf("must be 1 to %d characters with no control characters", MaxEventTypeChars)
	}
	return nil
}

// parsed is a validated Config with its defaults applied.
type parsed struct {
	Config
	base    *url.URL
	timeout time.Duration
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("github-dispatch: invalid configuration: "+format, a...)
}

// parseConfig validates raw. deep also compiles the templates and renders
// them on the sample events (CheckTemplate), as Validate does when a sink is
// stored; Deliver does not repeat that: it compiles the templates it renders.
func parseConfig(raw sink.Config, deep bool) (parsed, error) {
	var p parsed
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p.Config); err != nil {
		return p, invalid("want an object with repo, token_env or token_ref, and the keys of the mode")
	}
	switch p.Mode {
	case "":
		p.Mode = ModeRepositoryDispatch
	case ModeRepositoryDispatch, ModeWorkflowDispatch:
	default:
		return p, invalid("mode must be %s or %s", ModeRepositoryDispatch, ModeWorkflowDispatch)
	}
	if !ValidRepo(p.Repo) {
		return p, invalid("repo must be owner/name")
	}
	if p.APIBaseURL == "" {
		p.APIBaseURL = DefaultAPIBaseURL
	}
	if err := CheckAPIBaseURL(p.APIBaseURL); err != nil {
		return p, invalid("api_base_url %v", err)
	}
	p.base, _ = url.Parse(p.APIBaseURL)
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
	if p.Mode == ModeRepositoryDispatch {
		if p.Workflow != "" || p.Ref != "" || p.Inputs != nil {
			return p, invalid("workflow, ref and inputs are for mode %s", ModeWorkflowDispatch)
		}
		if p.EventType == "" {
			p.EventType = DefaultEventType
		}
		if err := p.checkTemplate(p.EventType, MaxEventTypeTemplate, deep); err != nil {
			return p, invalid("event_type %v", err)
		}
		return p, nil
	}
	if p.EventType != "" {
		return p, invalid("event_type is for mode %s", ModeRepositoryDispatch)
	}
	if err := CheckWorkflow(p.Workflow); err != nil {
		return p, invalid("workflow %v", err)
	}
	if err := CheckRef(p.Ref); err != nil {
		return p, invalid("ref %v", err)
	}
	if len(p.Inputs) > MaxInputs {
		return p, invalid("at most %d inputs", MaxInputs)
	}
	for k, v := range p.Inputs {
		if err := CheckInputName(k); err != nil {
			return p, invalid("inputs: %v", err)
		}
		if err := p.checkTemplate(v, MaxInputTemplate, deep); err != nil {
			return p, invalid("an input value %v", err)
		}
	}
	return p, nil
}

func (parsed) checkTemplate(src string, limit int, deep bool) error {
	if deep {
		return CheckTemplate(src, limit)
	}
	if len(src) > limit {
		return fmt.Errorf("at most %d bytes", limit)
	}
	return nil
}
