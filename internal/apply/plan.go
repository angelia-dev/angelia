package apply

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Selector is the label scope of --prune: every pair must match.
type Selector map[string]string

// ParseSelector parses -l values, each one key=value.
func ParseSelector(vals []string) (Selector, error) {
	sel := Selector{}
	for _, v := range vals {
		k, val, ok := strings.Cut(v, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("-l %q: want key=value", v)
		}
		if prev, dup := sel[k]; dup && prev != val {
			return nil, fmt.Errorf("-l: label %q given twice with different values", k)
		}
		sel[k] = val
	}
	return sel, nil
}

// Matches reports whether labels carry every pair of the selector.
func (s Selector) Matches(labels map[string]string) bool {
	for k, v := range s {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// String formats the selector as sorted key=value pairs.
func (s Selector) String() string {
	parts := make([]string, 0, len(s))
	for _, k := range sortedKeys(s) {
		parts = append(parts, k+"="+s[k])
	}
	return strings.Join(parts, ",")
}

// WatchesInScope counts the file's watches whose labels match sel.
func (s *Set) WatchesInScope(sel Selector) int {
	n := 0
	for _, o := range s.Objects[Watch] {
		if sel.Matches(o.Labels) {
			n++
		}
	}
	return n
}

// OutOfScope returns a warning for every watch in the files that lacks the
// scope's labels: it is applied, but a prune with this scope never deletes it.
func (s *Set) OutOfScope(sel Selector) []string {
	var out []string
	for _, o := range s.Objects[Watch] {
		if !sel.Matches(o.Labels) {
			out = append(out, fmt.Sprintf("%s: watch %q: its labels lack %s, so --prune with this scope will never delete it", o.Origin, o.Name, sel))
		}
	}
	return out
}

// Resource is a sink, watch or subscription as the API returned it.
type Resource = map[string]any

// Current is what the server has, per kind: name -> resource.
type Current [NumKinds]map[string]Resource

// Labels returns the string values of a decoded labels object.
func Labels(v any) map[string]string {
	m, _ := v.(map[string]any)
	out := map[string]string{}
	for k, x := range m {
		if s, ok := x.(string); ok {
			out[k] = s
		}
	}
	return out
}

func byAPI(r Resource) bool { return r["managed_by"] == "api" }

// Prunable reports whether a watch may be pruned: managed by the API and in
// the scope. Objects managed by the watch file are never pruned.
func Prunable(w Resource, sel Selector) bool {
	return byAPI(w) && sel.Matches(Labels(w["labels"]))
}

// PruneWatches returns the sorted names of the watches --prune deletes: in the
// scope (Prunable) and not in the files. Only watches carry labels in API v1,
// so only watches are pruned.
func PruneWatches(cur Current, set *Set, sel Selector) []string {
	var out []string
	for name, w := range cur[Watch] {
		if !set.Has(Watch, name) && Prunable(w, sel) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Kept lists the subscriptions and sinks a prune leaves alone although they
// may be left over, each with a note on how to delete it by hand:
//   - an API subscription not in the files whose filter.labels match the
//     scope (filter.labels select events; they do not say who owns it);
//   - an API sink not in the files that no subscription uses after the apply.
func Kept(cur Current, set *Set, sel Selector) []Change {
	var out []Change
	used := map[string]bool{}
	for _, o := range set.Objects[Subscription] {
		used[o.Sink] = true
	}
	for _, name := range sortedKeys(cur[Subscription]) {
		s := cur[Subscription][name]
		if set.Has(Subscription, name) {
			continue
		}
		if sink, ok := s["sink"].(string); ok {
			used[sink] = true
		}
		f, _ := s["filter"].(map[string]any)
		if byAPI(s) && sel.Matches(Labels(f["labels"])) {
			out = append(out, Change{
				Kind: "subscription", Name: name, Change: ChangeKept,
				Note: fmt.Sprintf("not pruned: subscriptions carry no owner labels in API v1; if it is yours, run: angelia sub delete %s", name),
			})
		}
	}
	for _, name := range sortedKeys(cur[Sink]) {
		if k := cur[Sink][name]; byAPI(k) && !set.Has(Sink, name) && !used[name] {
			out = append(out, Change{
				Kind: "sink", Name: name, Change: ChangeKept,
				Note: fmt.Sprintf("not pruned: no subscription uses it, and sinks carry no owner labels in API v1; if it is yours, run: angelia sink delete %s", name),
			})
		}
	}
	return out
}

// Changes in the report.
const (
	ChangeCreated   = "created"
	ChangeUpdated   = "updated"
	ChangeUnchanged = "unchanged"
	ChangeDeleted   = "deleted"
	ChangeKept      = "kept"    // a prune candidate left alone
	ChangeSkipped   = "skipped" // not deleted because the prune stopped
	ChangeFailed    = "failed"
)

// Change is one line of the report.
type Change struct {
	Kind   string      `json:"kind"`
	Name   string      `json:"name"`
	Change string      `json:"change"`
	Diff   []FieldDiff `json:"diff,omitempty"`
	Note   string      `json:"note,omitempty"`
	Error  string      `json:"error,omitempty"`
}

// FieldDiff is one changed write key, as a dotted path; null is absent.
type FieldDiff struct {
	Path string `json:"path"`
	Old  any    `json:"old"`
	New  any    `json:"new"`
}

// Report is the result of a run.
type Report struct {
	DryRun  bool     `json:"dry_run"`
	Changes []Change `json:"changes"`
}

// Count returns the number of changes of one kind.
func (r *Report) Count(change string) int {
	n := 0
	for _, c := range r.Changes {
		if c.Change == change {
			n++
		}
	}
	return n
}

// Summary is the count line: created, updated, unchanged and deleted always,
// the others when they occur.
func (r *Report) Summary() string {
	var parts []string
	order := []string{ChangeCreated, ChangeUpdated, ChangeUnchanged, ChangeDeleted, ChangeKept, ChangeSkipped, ChangeFailed}
	for i, ch := range order {
		if n := r.Count(ch); n > 0 || i < 4 {
			parts = append(parts, fmt.Sprintf("%d %s", n, ch))
		}
	}
	s := strings.Join(parts, ", ")
	if r.DryRun {
		s += " (dry run: nothing was written)"
	}
	return s
}

// DiffWriteKeys compares the write keys of two resources, flattened to
// dotted paths and sorted.
func DiffWriteKeys(old, cur Resource, keys []string) []FieldDiff {
	a, b := map[string]any{}, map[string]any{}
	for _, k := range keys {
		flatten(a, k, old[k])
		flatten(b, k, cur[k])
	}
	paths := sortedKeys(a)
	for p := range b {
		if _, ok := a[p]; !ok {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	var out []FieldDiff
	for _, p := range paths {
		if CompactJSON(a[p]) != CompactJSON(b[p]) {
			out = append(out, FieldDiff{Path: p, Old: a[p], New: b[p]})
		}
	}
	return out
}

// flatten stores the leaves of v under dotted paths. A null leaf is absent.
func flatten(dst map[string]any, path string, v any) {
	if m, ok := v.(map[string]any); ok {
		for _, k := range sortedKeys(m) {
			flatten(dst, path+"."+k, m[k])
		}
		return
	}
	if v != nil {
		dst[path] = v
	}
}

// CompactJSON renders a value of the report; an absent one is "(none)".
func CompactJSON(v any) string {
	if v == nil {
		return "(none)"
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Sprint(v)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}
