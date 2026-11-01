package watchfile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Limits and defaults of the format (docs/watch-file.md section 2).
const (
	Version = 1

	DefaultInterval = time.Hour
	MinInterval     = 5 * time.Minute
	MaxInterval     = 720 * time.Hour
	MaxCooldown     = 8760 * time.Hour

	DefaultWebhookTimeout = 10 * time.Second
	MinWebhookTimeout     = time.Second
	MaxWebhookTimeout     = 2 * time.Minute

	maxLabels   = 32
	maxPatterns = 16
	maxValueLen = 253
)

// File is a parsed and validated watch file.
type File struct {
	Version       int
	Sinks         []Sink
	Watches       []Watch
	Subscriptions []Subscription
	// Warnings are non-fatal findings, in file order.
	Warnings []string
}

// Sink is a delivery destination.
type Sink struct {
	Name string
	Kind string
	// Config is the canonical JSON of the sink's validated configuration, with
	// defaults applied. It holds secret references (secret_env), never values.
	Config json.RawMessage
	Line   int
}

// Watch is one upstream to watch.
type Watch struct {
	Name        string
	Source      string
	Coordinates map[string]string
	Interval    time.Duration
	Scheme      string
	Labels      map[string]string
	Policy      Policy
	Line        int
}

// Policy is the validated watch policy with defaults applied.
type Policy struct {
	Prerelease string
	Include    []string
	Exclude    []string
	Constraint string
	Bump       []string
	Cooldown   time.Duration
	Baseline   bool
}

// Subscription routes the events matching Filter to the sink named Sink.
type Subscription struct {
	Name     string
	Sink     string
	Filter   Filter
	Template string
	Line     int
}

// Filter selects events; every given key must match.
type Filter struct {
	Source  []string
	Labels  map[string]string
	Bump    []string
	Package string
	Type    []string
}

// policyJSON is the stored form of a watch policy (watch.policy_json). The
// scheme rides along because the store has no column for it (decision D17).
type policyJSON struct {
	Prerelease string   `json:"prerelease"`
	Include    []string `json:"include"`
	Exclude    []string `json:"exclude"`
	Constraint string   `json:"constraint,omitempty"`
	Bump       []string `json:"bump"`
	Cooldown   string   `json:"cooldown"`
	Baseline   bool     `json:"baseline"`
	Scheme     string   `json:"scheme,omitempty"`
}

// PolicyJSON returns the canonical JSON stored as watch.policy_json.
func (w Watch) PolicyJSON() json.RawMessage {
	p := w.Policy
	b := marshal(policyJSON{
		Prerelease: p.Prerelease, Include: nonNilStrings(p.Include), Exclude: nonNilStrings(p.Exclude),
		Constraint: p.Constraint, Bump: nonNilStrings(p.Bump), Cooldown: p.Cooldown.String(),
		Baseline: p.Baseline, Scheme: w.Scheme,
	})
	return b
}

type filterJSON struct {
	Source  []string          `json:"source"`
	Labels  map[string]string `json:"labels"`
	Bump    []string          `json:"bump"`
	Package string            `json:"package,omitempty"`
	Type    []string          `json:"type"`
}

// FilterJSON returns the canonical JSON stored as subscription.filter_json.
func (s Subscription) FilterJSON() json.RawMessage {
	f := s.Filter
	labels := f.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	b := marshal(filterJSON{
		Source: nonNilStrings(f.Source), Labels: labels, Bump: nonNilStrings(f.Bump), Package: f.Package, Type: nonNilStrings(f.Type),
	})
	return b
}

// marshal encodes v as compact JSON without HTML escaping, so stored values
// read like the file ("<" stays "<"). Map keys are sorted: the output is
// canonical.
func marshal(v any) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v) // the types encoded here cannot fail
	return bytes.TrimRight(b.Bytes(), "\n")
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Error is one problem in a watch file.
type Error struct {
	File string
	Line int
	Path string // for example watches[1].policy.prerelease; empty for the document
	Msg  string
}

func (e Error) Error() string {
	var b strings.Builder
	b.WriteString(e.File)
	if e.Line > 0 {
		fmt.Fprintf(&b, ": line %d", e.Line)
	}
	if e.Path != "" {
		b.WriteString(": " + e.Path)
	}
	b.WriteString(": " + e.Msg)
	return b.String()
}

// Errors is the list of problems of one file, ordered by line. It implements
// error and unwraps to its members, so errors.As finds an [Error].
type Errors []Error

func (es Errors) Error() string {
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "\n")
}

// Unwrap lets errors.Is and errors.As see every member.
func (es Errors) Unwrap() []error {
	out := make([]error, len(es))
	for i := range es {
		out[i] = es[i]
	}
	return out
}
