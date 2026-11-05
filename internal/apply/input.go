// Package apply holds the plan logic of `angelia apply` (story #73): reading
// watch files (docs/watch-file.md, unchanged), the label scope of --prune and
// the diff of the report. It does no I/O on the network: cmd/angelia sends the
// requests and prints the report.
package apply

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/angelia-dev/angelia/internal/watchfile"
)

// The kinds apply handles, in dependency order.
const (
	Sink = iota
	Watch
	Subscription
	NumKinds
)

// KindInfo describes a resource type.
type KindInfo struct {
	Name    string // in the report: sink, watch, subscription
	Section string // the key of the watch file
	// WriteKeys are the keys of the *Write schema: the ones a diff compares.
	WriteKeys []string
}

// Kinds returns the resource types, indexed by Sink, Watch and Subscription.
func Kinds() [NumKinds]KindInfo {
	return [NumKinds]KindInfo{
		Sink:         {Name: "sink", Section: "sinks", WriteKeys: []string{"kind", "config"}},
		Watch:        {Name: "watch", Section: "watches", WriteKeys: []string{"source", "coordinates", "interval", "scheme", "labels", "policy"}},
		Subscription: {Name: "subscription", Section: "subscriptions", WriteKeys: []string{"sink", "filter", "policy", "template"}},
	}
}

// Object is one sink, watch or subscription read from a file.
type Object struct {
	Name   string
	Body   []byte // the object as JSON: the PUT body
	Origin string // file and position, for messages
	// Labels are a watch's labels, or a subscription's filter.labels.
	Labels map[string]string
	Sink   string // a subscription's sink
}

// Set is everything read from the files, per kind in file order.
type Set struct {
	Objects [NumKinds][]Object
	origins [NumKinds]map[string]string // name -> origin
}

// NewSet returns an empty set.
func NewSet() *Set {
	s := &Set{}
	for k := range s.origins {
		s.origins[k] = map[string]string{}
	}
	return s
}

// Has reports whether the files hold an object of kind k named name.
func (s *Set) Has(k int, name string) bool {
	_, ok := s.origins[k][name]
	return ok
}

// Read reads every path: a watch file, a directory of .yaml, .yml and .json
// watch files, or "-" for stdin. Each file holds one YAML document, as
// internal/watchfile requires. Every problem is reported.
func Read(stdin io.Reader, paths []string) (*Set, error) {
	set := NewSet()
	var errs []error
	for _, p := range paths {
		files, err := expand(p)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			label := f
			if f == "-" {
				label = "stdin"
			}
			raw, err := readFile(stdin, f, label)
			if err != nil {
				return nil, err
			}
			doc, err := decodeOne(raw)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", label, err))
				continue
			}
			errs = append(errs, set.AddDocument(doc, label)...)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return set, nil
}

// expand turns a directory into its .yaml, .yml and .json files (not
// recursive), sorted by name.
func expand(p string) ([]string, error) {
	if p == "-" {
		return []string{p}, nil
	}
	fi, err := os.Stat(p)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	if !fi.IsDir() {
		return []string{p}, nil
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	var out []string
	for _, e := range entries {
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".yaml", ".yml", ".json":
			if e.Type().IsRegular() {
				out = append(out, filepath.Join(p, e.Name()))
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no .yaml, .yml or .json file in the directory", p)
	}
	return out, nil
}

// readFile reads a file, or stdin for "-", up to watchfile.MaxFileSize.
func readFile(stdin io.Reader, path, label string) ([]byte, error) {
	r := stdin
	if path != "-" {
		f, err := os.Open(path) //nolint:gosec // the file the user named
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", label, err)
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	raw, err := io.ReadAll(io.LimitReader(r, watchfile.MaxFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", label, err)
	}
	if len(raw) > watchfile.MaxFileSize {
		return nil, fmt.Errorf("%s is larger than the %d MiB limit of a watch file", label, watchfile.MaxFileSize>>20)
	}
	return raw, nil
}

// decodeOne decodes the single YAML (or JSON) document of a watch file.
func decodeOne(raw []byte) (any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var doc any
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("empty: a watch file needs at least \"version: %d\"", watchfile.Version)
		}
		return nil, fmt.Errorf("parse: %w", err)
	}
	var extra any
	switch err := dec.Decode(&extra); {
	case errors.Is(err, io.EOF):
	case err != nil:
		return nil, fmt.Errorf("parse: %w", err)
	default:
		return nil, errors.New("several YAML documents: a watch file holds one document; split them into several files and pass a directory or repeat -f")
	}
	if doc == nil {
		return nil, fmt.Errorf("empty: a watch file needs at least \"version: %d\"", watchfile.Version)
	}
	return doc, nil
}

// AddDocument adds the objects of one watch file document (docs/watch-file.md
// section 2). Only the structure, names and label types are checked here; the
// server validates each object by the rules of the watch file.
func (s *Set) AddDocument(doc any, origin string) []error {
	top, ok := doc.(map[string]any)
	if !ok {
		return []error{fmt.Errorf("%s: must be a mapping with \"version: %d\" and sinks, watches, subscriptions", origin, watchfile.Version)}
	}
	var errs []error
	for _, k := range sortedKeys(top) {
		switch k {
		case "version", "sinks", "watches", "subscriptions":
		default:
			errs = append(errs, fmt.Errorf("%s: %s: unknown key", origin, k))
		}
	}
	switch v, ok := top["version"]; {
	case !ok:
		errs = append(errs, fmt.Errorf("%s: version: required, must be %d", origin, watchfile.Version))
	case v != watchfile.Version:
		errs = append(errs, fmt.Errorf("%s: version: must be %d", origin, watchfile.Version))
	}
	kinds := Kinds()
	for k := range kinds {
		sec := kinds[k].Section
		raw, ok := top[sec]
		if !ok || raw == nil {
			continue
		}
		items, ok := raw.([]any)
		if !ok {
			errs = append(errs, fmt.Errorf("%s: %s: must be a list", origin, sec))
			continue
		}
		for i, item := range items {
			errs = append(errs, s.addObject(k, item, fmt.Sprintf("%s: %s[%d]", origin, sec, i))...)
		}
	}
	return errs
}

func (s *Set) addObject(k int, item any, origin string) []error {
	obj, ok := item.(map[string]any)
	if !ok {
		return []error{fmt.Errorf("%s: must be an object", origin)}
	}
	name, _ := obj["name"].(string)
	if !watchfile.ValidName(name) {
		return []error{fmt.Errorf("%s: name: required, must match %s", origin, watchfile.NamePattern)}
	}
	if prev, dup := s.origins[k][name]; dup {
		return []error{fmt.Errorf("%s: name: duplicate %s %q (also at %s)", origin, Kinds()[k].Name, name, prev)}
	}
	body, err := json.Marshal(obj)
	if err != nil {
		return []error{fmt.Errorf("%s: %w", origin, err)}
	}
	o := Object{Name: name, Body: body, Origin: origin}
	var errs []error
	switch k {
	case Watch:
		o.Labels, errs = fileLabels(obj["labels"], origin+".labels")
	case Subscription:
		o.Sink, _ = obj["sink"].(string)
		if f, ok := obj["filter"].(map[string]any); ok {
			o.Labels, errs = fileLabels(f["labels"], origin+".filter.labels")
		}
	}
	if len(errs) > 0 {
		return errs
	}
	s.origins[k][name] = origin
	s.Objects[k] = append(s.Objects[k], o)
	return nil
}

// fileLabels reads a labels mapping of a file. A non-string value is an error
// here, so that the scope never silently misses a label.
func fileLabels(v any, origin string) (map[string]string, []error) {
	out := map[string]string{}
	if v == nil {
		return out, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, []error{fmt.Errorf("%s: must be a mapping of strings", origin)}
	}
	var errs []error
	for _, k := range sortedKeys(m) {
		s, ok := m[k].(string)
		if !ok {
			errs = append(errs, fmt.Errorf("%s.%s: must be a string (quote it)", origin, k))
			continue
		}
		out[k] = s
	}
	return out, errs
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
