package watchfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/dispatch"
	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/policy"
	"github.com/angelia-dev/angelia/internal/render"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/sink/githubdispatch"
	"github.com/angelia-dev/angelia/internal/sink/shoutrrr"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/subscription"
	"github.com/angelia-dev/angelia/internal/version"
	"github.com/angelia-dev/angelia/internal/version/schemes"
)

var (
	nameRE     = regexp.MustCompile(NamePattern)
	labelKeyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,62}$`)

	prereleaseModes = []string{"none", "only", "include"}
	bumpKinds       = []string{"major", "minor", "patch"}
	eventTypes      = []string{"release.published", "release.yanked", "release.prerelease"}
	secretQueryKeys = []string{"token", "secret", "key", "password", "passwd", "auth", "sig"}
)

// NamePattern is the pattern of the name of a sink, watch or subscription
// (docs/watch-file.md section 2.1), which the API shares.
const NamePattern = `^[a-z0-9][a-z0-9._-]{0,62}$`

// ValidName reports whether s is a valid sink, watch or subscription name.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// SinkKinds are the sink kinds a watch file may declare.
var SinkKinds = []string{"webhook", githubdispatch.Kind, shoutrrr.Kind}

// Options controls Parse.
type Options struct {
	// Sources validates watch coordinates (Source.Validate, no I/O). Required.
	Sources *source.Registry
	// AllowHTTPHosts lists host names for which a webhook may use http://
	// instead of https:// (decision D12). Empty: https only.
	AllowHTTPHosts []string
	// LookupEnv reports whether the environment variable named by a sink's
	// secret_env is set; it only feeds warnings. Nil means os.LookupEnv.
	LookupEnv func(string) (string, bool)
}

// Load reads and validates the watch file at path.
func Load(path string, opts Options) (*File, error) {
	f, err := os.Open(path) //nolint:gosec // the operator chooses the path
	if err != nil {
		return nil, fmt.Errorf("read watch file: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("read watch file: %w", err)
	}
	return Parse(data, path, opts)
}

// Parse validates data as a watch file. name is used in error messages. The
// whole file is checked and every problem is returned, as an [Errors] value,
// each with its line and key path.
func Parse(data []byte, name string, opts Options) (*File, error) {
	p := &parser{file: name, opts: opts}
	if len(data) > MaxFileSize {
		p.errs = append(p.errs, Error{File: name, Msg: fmt.Sprintf("file is larger than the %d MiB limit", MaxFileSize>>20)})
		return nil, p.errs
	}
	root := p.document(data)
	var f *File
	if root != nil {
		f = p.top(root)
	}
	if len(p.errs) > 0 {
		sort.SliceStable(p.errs, func(i, j int) bool { return p.errs[i].Line < p.errs[j].Line })
		return nil, p.errs
	}
	f.Warnings = p.warns
	return f, nil
}

type parser struct {
	file  string
	opts  Options
	errs  Errors
	warns []string
}

func (p *parser) errf(n *yaml.Node, jp, format string, a ...any) {
	line := 0
	if n != nil {
		line = n.Line
	}
	p.errs = append(p.errs, Error{File: p.file, Line: line, Path: jp, Msg: fmt.Sprintf(format, a...)})
}

func (p *parser) warnf(n *yaml.Node, jp, format string, a ...any) {
	p.warns = append(p.warns, Error{File: p.file, Line: n.Line, Path: jp, Msg: fmt.Sprintf(format, a...)}.Error())
}

// document decodes exactly one YAML document and returns its root node.
func (p *parser) document(data []byte) *yaml.Node {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			p.errs = append(p.errs, Error{File: p.file, Line: 1, Msg: fmt.Sprintf("file is empty: a watch file needs at least \"version: %d\"", Version)})
			return nil
		}
		p.errs = append(p.errs, yamlError(p.file, err))
		return nil
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		p.errf(&extra, "", "multiple YAML documents are not supported")
		return nil
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		p.errf(&doc, "", "not a YAML mapping")
		return nil
	}
	return doc.Content[0]
}

var yamlLineRE = regexp.MustCompile(`^yaml: line (\d+): (.*)$`)

func yamlError(file string, err error) Error {
	msg := err.Error()
	if g := yamlLineRE.FindStringSubmatch(msg); g != nil {
		line, _ := strconv.Atoi(g[1])
		return Error{File: file, Line: line, Msg: "invalid YAML: " + g[2]}
	}
	return Error{File: file, Msg: "invalid YAML: " + strings.TrimPrefix(msg, "yaml: ")}
}

// mapping checks that n is a mapping with only the allowed keys, each at most
// once, and returns its entries. A nil result means an error was reported.
func (p *parser) mapping(n *yaml.Node, jp string, allowed ...string) map[string]*yaml.Node {
	if n.Kind == yaml.AliasNode {
		p.errf(n, jp, "YAML aliases are not supported")
		return nil
	}
	if n.Kind != yaml.MappingNode {
		p.errf(n, jp, "must be a mapping")
		return nil
	}
	m := make(map[string]*yaml.Node, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Kind != yaml.ScalarNode {
			p.errf(k, jp, "keys must be plain strings")
			continue
		}
		kp := join(jp, k.Value)
		switch {
		case len(allowed) > 0 && !slices.Contains(allowed, k.Value):
			p.errf(k, kp, "unknown key %q (allowed: %s)", k.Value, strings.Join(allowed, ", "))
		case m[k.Value] != nil:
			p.errf(k, kp, "duplicate key")
		default:
			m[k.Value] = v
		}
	}
	return m
}

func join(jp, key string) string {
	if jp == "" {
		return key
	}
	return jp + "." + key
}

func isNull(n *yaml.Node) bool { return n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null" }

// str reads a string scalar. Numbers, booleans and null are rejected so that
// an unquoted 1.10 is not silently read as 1.1.
func (p *parser) str(n *yaml.Node, jp string) (string, bool) {
	if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!str" {
		p.errf(n, jp, "must be a string (quote it if it looks like a number or boolean)")
		return "", false
	}
	return n.Value, true
}

// secretFree is str for values that must never hold a credential (labels,
// URLs, *_env names): token-looking values are rejected without being echoed.
func (p *parser) secretFree(n *yaml.Node, jp string) (string, bool) {
	s, ok := p.str(n, jp)
	if ok && config.LooksLikeToken(s) {
		p.errf(n, jp, "looks like a credential; put secrets in an environment variable and reference it with a *_env key")
		return "", false
	}
	return s, ok
}

func (p *parser) boolean(n *yaml.Node, jp string) (bool, bool) {
	if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!bool" {
		p.errf(n, jp, "must be true or false")
		return false, false
	}
	return n.Value == "true", true
}

func (p *parser) duration(n *yaml.Node, jp string, lo, hi time.Duration, wholeSeconds bool) (time.Duration, bool) {
	s, ok := p.str(n, jp)
	if !ok {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		p.errf(n, jp, "invalid duration (use Go syntax such as 30m or 6h)")
		return 0, false
	}
	if d < lo || d > hi {
		p.errf(n, jp, "must be between %s and %s", lo, hi)
		return 0, false
	}
	if wholeSeconds && d%time.Second != 0 {
		p.errf(n, jp, "must be a whole number of seconds (for example 90s or 1m30s)")
		return 0, false
	}
	return d, true
}

func (p *parser) seq(n *yaml.Node, jp string) []*yaml.Node {
	if isNull(n) {
		return nil
	}
	if n.Kind != yaml.SequenceNode {
		p.errf(n, jp, "must be a list")
		return nil
	}
	return n.Content
}

func (p *parser) strList(n *yaml.Node, jp string, limit int) ([]string, bool) {
	items := p.seq(n, jp)
	if limit > 0 && len(items) > limit {
		p.errf(n, jp, "at most %d entries", limit)
		return nil, false
	}
	out := make([]string, 0, len(items))
	ok := true
	for i, it := range items {
		s, good := p.str(it, fmt.Sprintf("%s[%d]", jp, i))
		ok = ok && good
		out = append(out, s)
	}
	return out, ok
}

func (p *parser) enumList(n *yaml.Node, jp string, allowed []string) []string {
	vals, _ := p.strList(n, jp, 0)
	for i, v := range vals {
		if !slices.Contains(allowed, v) {
			p.errf(n.Content[i], fmt.Sprintf("%s[%d]", jp, i), "must be one of %s", strings.Join(allowed, ", "))
		}
	}
	return vals
}

func (p *parser) enum(n *yaml.Node, jp string, allowed []string) string {
	s, ok := p.str(n, jp)
	if ok && !slices.Contains(allowed, s) {
		p.errf(n, jp, "must be one of %s", strings.Join(allowed, ", "))
	}
	return s
}

// stringMap reads a map of string to string; check validates each key and value.
func (p *parser) stringMap(n *yaml.Node, jp string, noSecrets bool, check func(k string, kn *yaml.Node, v string, vn *yaml.Node)) map[string]string {
	if isNull(n) {
		return map[string]string{}
	}
	m := p.mapping(n, jp)
	out := make(map[string]string, len(m))
	for i := 0; i+1 < len(n.Content) && m != nil; i += 2 {
		k, vn := n.Content[i], n.Content[i+1]
		if m[k.Value] != vn {
			continue // duplicate, already reported
		}
		read := p.str
		if noSecrets {
			read = p.secretFree
		}
		v, ok := read(vn, join(jp, k.Value))
		if !ok {
			continue
		}
		if check != nil {
			check(k.Value, k, v, vn)
		}
		out[k.Value] = v
	}
	return out
}

func (p *parser) labels(n *yaml.Node, jp string) map[string]string {
	out := p.stringMap(n, jp, true, func(k string, kn *yaml.Node, v string, vn *yaml.Node) {
		kp := join(jp, k)
		if !labelKeyRE.MatchString(k) {
			p.errf(kn, kp, "invalid label key (pattern %s)", labelKeyRE)
		}
		if len(v) > maxValueLen || strings.IndexFunc(v, unicode.IsControl) >= 0 {
			p.errf(vn, kp, "label value must be at most %d characters with no control characters", maxValueLen)
		}
	})
	if len(out) > maxLabels {
		p.errf(n, jp, "at most %d entries", maxLabels)
	}
	return out
}

func (p *parser) name(m map[string]*yaml.Node, jp string, parent *yaml.Node, seen map[string]int) string {
	n := m["name"]
	if n == nil {
		p.errf(parent, jp, "missing required key \"name\"")
		return ""
	}
	s, ok := p.str(n, jp+".name")
	if !ok {
		return ""
	}
	if !ValidName(s) {
		p.errf(n, jp+".name", "invalid name (pattern %s)", NamePattern)
		return ""
	}
	if first, dup := seen[s]; dup {
		p.errf(n, jp+".name", "duplicate name %q (first used on line %d)", s, first)
		return ""
	}
	seen[s] = n.Line
	return s
}

func (p *parser) need(m map[string]*yaml.Node, key, jp string, parent *yaml.Node) *yaml.Node {
	n := m[key]
	if n == nil {
		p.errf(parent, jp, "missing required key %q", key)
	}
	return n
}

func (p *parser) top(root *yaml.Node) *File {
	m := p.mapping(root, "", "version", "sinks", "watches", "subscriptions")
	f := &File{}
	if m == nil {
		return f
	}
	if v := m["version"]; v == nil {
		p.errf(root, "version", "missing required key \"version\" (it guards against an empty or truncated file)")
	} else if n, err := strconv.Atoi(v.Value); v.Kind != yaml.ScalarNode || v.ShortTag() != "!!int" || err != nil || n != Version {
		p.errf(v, "version", "must be %d", Version)
	} else {
		f.Version = n
	}

	sinkNames := map[string]int{}
	if n := m["sinks"]; n != nil {
		for i, it := range p.seq(n, "sinks") {
			if s, ok := p.sink(it, fmt.Sprintf("sinks[%d]", i), sinkNames); ok {
				f.Sinks = append(f.Sinks, s)
			}
		}
	}
	watchNames := map[string]int{}
	if n := m["watches"]; n != nil {
		for i, it := range p.seq(n, "watches") {
			if w, ok := p.watch(it, fmt.Sprintf("watches[%d]", i), watchNames); ok {
				f.Watches = append(f.Watches, w)
			}
		}
	}
	subNames := map[string]int{}
	if n := m["subscriptions"]; n != nil {
		for i, it := range p.seq(n, "subscriptions") {
			jp := fmt.Sprintf("subscriptions[%d]", i)
			s, ok := p.subscription(it, jp, subNames)
			if !ok {
				continue
			}
			if _, known := sinkNames[s.Sink]; !known {
				p.errf(it, jp+".sink", "no sink named %q in this file", s.Sink)
			} else if s.Template != "" && slices.ContainsFunc(f.Sinks, func(k Sink) bool { return k.Name == s.Sink && k.Kind == githubdispatch.Kind }) {
				p.warnf(it, jp+".template", "template has no effect on a github-dispatch sink; use event_type or inputs")
			}
			f.Subscriptions = append(f.Subscriptions, s)
		}
	}
	return f
}

func (p *parser) sink(n *yaml.Node, jp string, seen map[string]int) (Sink, bool) {
	before := len(p.errs)
	m := p.mapping(n, jp, "name", "kind", "config")
	if m == nil {
		return Sink{}, false
	}
	s := Sink{Line: n.Line}
	s.Name = p.name(m, jp, n, seen)
	if kn := p.need(m, "kind", jp+".kind", n); kn != nil {
		s.Kind = p.enum(kn, jp+".kind", SinkKinds)
	}
	if cn := p.need(m, "config", jp+".config", n); cn != nil {
		switch s.Kind {
		case "webhook":
			s.Config = p.webhook(cn, jp+".config")
		case githubdispatch.Kind:
			s.Config = p.githubDispatch(cn, jp+".config")
		case shoutrrr.Kind:
			s.Config = p.shoutrrr(cn, jp+".config")
		}
	}
	return s, len(p.errs) == before
}

func (p *parser) webhook(n *yaml.Node, jp string) json.RawMessage {
	m := p.mapping(n, jp, "url", "secret_env", "secret_ref", "timeout")
	if m == nil {
		return nil
	}
	cfg := struct {
		URL       string `json:"url"`
		SecretEnv string `json:"secret_env,omitempty"`
		SecretRef string `json:"secret_ref,omitempty"`
		Timeout   string `json:"timeout"`
	}{Timeout: DefaultWebhookTimeout.String()}
	if un := p.need(m, "url", jp+".url", n); un != nil {
		if s, ok := p.secretFree(un, jp+".url"); ok && p.webhookURL(s, un, jp+".url") {
			cfg.URL = s
		}
	}
	cfg.SecretEnv, cfg.SecretRef = p.credentialRefs(m, jp, "secret_env", "secret_ref")
	if m["secret_env"] == nil && m["secret_ref"] == nil {
		p.warnf(n, jp, "no secret_env or secret_ref: the webhook is delivered unsigned")
	}
	if tn := m["timeout"]; tn != nil {
		if d, ok := p.duration(tn, jp+".timeout", MinWebhookTimeout, MaxWebhookTimeout, false); ok {
			cfg.Timeout = d.String()
			if d > dispatch.DefaultDeliverTimeout {
				// ADR 0023 decided question 3: the contract stays, the
				// dispatcher's bound wins.
				p.warnf(tn, jp+".timeout", "%v is above the dispatcher's deliver timeout (%v), which bounds every attempt: a longer timeout has no effect",
					d, dispatch.DefaultDeliverTimeout)
			}
		}
	}
	return marshal(cfg)
}

// shoutrrr reads the config of a shoutrrr sink (spec ShoutrrrSinkConfig,
// ADR 0026). The service URL is a secret: the file names it with url_ref
// or url_env and never holds it.
func (p *parser) shoutrrr(n *yaml.Node, jp string) json.RawMessage {
	m := p.mapping(n, jp, "url_ref", "url_env", "service", "timeout")
	if m == nil {
		return nil
	}
	cfg := shoutrrr.Config{Timeout: shoutrrr.DefaultTimeout.String()}
	cfg.URLEnv, cfg.URLRef = p.credentialRefs(m, jp, "url_env", "url_ref")
	if m["url_env"] == nil && m["url_ref"] == nil {
		p.errf(n, jp, "set exactly one of url_ref and url_env: the shoutrrr URL is a secret")
	}
	if sn := m["service"]; sn != nil {
		cfg.Service = p.enum(sn, jp+".service", shoutrrr.Services())
	}
	if tn := m["timeout"]; tn != nil {
		if d, ok := p.duration(tn, jp+".timeout", shoutrrr.MinTimeout, shoutrrr.MaxTimeout, false); ok {
			cfg.Timeout = d.String()
			if d > dispatch.DefaultDeliverTimeout {
				p.warnf(tn, jp+".timeout", "%v is above the dispatcher's deliver timeout (%v), which bounds every attempt: a longer timeout has no effect",
					d, dispatch.DefaultDeliverTimeout)
			}
		}
	}
	return marshal(cfg)
}

// githubDispatch reads the config of a github-dispatch sink (spec
// GithubDispatchSinkConfig). It writes the defaults into the stored
// configuration, so that api_base_url, the key a stored token is bound to
// (x-angelia-destination), is always present.
func (p *parser) githubDispatch(n *yaml.Node, jp string) json.RawMessage {
	m := p.mapping(n, jp, "mode", "repo", "api_base_url", "token_env", "token_ref", "event_type", "workflow", "ref", "inputs", "timeout")
	if m == nil {
		return nil
	}
	cfg := githubdispatch.Config{
		Mode: githubdispatch.ModeRepositoryDispatch, APIBaseURL: githubdispatch.DefaultAPIBaseURL,
		Timeout: githubdispatch.DefaultTimeout.String(),
	}
	if mn := m["mode"]; mn != nil {
		cfg.Mode = p.enum(mn, jp+".mode", []string{githubdispatch.ModeRepositoryDispatch, githubdispatch.ModeWorkflowDispatch})
	}
	workflow := cfg.Mode == githubdispatch.ModeWorkflowDispatch
	modeOK := cfg.Mode == githubdispatch.ModeRepositoryDispatch || workflow
	if rn := p.need(m, "repo", jp+".repo", n); rn != nil {
		if s, ok := p.secretFree(rn, jp+".repo"); ok {
			if githubdispatch.ValidRepo(s) {
				cfg.Repo = s
			} else {
				p.errf(rn, jp+".repo", "must be owner/name")
			}
		}
	}
	if un := m["api_base_url"]; un != nil {
		if s, ok := p.secretFree(un, jp+".api_base_url"); ok {
			if err := githubdispatch.CheckAPIBaseURL(s); err != nil {
				p.errf(un, jp+".api_base_url", "%v", err)
			} else {
				cfg.APIBaseURL = s
			}
		}
	}
	cfg.TokenEnv, cfg.TokenRef = p.credentialRefs(m, jp, "token_env", "token_ref")
	if m["token_env"] == nil && m["token_ref"] == nil {
		p.errf(n, jp, "set one of token_env and token_ref: GitHub accepts no unauthenticated dispatch")
	}
	only := func(key string, want string) bool {
		if m[key] != nil {
			p.errf(m[key], jp+"."+key, "only for mode %s", want)
			return true
		}
		return false
	}
	switch {
	case !modeOK:
		// The mode is already reported; its keys cannot be judged.
	case workflow:
		only("event_type", githubdispatch.ModeRepositoryDispatch)
		for _, k := range []struct {
			key   string
			check func(string) error
			dst   *string
		}{{"workflow", githubdispatch.CheckWorkflow, &cfg.Workflow}, {"ref", githubdispatch.CheckRef, &cfg.Ref}} {
			kn := p.need(m, k.key, jp+"."+k.key, n)
			if kn == nil {
				continue
			}
			if s, ok := p.secretFree(kn, jp+"."+k.key); ok {
				if err := k.check(s); err != nil {
					p.errf(kn, jp+"."+k.key, "%v", err)
				} else {
					*k.dst = s
				}
			}
		}
		if in := m["inputs"]; in != nil {
			cfg.Inputs = p.stringMap(in, jp+".inputs", true, func(k string, kn *yaml.Node, v string, vn *yaml.Node) {
				if err := githubdispatch.CheckInputName(k); err != nil {
					p.errf(kn, join(jp+".inputs", k), "%v", err)
				}
				p.template(vn, join(jp+".inputs", k), v, githubdispatch.MaxInputTemplate)
			})
			if len(cfg.Inputs) > githubdispatch.MaxInputs {
				p.errf(in, jp+".inputs", "at most %d inputs", githubdispatch.MaxInputs)
			}
		}
	default:
		for _, k := range []string{"workflow", "ref", "inputs"} {
			only(k, githubdispatch.ModeWorkflowDispatch)
		}
		cfg.EventType = githubdispatch.DefaultEventType
		if en := m["event_type"]; en != nil {
			if s, ok := p.secretFree(en, jp+".event_type"); ok {
				cfg.EventType = s
				if err := githubdispatch.CheckEventTypeTemplate(s); err != nil {
					p.errf(en, jp+".event_type", "%v", err)
				}
			}
		}
	}
	if tn := m["timeout"]; tn != nil {
		if d, ok := p.duration(tn, jp+".timeout", githubdispatch.MinTimeout, githubdispatch.MaxTimeout, false); ok {
			cfg.Timeout = d.String()
			if d > dispatch.DefaultDeliverTimeout {
				p.warnf(tn, jp+".timeout", "%v is above the dispatcher's deliver timeout (%v), which bounds every attempt: a longer timeout has no effect",
					d, dispatch.DefaultDeliverTimeout)
			}
		}
	}
	return marshal(cfg)
}

// template checks an event_type or workflow input template the way a
// subscription template is checked: it compiles and renders on the sample
// events. The error gives a position and a reason, never the template text.
func (p *parser) template(n *yaml.Node, jp, src string, limit int) {
	if err := githubdispatch.CheckTemplate(src, limit); err != nil {
		p.errf(n, jp, "%v", err)
	}
}

// credentialRefs reads the two keys that name a sink's credential, an
// environment variable (envKey) and a stored secret (refKey), and returns
// the names that are valid. At most one may be set; the caller decides
// whether neither is acceptable. The file never holds the credential.
func (p *parser) credentialRefs(m map[string]*yaml.Node, jp, envKey, refKey string) (env, ref string) {
	if sn := m[envKey]; sn != nil {
		if s, ok := p.secretFree(sn, join(jp, envKey)); ok {
			switch {
			case !config.IsEnvName(s):
				p.errf(sn, join(jp, envKey), "must be an environment variable name, not the secret itself")
			case config.ReservedEnvName(s):
				p.errf(sn, join(jp, envKey), "%s: they hold the instance's own settings", config.ReservedEnvRefusal)
			default:
				env = s
				lookup := p.opts.LookupEnv
				if lookup == nil {
					lookup = os.LookupEnv
				}
				if v, _ := lookup(s); v == "" {
					p.warnf(sn, join(jp, envKey), "environment variable %s is not set or empty; deliveries fail until it is set", s)
				}
			}
		}
	}
	if rn := m[refKey]; rn != nil {
		// A stored secret's name (ADR 0024): `angelia secret set` stores
		// the value; the file never holds it.
		if s, ok := p.secretFree(rn, join(jp, refKey)); ok {
			switch {
			case !secrets.ValidName(s):
				p.errf(rn, join(jp, refKey), "must be the name of a stored secret (pattern ^[a-z0-9][a-z0-9._-]{0,62}$), not the secret itself")
			case m[envKey] != nil:
				p.errf(rn, join(jp, refKey), "set at most one of %s and %s", envKey, refKey)
			default:
				ref = s
			}
		}
	}
	return env, ref
}

// webhookURL checks the URL without echoing it: it may carry a secret.
func (p *parser) webhookURL(s string, n *yaml.Node, jp string) bool {
	u, err := url.Parse(s)
	switch {
	case err != nil || u.Host == "" || u.Hostname() == "":
		p.errf(n, jp, "must be an absolute http(s) URL")
		return false
	case u.User != nil:
		p.errf(n, jp, "credentials in the URL are not allowed")
		return false
	case netguard.NonCanonicalIP(u.Hostname()):
		p.errf(n, jp, "the host is an IP address in a non-canonical form; write it as a.b.c.d")
		return false
	case u.Scheme == "https":
	case u.Scheme == "http":
		if !slices.ContainsFunc(p.opts.AllowHTTPHosts, func(h string) bool { return strings.EqualFold(h, u.Hostname()) }) {
			p.errf(n, jp, "must be https:// (http:// is accepted only for hosts on the instance allowlist)")
			return false
		}
	default:
		p.errf(n, jp, "scheme must be https")
		return false
	}
	if u.Fragment != "" || u.RawFragment != "" || strings.Contains(s, "#") {
		p.errf(n, jp, "the URL must not have a fragment")
		return false
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if config.LooksLikeToken(seg) {
			p.errf(n, jp, "a path segment looks like a credential; use secret_env to sign instead")
			return false
		}
	}
	for k, vs := range u.Query() {
		for _, v := range vs {
			if config.LooksLikeToken(v) {
				p.errf(n, jp, "a query value looks like a credential; use secret_env to sign instead")
				return false
			}
		}
		lk := strings.ToLower(k)
		if slices.ContainsFunc(secretQueryKeys, func(s string) bool { return strings.Contains(lk, s) }) {
			p.errf(n, jp, "the query string must not carry a secret (found parameter %q); use secret_env to sign instead", k)
			return false
		}
	}
	return true
}

func (p *parser) watch(n *yaml.Node, jp string, seen map[string]int) (Watch, bool) {
	before := len(p.errs)
	m := p.mapping(n, jp, "name", "source", "coordinates", "interval", "scheme", "labels", "policy")
	if m == nil {
		return Watch{}, false
	}
	w := Watch{Line: n.Line, Interval: DefaultInterval, Labels: map[string]string{}, Policy: defaultPolicy()}
	w.Name = p.name(m, jp, n, seen)

	var names []string
	if p.opts.Sources != nil {
		names = p.opts.Sources.Names()
	}
	if sn := p.need(m, "source", jp+".source", n); sn != nil {
		w.Source = p.enum(sn, jp+".source", names)
		if w.Source != "" && p.opts.Sources != nil {
			w.Kind = p.opts.Sources.KindOf(w.Source)
		}
	}
	if cn := p.need(m, "coordinates", jp+".coordinates", n); cn != nil {
		w.Coordinates = p.stringMap(cn, jp+".coordinates", false, nil)
		if len(p.errs) == before && w.Source != "" && p.opts.Sources != nil {
			if src, err := p.opts.Sources.Get(w.Source); err == nil {
				w.Coordinates = p.canonical(src, cn, jp, w)
			}
		}
	}
	if in := m["interval"]; in != nil {
		if d, ok := p.duration(in, jp+".interval", MinInterval, MaxInterval, true); ok {
			w.Interval = d
		}
	}
	if sn := m["scheme"]; sn != nil {
		w.Scheme = p.enum(sn, jp+".scheme", schemes.Names())
	}
	if ln := m["labels"]; ln != nil {
		w.Labels = p.labels(ln, jp+".labels")
	}
	if pn := m["policy"]; pn != nil && !isNull(pn) {
		p.policy(pn, jp+".policy", &w.Policy)
	}
	if len(p.errs) == before {
		p.compilePolicy(w, m, n, jp)
	}
	return w, len(p.errs) == before
}

// canonical validates the watch's coordinates with src and returns their
// canonical form (source.Canonical, ADR 0021), the one stored in the target
// row, so that every spelling of one upstream object shares one target. On
// an error it records it and returns the coordinates as written.
func (p *parser) canonical(src source.Source, cn *yaml.Node, jp string, w Watch) map[string]string {
	t := source.Target{Kind: w.SourceKind(), Coordinates: w.Coordinates}
	err := src.Validate(t)
	if err == nil {
		if t, err = source.Canonical(src, t); err == nil {
			return t.Coordinates
		}
	}
	// Drop the sentinel's text wherever it is: Validate errors start with it,
	// source.Canonical puts "canonical target for ..." before it.
	p.errf(cn, jp+".coordinates", "%s", strings.Replace(err.Error(), source.ErrInvalidTarget.Error()+": ", "", 1))
	return w.Coordinates
}

// compilePolicy runs the stored form of the watch's policy through the
// policy engine, so that what the engine would reject on every check (a
// constraint with an operand the watch's scheme cannot parse, say) fails at
// load time instead.
func (p *parser) compilePolicy(w Watch, m map[string]*yaml.Node, wn *yaml.Node, jp string) {
	pol, err := policy.Decode(w.PolicyJSON())
	if err == nil {
		var s version.Scheme
		if s, err = schemes.Select(w.SourceKind(), pol.Scheme, nil); err == nil {
			_, err = policy.Compile(pol, s)
		}
	}
	if err == nil {
		return
	}
	at := wn
	if pn := m["policy"]; pn != nil && !isNull(pn) {
		at = pn
	}
	for _, line := range strings.Split(err.Error(), "\n") {
		line = strings.TrimPrefix(line, policy.ErrInvalid.Error()+": ")
		path, node := jp+".policy", at
		// Only constraint realistically gets here: the parser above already
		// rejects the other keys; they are listed for safety.
		if key, _, ok := strings.Cut(line, ":"); ok && at != wn {
			switch key {
			case "constraint", "prerelease", "cooldown", "baseline":
				if kn := mapValue(at, key); kn != nil {
					path, node = jp+".policy."+key, kn
					line = strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(line, key+":"), " "), policy.ErrInvalid.Error()+": ")
				}
			}
		}
		p.errf(node, path, "%s", line)
	}
}

// mapValue returns the value node of key in a mapping node, or nil.
func mapValue(n *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func defaultPolicy() Policy { return Policy{Prerelease: "none", Baseline: true} }

func (p *parser) policy(n *yaml.Node, jp string, out *Policy) {
	m := p.mapping(n, jp, "prerelease", "include", "exclude", "constraint", "bump", "cooldown", "baseline")
	if m == nil {
		return
	}
	if v := m["prerelease"]; v != nil {
		out.Prerelease = p.enum(v, jp+".prerelease", prereleaseModes)
	}
	for _, k := range []string{"include", "exclude"} {
		v := m[k]
		if v == nil {
			continue
		}
		pats, _ := p.strList(v, jp+"."+k, maxPatterns)
		for i, pat := range pats {
			if _, err := regexp.Compile(pat); err != nil {
				p.errf(v.Content[i], fmt.Sprintf("%s.%s[%d]", jp, k, i), "invalid regular expression (RE2 syntax)")
			}
		}
		if k == "include" {
			out.Include = pats
		} else {
			out.Exclude = pats
		}
	}
	if v := m["constraint"]; v != nil {
		// The syntax and operands are checked by compilePolicy (docs/watch-file.md 2.3).
		if s, ok := p.str(v, jp+".constraint"); ok {
			if strings.TrimSpace(s) == "" {
				p.errf(v, jp+".constraint", "must not be empty (omit the key to accept any version)")
			}
			if strings.ContainsAny(s, "\r\n") {
				p.errf(v, jp+".constraint", "must be a single line")
			}
			out.Constraint = strings.TrimSpace(s)
		}
	}
	if v := m["bump"]; v != nil {
		out.Bump = p.enumList(v, jp+".bump", bumpKinds)
	}
	if v := m["cooldown"]; v != nil {
		if d, ok := p.duration(v, jp+".cooldown", 0, MaxCooldown, true); ok {
			out.Cooldown = d
		}
	}
	if v := m["baseline"]; v != nil {
		if b, ok := p.boolean(v, jp+".baseline"); ok {
			out.Baseline = b
		}
	}
}

func (p *parser) subscription(n *yaml.Node, jp string, seen map[string]int) (Subscription, bool) {
	before := len(p.errs)
	m := p.mapping(n, jp, "name", "sink", "filter", "policy", "template")
	if m == nil {
		return Subscription{}, false
	}
	s := Subscription{Line: n.Line}
	s.Name = p.name(m, jp, n, seen)
	if sn := p.need(m, "sink", jp+".sink", n); sn != nil {
		s.Sink, _ = p.str(sn, jp+".sink")
	}
	if fn := m["filter"]; fn != nil && !isNull(fn) {
		s.Filter = p.filter(fn, jp+".filter")
	}
	if len(s.Filter.Type) == 0 {
		s.Filter.Type = []string{"release.published"}
	}
	if pn := m["policy"]; pn != nil && !isNull(pn) {
		s.Policy = p.subscriptionPolicy(pn, jp+".policy")
	}
	if tn := m["template"]; tn != nil {
		if t, ok := p.secretFree(tn, jp+".template"); ok && t != "" {
			// Compiled and run on the sample events (docs/templates.md). Render
			// errors give a position and a reason, never the template text.
			tm, err := render.Compile(t, render.Limits{})
			switch {
			case errors.Is(err, render.ErrParse):
				p.errf(tn, jp+".template", "does not parse as a Go template: %v", err)
			case err != nil:
				p.errf(tn, jp+".template", "%v", err)
			default:
				if err := tm.Check(); err != nil {
					p.errf(tn, jp+".template", "%v", err)
				}
			}
			s.Template = t
		}
	}
	return s, len(p.errs) == before
}

// subscriptionPolicy parses the narrowing keys of a subscription. Only syntax
// is checked: a value that widens or does nothing is not an error, because
// the subscription may match watches for which it is useful, and Evaluate
// always intersects it with the watch (ADR 0014). Out-of-range cooldowns are
// clamped and cut to whole seconds.
func (p *parser) subscriptionPolicy(n *yaml.Node, jp string) subscription.Policy {
	var out subscription.Policy
	m := p.mapping(n, jp, "prerelease", "cooldown")
	if m == nil {
		return out
	}
	if v := m["prerelease"]; v != nil {
		out.Prerelease = p.enum(v, jp+".prerelease", prereleaseModes)
	}
	if v := m["cooldown"]; v != nil {
		if s, ok := p.str(v, jp+".cooldown"); ok {
			d, err := time.ParseDuration(s)
			if err != nil {
				p.errf(v, jp+".cooldown", "invalid duration (use Go syntax such as 30m or 72h)")
			} else {
				out.Cooldown = d.Truncate(time.Second)
			}
		}
	}
	return out.Clamp()
}

func (p *parser) filter(n *yaml.Node, jp string) Filter {
	var f Filter
	m := p.mapping(n, jp, "source", "labels", "bump", "package", "type")
	if m == nil {
		return f
	}
	if v := m["source"]; v != nil {
		var kinds []string
		if p.opts.Sources != nil {
			kinds = p.opts.Sources.Kinds()
		}
		f.Source = p.enumList(v, jp+".source", kinds)
	}
	if v := m["labels"]; v != nil {
		f.Labels = p.labels(v, jp+".labels")
	}
	if v := m["bump"]; v != nil {
		f.Bump = p.enumList(v, jp+".bump", bumpKinds)
	}
	if v := m["package"]; v != nil {
		if s, ok := p.str(v, jp+".package"); ok {
			if _, err := path.Match(s, ""); err != nil {
				p.errf(v, jp+".package", "invalid glob pattern")
			}
			f.Package = s
		}
	}
	if v := m["type"]; v != nil {
		f.Type = p.enumList(v, jp+".type", eventTypes)
	}
	return f
}
