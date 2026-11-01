package subscription

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"slices"

	"github.com/angelia-dev/angelia/internal/watchfile"
)

// Input is what a filter looks at. It is the minimal shape the future release
// event (#106) maps to; none of the fields needs I/O to fill.
type Input struct {
	// Source is the source kind of the watch ("npm", "pypi", ...), the
	// "ecosystem" of the PRD.
	Source string
	// Package is the package name as the source normalised it ("@angular/core").
	Package string
	// Labels are the labels of the watch that produced the event.
	Labels map[string]string
	// Bump is "major", "minor" or "patch"; empty when the change has no
	// classification (first version, scheme change). A filter with a Bump
	// list never matches an empty bump.
	Bump string
	// Type is the event type: "release.published", "release.yanked" or
	// "release.prerelease".
	Type string
}

// Filter selects events. Every non-empty field must match. The zero value
// matches everything. Field names and meaning follow the watch file.
type Filter struct {
	// Source lists accepted source kinds; empty means any.
	Source []string `json:"source"`
	// Labels must all be present on the input with equal values.
	Labels map[string]string `json:"labels"`
	// Bump lists accepted bumps; empty means any.
	Bump []string `json:"bump"`
	// Package is a path.Match glob on the package name; empty means any.
	// The match is case-sensitive and "*" does not cross "/", so "@angular/*"
	// matches "@angular/core" but not "@angular/core/testing".
	Package string `json:"package"`
	// Type lists accepted event types; empty means any. The watch-file loader
	// fills in [release.published] when the file omits it, so a stored filter
	// normally has it set; this package adds no default.
	Type []string `json:"type"`
}

// Match reports whether the filter accepts the input. It is pure.
func (f Filter) Match(in Input) bool {
	if len(f.Source) > 0 && !slices.Contains(f.Source, in.Source) {
		return false
	}
	if len(f.Type) > 0 && !slices.Contains(f.Type, in.Type) {
		return false
	}
	if len(f.Bump) > 0 && !slices.Contains(f.Bump, in.Bump) {
		return false
	}
	for k, v := range f.Labels {
		if got, ok := in.Labels[k]; !ok || got != v {
			return false
		}
	}
	if f.Package != "" {
		// A malformed pattern (rejected at load time) matches nothing.
		if ok, err := path.Match(f.Package, in.Package); err != nil || !ok {
			return false
		}
	}
	return true
}

// Validate reports a filter that cannot match as intended (a bad glob).
func (f Filter) Validate() error {
	if _, err := path.Match(f.Package, ""); err != nil {
		return fmt.Errorf("subscription: package glob %q: %w", f.Package, err)
	}
	return nil
}

// FromJSON decodes a stored filter (subscription.filter_json). Unknown keys
// are errors, so a typo is not silently a match-everything filter. Empty or
// "null" input yields the zero (match-all) filter.
func FromJSON(b []byte) (Filter, error) {
	var f Filter
	if len(bytes.TrimSpace(b)) == 0 {
		return f, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return Filter{}, fmt.Errorf("subscription: decode filter: %w", err)
	}
	if err := f.Validate(); err != nil {
		return Filter{}, err
	}
	return f, nil
}

// FromWatchFile converts a parsed watch-file filter.
func FromWatchFile(w watchfile.Filter) Filter {
	return Filter{Source: w.Source, Labels: w.Labels, Bump: w.Bump, Package: w.Package, Type: w.Type}
}

// Subscription is a filter with the id of the subscription it belongs to.
type Subscription struct {
	ID     string
	Filter Filter
}

// Matcher holds a set of subscriptions and finds those accepting an input.
// It is immutable after construction and safe for concurrent use.
type Matcher struct {
	subs []Subscription
}

// NewMatcher builds a matcher over subs (the slice is copied).
func NewMatcher(subs []Subscription) *Matcher {
	return &Matcher{subs: slices.Clone(subs)}
}

// Len returns the number of subscriptions.
func (m *Matcher) Len() int { return len(m.subs) }

// Match appends to dst the ids of the subscriptions accepting in, in the
// order they were given, and returns the extended slice.
func (m *Matcher) Match(in Input, dst []string) []string {
	for i := range m.subs {
		if m.subs[i].Filter.Match(in) {
			dst = append(dst, m.subs[i].ID)
		}
	}
	return dst
}
