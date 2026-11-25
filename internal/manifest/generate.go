package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/npm"
	"github.com/angelia-dev/angelia/internal/source/pypi"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

// maxNameLen is the longest watch name; a longer one is cut and hashed.
const (
	maxNameLen = 63
	cutLen     = 54
)

var (
	projectRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,23}$`)
	scopeRE   = regexp.MustCompile(`^@[a-z0-9][a-z0-9._~-]*$`)
)

// kindSpec is what the generator knows about a source kind.
type kindSpec struct {
	// canonical returns the canonical package name: the stored coordinate.
	canonical func(name string) string
}

var kinds = map[string]kindSpec{
	KindPyPI: {canonical: func(n string) string { return pypi.Normalize(strings.TrimSpace(n)) }},
	// npm stores a name as written: the source has no Canonical, and its
	// Validate (run by the generator) rejects what is not a valid name.
	KindNPM: {canonical: func(n string) string { return n }},
}

// Options controls Generate. The zero value is not valid: Project is required.
type Options struct {
	Project string
	// Labels are the owner labels; nil means project=<Project>.
	Labels map[string]string
	// Include lists the dependency types to keep; nil means every type but peer.
	Include []string
	// Exclude holds globs matched on the package name.
	Exclude []string
	// Sources maps a kind (or an npm "@scope") to the name of a source instance.
	Sources map[string]string
	// Interval is a Go duration for every watch; empty leaves the default.
	Interval string
	// Policy is the content of a policy file (YAML), copied into every
	// watch; PolicyName names it in errors.
	Policy     []byte
	PolicyName string
	// Subscribe names an existing sink: the output gets one subscription on it.
	Subscribe string
	// Inputs are the manifest names given on the command line, for the header.
	Inputs []string
}

// Entry reports what happened to one dependency.
type Entry struct {
	Dep      Dependency
	Watch    string // empty when excluded
	Excluded bool
}

// Output is a generated watch file.
type Output struct {
	YAML    []byte
	Entries []Entry // by watch name, then excluded ones by name
}

type (
	docOut struct {
		Version       int        `yaml:"version"`
		Watches       []watchOut `yaml:"watches"`
		Subscriptions []subOut   `yaml:"subscriptions,omitempty"`
	}
	watchOut struct {
		Name        string            `yaml:"name"`
		Source      string            `yaml:"source"`
		Coordinates map[string]string `yaml:"coordinates"`
		Interval    string            `yaml:"interval,omitempty"`
		Labels      map[string]string `yaml:"labels"`
		Policy      map[string]any    `yaml:"policy,omitempty"`
	}
	subOut struct {
		Name   string    `yaml:"name"`
		Sink   string    `yaml:"sink"`
		Filter filterOut `yaml:"filter"`
	}
	filterOut struct {
		Labels map[string]string `yaml:"labels"`
	}
)

// ParseSourceMappings reads the --source values (KIND=NAME).
func ParseSourceMappings(vals []string) (map[string]string, error) {
	out := map[string]string{}
	for _, v := range vals {
		k, name, ok := strings.Cut(v, "=")
		switch {
		case !ok || k == "" || name == "":
			return nil, fmt.Errorf("--source %q: want KIND=NAME", v)
		case !watchfile.ValidName(name):
			return nil, fmt.Errorf("--source %q: %q is not a valid source name (%s)", v, name, watchfile.NamePattern)
		}
		if strings.HasPrefix(k, "@") {
			if !scopeRE.MatchString(k) {
				return nil, fmt.Errorf("--source %q: %q is not an npm scope (@name)", v, k)
			}
		} else if _, ok := kinds[k]; !ok {
			return nil, fmt.Errorf("--source %q: unknown kind %q, want %s, or an npm scope with the @ (@scope=NAME)", v, k, strings.Join(kindNames(), ", "))
		}
		if prev, dup := out[k]; dup && prev != name {
			return nil, fmt.Errorf("--source: %q is mapped twice (%s and %s)", k, prev, name)
		}
		out[k] = name
	}
	return out, nil
}

func kindNames() []string {
	var n []string
	for k := range kinds {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// sourceFor returns the source a watch of d uses: the instance of the npm
// scope of d, else of its kind, else the kind.
func (o *Options) sourceFor(d Dependency) string {
	if d.Kind == KindNPM && strings.HasPrefix(d.Name, "@") {
		scope, _, _ := strings.Cut(d.Name, "/")
		if s, ok := o.Sources[scope]; ok {
			return s
		}
	}
	if s, ok := o.Sources[d.Kind]; ok {
		return s
	}
	return d.Kind
}

func (o *Options) validate() error {
	if !projectRE.MatchString(o.Project) {
		return fmt.Errorf("--project %q: must match %s", o.Project, projectRE)
	}
	for _, t := range o.Include {
		if !slices.Contains(Types, t) {
			return fmt.Errorf("--include %q: want a comma-separated list of %s", t, strings.Join(Types, ", "))
		}
	}
	for _, g := range o.Exclude {
		if _, err := path.Match(g, ""); err != nil {
			return fmt.Errorf("--exclude %q: %w", g, err)
		}
	}
	if o.Interval != "" {
		d, err := time.ParseDuration(o.Interval)
		switch {
		case err != nil:
			return fmt.Errorf("--interval %q: %w", o.Interval, err)
		case d < watchfile.MinInterval || d > watchfile.MaxInterval:
			return fmt.Errorf("--interval %s: must be between %s and %s", o.Interval, watchfile.MinInterval, watchfile.MaxInterval)
		case d%time.Second != 0:
			return fmt.Errorf("--interval %s: must be a whole number of seconds", o.Interval)
		}
	}
	if o.Subscribe != "" && !watchfile.ValidName(o.Subscribe) {
		return fmt.Errorf("--subscribe %q: not a valid sink name (%s)", o.Subscribe, watchfile.NamePattern)
	}
	return nil
}

func (o *Options) ownerLabels() map[string]string {
	if o.Labels != nil {
		return o.Labels
	}
	return map[string]string{"project": o.Project}
}

func (o *Options) included(t string) bool {
	if o.Include == nil {
		return t != TypePeer
	}
	return slices.Contains(o.Include, t)
}

func (o *Options) excluded(d Dependency) bool {
	for _, g := range o.Exclude {
		for _, n := range []string{d.Name, kinds[d.Kind].canonical(d.Name)} {
			if ok, _ := path.Match(g, n); ok {
				return true
			}
		}
	}
	return false
}

// Generate turns deps into a watch file (docs/watch-file.md, format v1).
// The output depends only on deps and o, not on their order. The result is
// checked with the real watch-file parser before it is returned.
func Generate(deps []Dependency, o Options) (*Output, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	if _, reserved := o.Labels["dependency"]; reserved {
		return nil, errors.New(`-l dependency=...: the label "dependency" is set per watch by angelia import`)
	}
	policy, err := o.policyBlock()
	if err != nil {
		return nil, err
	}
	merged, excluded := o.collect(deps)

	var watches []watchOut
	var entries []Entry
	byName := map[string]Dependency{}
	for _, d := range merged {
		canon := kinds[d.Kind].canonical(d.Name)
		name := watchName(o.Project, d.Kind, canon)
		if prev, dup := byName[name]; dup {
			return nil, fmt.Errorf("%s and %s both map to the watch name %q; exclude one with --exclude", describe(prev), describe(d), name)
		}
		byName[name] = d
		labels := map[string]string{"dependency": d.Type}
		for k, v := range o.ownerLabels() {
			labels[k] = v
		}
		w := watchOut{
			Name: name, Source: d.Kind, Coordinates: map[string]string{"name": canon},
			Interval: o.Interval, Labels: labels, Policy: policy,
		}
		if o.Interval != "" {
			w.Interval = formatDuration(o.Interval)
		}
		watches = append(watches, w)
		entries = append(entries, Entry{Dep: d, Watch: name})
	}
	sort.Slice(watches, func(i, j int) bool { return watches[i].Name < watches[j].Name })
	sort.Slice(entries, func(i, j int) bool { return entries[i].Watch < entries[j].Watch })
	sort.SliceStable(excluded, func(i, j int) bool { return excluded[i].Dep.Name < excluded[j].Dep.Name })
	entries = append(entries, excluded...)
	if watches == nil {
		watches = []watchOut{}
	}

	doc := docOut{Version: watchfile.Version, Watches: watches}
	if o.Subscribe != "" {
		doc.Subscriptions = []subOut{{Name: o.Project + "-deps", Sink: o.Subscribe, Filter: filterOut{Labels: o.ownerLabels()}}}
	}
	// Check with the built-in kind as the source: the client cannot know the
	// instances; the server validates the instance name at apply.
	if err := check(doc, o.ownerLabels(), byName); err != nil {
		return nil, err
	}
	for i := range doc.Watches {
		doc.Watches[i].Source = o.sourceFor(byName[doc.Watches[i].Name])
	}
	body, err := marshal(doc)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "# Generated by angelia import from %s. Changes are overwritten by the next import.\n", headerNames(o.Inputs))
	b.Write(body)
	return &Output{YAML: b.Bytes(), Entries: entries}, nil
}

func describe(d Dependency) string { return fmt.Sprintf("%s %s (%s)", d.Kind, d.Name, d.Origin) }

func marshal(doc docOut) ([]byte, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("encode watch file: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode watch file: %w", err)
	}
	return b.Bytes(), nil
}

func headerNames(in []string) string {
	seen := map[string]bool{}
	var names []string
	for _, n := range in {
		n = strings.Map(func(r rune) rune {
			if r < ' ' || r == 0x7f {
				return '?'
			}
			return r
		}, n)
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// formatDuration prints d the short way: 6h, 90m.
func formatDuration(s string) string {
	d, _ := time.ParseDuration(s)
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return d.String()
}

// collect filters by type, drops the excluded packages and merges the rest
// by package: the first of runtime, optional, dev, peer wins.
func (o *Options) collect(deps []Dependency) (merged []Dependency, excluded []Entry) {
	type key struct{ kind, name string }
	idx := map[key]int{}
	seenEx := map[key]bool{}
	for _, d := range deps {
		if !o.included(d.Type) {
			continue
		}
		k := key{d.Kind, kinds[d.Kind].canonical(d.Name)}
		if o.excluded(d) {
			if !seenEx[k] {
				seenEx[k] = true
				excluded = append(excluded, Entry{Dep: d, Excluded: true})
			}
			continue
		}
		i, ok := idx[k]
		switch {
		case !ok:
			idx[k] = len(merged)
			merged = append(merged, d)
		case precedence(d.Type) < precedence(merged[i].Type):
			merged[i] = d
		}
	}
	return merged, excluded
}

// watchName is <project>-<kind>-<slug>, cut and hashed when over 63 characters.
func watchName(project, kind, canon string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.ReplaceAll(canon, "@", "")) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	name := project + "-" + kind + "-" + b.String()
	if len(name) <= maxNameLen {
		return name
	}
	sum := sha256.Sum256([]byte(kind + "\x00" + canon))
	return name[:cutLen] + "-" + hex.EncodeToString(sum[:])[:8]
}

// policyBlock decodes the policy file, if any, and validates it for every
// kind the generator knows, reporting the line of the file.
func (o *Options) policyBlock() (map[string]any, error) {
	if len(bytes.TrimSpace(o.Policy)) == 0 {
		if o.Policy != nil {
			return nil, fmt.Errorf("%s: empty policy file", o.PolicyName)
		}
		return nil, nil
	}
	var block map[string]any
	if err := yaml.Unmarshal(o.Policy, &block); err != nil {
		return nil, fmt.Errorf("%s: %w", o.PolicyName, err)
	}
	if len(block) == 0 {
		return nil, fmt.Errorf("%s: want a mapping with the keys of a watch's policy (prerelease, constraint, cooldown, ...)", o.PolicyName)
	}
	// Check the file as the policy of a probe watch, so that errors carry the
	// line of the policy file: it starts at line 7 of the probe.
	const headerLines = 6
	var indented strings.Builder
	for _, l := range strings.Split(strings.TrimRight(string(o.Policy), "\r\n"), "\n") {
		indented.WriteString("      " + l + "\n")
	}
	var seen []string
	for _, kind := range kindNames() {
		probe := fmt.Sprintf("version: 1\nwatches:\n  - name: probe\n    source: %s\n    coordinates: {name: probe}\n    policy:\n%s", kind, indented.String())
		_, err := watchfile.Parse([]byte(probe), o.PolicyName, probeOptions())
		var errs watchfile.Errors
		if errors.As(err, &errs) {
			for _, e := range errs {
				msg := fmt.Sprintf("%s: line %d: %s", o.PolicyName, max(e.Line-headerLines, 1), e.Msg)
				if e.Path != "" {
					msg = fmt.Sprintf("%s: line %d: %s: %s", o.PolicyName, max(e.Line-headerLines, 1), strings.TrimPrefix(e.Path, "watches[0].policy."), e.Msg)
				}
				if !slices.Contains(seen, msg) {
					seen = append(seen, msg)
				}
			}
		} else if err != nil {
			return nil, err
		}
	}
	if len(seen) > 0 {
		return nil, fmt.Errorf("invalid policy:\n%s", strings.Join(seen, "\n"))
	}
	return block, nil
}

// probeOptions builds the watch-file options of a check: the built-in kinds,
// which never see the network here.
func probeOptions() watchfile.Options {
	reg := source.NewRegistry()
	hc := &http.Client{}
	for _, s := range []source.Source{pypi.New(pypi.DefaultBaseURL, hc), npm.New("", hc)} {
		if err := reg.Register(s); err != nil {
			panic(err) // a duplicate kind in a fixed list
		}
	}
	return watchfile.Options{Sources: reg}
}

// check parses the watches of doc with the real watch-file parser: labels,
// names, coordinates and the policy are the server's. byName names the
// dependency of each watch, for the messages.
func check(doc docOut, owner map[string]string, byName map[string]Dependency) error {
	doc.Subscriptions = nil // they name a sink that only the server knows
	// The owner labels are checked even when no watch carries them (also
	// the filter of the subscription).
	doc.Watches = append(slices.Clone(doc.Watches), watchOut{
		Name: "labels-probe", Source: KindPyPI, Coordinates: map[string]string{"name": "probe"}, Labels: owner,
	})
	data, err := marshal(doc)
	if err != nil {
		return err
	}
	_, err = watchfile.Parse(data, "generated watches", probeOptions())
	var errs watchfile.Errors
	if !errors.As(err, &errs) {
		return err
	}
	var msgs []string
	for _, e := range errs {
		msg := e.Msg
		var wi int
		if _, scanErr := fmt.Sscanf(e.Path, "watches[%d]", &wi); scanErr == nil && wi == len(doc.Watches)-1 {
			msg = fmt.Sprintf("%s: %s", strings.TrimPrefix(strings.TrimPrefix(e.Path, fmt.Sprintf("watches[%d]", wi)), "."), e.Msg)
		} else if scanErr == nil && wi < len(doc.Watches) {
			d := byName[doc.Watches[wi].Name]
			rest := strings.TrimPrefix(strings.TrimPrefix(e.Path, fmt.Sprintf("watches[%d]", wi)), ".")
			msg = fmt.Sprintf("watch %s (%s %s, %s): %s: %s", doc.Watches[wi].Name, d.Kind, d.Name, d.Origin, rest, e.Msg)
		} else if e.Path != "" {
			msg = e.Path + ": " + msg
		}
		msgs = append(msgs, msg)
	}
	return fmt.Errorf("the generated watches are not valid:\n%s", strings.Join(slices.Compact(msgs), "\n"))
}
