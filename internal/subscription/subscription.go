package subscription

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
)

var (
	validBumps = []string{"major", "minor", "patch"}
	validTypes = []string{"release.published", "release.yanked", "release.prerelease"}
)

// Input is what a filter looks at. It is the minimal shape the future release
// event (#106) maps to; none of the fields needs I/O to fill.
type Input struct {
	// Source is the source kind of the watch ("npm", "pypi", ...), the
	// "ecosystem" of the PRD.
	Source string `json:"source"`
	// Package is the package name as the source normalised it ("@angular/core").
	Package string `json:"package"`
	// Labels are the labels of the watch that produced the event.
	Labels map[string]string `json:"labels"`
	// Bump is the classifier's result: "major", "minor", "patch",
	// "prerelease" or "other", or empty when unclassified. A filter with a
	// Bump list matches only a value in that list, so "prerelease", "other"
	// and "" never match it.
	Bump string `json:"bump"`
	// Type is the event type: "release.published", "release.yanked" or
	// "release.prerelease".
	Type string `json:"type"`
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

// Validate reports a filter that cannot match as intended: a bad glob, or a
// bump or type outside the values the watch file accepts.
func (f Filter) Validate() error {
	for _, b := range f.Bump {
		if !slices.Contains(validBumps, b) {
			return fmt.Errorf("subscription: unknown bump %q", b)
		}
	}
	for _, t := range f.Type {
		if !slices.Contains(validTypes, t) {
			return fmt.Errorf("subscription: unknown event type %q", t)
		}
	}
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
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return Filter{}, errors.New("subscription: trailing data after filter")
	}
	if err := f.Validate(); err != nil {
		return Filter{}, err
	}
	return f, nil
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

// NewMatcher builds a matcher over subs. Filters are deep-copied, so callers
// may reuse their slices and maps afterwards.
func NewMatcher(subs []Subscription) *Matcher {
	c := make([]Subscription, len(subs))
	for i, s := range subs {
		s.Filter.Source = slices.Clone(s.Filter.Source)
		s.Filter.Bump = slices.Clone(s.Filter.Bump)
		s.Filter.Type = slices.Clone(s.Filter.Type)
		s.Filter.Labels = maps.Clone(s.Filter.Labels)
		c[i] = s
	}
	return &Matcher{subs: c}
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
