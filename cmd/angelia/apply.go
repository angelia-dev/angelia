package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/angelia-dev/angelia/internal/apiclient"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

// `angelia apply` (story #73) sends the objects of watch files (the Phase 0
// format of docs/watch-file.md, unchanged) to the API: one PUT per object in
// dependency order (sinks, watches, subscriptions), then, with --prune, one
// DELETE per object out of the file and in the label scope, in reverse order.
// The sequence is not atomic (spec docs/api.md, decision 5).

// The kinds apply handles, in dependency order.
const (
	kSink = iota
	kWatch
	kSub
	numKinds
)

// applyKind is what apply needs to know of a resource type.
type applyKind struct {
	name    string // in the report: sink, watch, subscription
	section string // the key of the watch file
	// writeKeys are the keys of the *Write schema: the ones a diff compares.
	writeKeys []string
	put       func(ctx context.Context, a *apiclient.API, name string, dry *bool, body io.Reader) (*http.Response, error)
	del       func(ctx context.Context, a *apiclient.API, name string) (*http.Response, error)
	list      func(ctx context.Context, a *apiclient.API, cursor *string) (*http.Response, error)
}

// listLimit is the largest page the API serves.
var listLimit = 500

var applyKinds = [numKinds]applyKind{
	kSink: {
		name: "sink", section: "sinks", writeKeys: []string{"kind", "config"},
		put: func(ctx context.Context, a *apiclient.API, n string, dry *bool, b io.Reader) (*http.Response, error) {
			return a.PutSinkWithBody(ctx, n, &apiclient.PutSinkParams{DryRun: dry}, jsonContentType, b)
		},
		del: func(ctx context.Context, a *apiclient.API, n string) (*http.Response, error) {
			return a.DeleteSink(ctx, n, &apiclient.DeleteSinkParams{})
		},
		list: func(ctx context.Context, a *apiclient.API, cursor *string) (*http.Response, error) {
			return a.ListSinks(ctx, &apiclient.ListSinksParams{Cursor: cursor, Limit: &listLimit})
		},
	},
	kWatch: {
		name: "watch", section: "watches", writeKeys: []string{"source", "coordinates", "interval", "scheme", "labels", "policy"},
		put: func(ctx context.Context, a *apiclient.API, n string, dry *bool, b io.Reader) (*http.Response, error) {
			return a.PutWatchWithBody(ctx, n, &apiclient.PutWatchParams{DryRun: dry}, jsonContentType, b)
		},
		del: func(ctx context.Context, a *apiclient.API, n string) (*http.Response, error) {
			return a.DeleteWatch(ctx, n, &apiclient.DeleteWatchParams{})
		},
		list: func(ctx context.Context, a *apiclient.API, cursor *string) (*http.Response, error) {
			return a.ListWatches(ctx, &apiclient.ListWatchesParams{Cursor: cursor, Limit: &listLimit})
		},
	},
	kSub: {
		name: "subscription", section: "subscriptions", writeKeys: []string{"sink", "filter", "policy", "template"},
		put: func(ctx context.Context, a *apiclient.API, n string, dry *bool, b io.Reader) (*http.Response, error) {
			return a.PutSubscriptionWithBody(ctx, n, &apiclient.PutSubscriptionParams{DryRun: dry}, jsonContentType, b)
		},
		del: func(ctx context.Context, a *apiclient.API, n string) (*http.Response, error) {
			return a.DeleteSubscription(ctx, n, &apiclient.DeleteSubscriptionParams{})
		},
		list: func(ctx context.Context, a *apiclient.API, cursor *string) (*http.Response, error) {
			return a.ListSubscriptions(ctx, &apiclient.ListSubscriptionsParams{Cursor: cursor, Limit: &listLimit})
		},
	},
}

// applyNameRE is the name pattern of the watch file and the API.
var applyNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// applyObject is one sink, watch or subscription read from a file.
type applyObject struct {
	name   string
	body   []byte // the object as JSON: the PUT body
	origin string // file and position, for messages
	labels map[string]string
	sink   string // subscriptions: the sink's name
}

// applySet is everything read from the files, per kind in file order.
type applySet struct {
	objects [numKinds][]applyObject
	origins [numKinds]map[string]string // name -> origin
}

func (s *applySet) has(k int, name string) bool {
	_, ok := s.origins[k][name]
	return ok
}

// readApplySet reads every path (a file, a directory of .yaml, .yml and .json
// files, or "-" for stdin). Every problem is reported, before any request.
func readApplySet(in io.Reader, paths []string) (*applySet, error) {
	set := &applySet{}
	for k := range set.origins {
		set.origins[k] = map[string]string{}
	}
	var errs []error
	for _, p := range paths {
		files, err := applyFiles(p)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			raw, err := readLimitedN(in, f, watchfile.MaxFileSize)
			if err != nil {
				return nil, err
			}
			label := f
			if f == "-" {
				label = "stdin"
			}
			docs, err := decodeDocuments(raw)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", label, err))
				continue
			}
			if len(docs) == 0 {
				errs = append(errs, fmt.Errorf("%s: empty: a watch file needs at least \"version: %d\"", label, watchfile.Version))
			}
			for i, doc := range docs {
				origin := label
				if len(docs) > 1 {
					origin = fmt.Sprintf("%s (document %d)", label, i+1)
				}
				errs = append(errs, set.addDocument(doc, origin)...)
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return set, nil
}

// applyFiles expands a -f value: a directory gives its .yaml, .yml and .json
// files (not recursive), sorted by name.
func applyFiles(p string) ([]string, error) {
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

// addDocument adds the objects of one watch file document (docs/watch-file.md
// section 2). Only the structure is checked here; the server validates each
// object by the rules of the watch file.
func (s *applySet) addDocument(doc any, origin string) []error {
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
	for k := range applyKinds {
		sec := applyKinds[k].section
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
			if err := s.addObject(k, item, fmt.Sprintf("%s: %s[%d]", origin, sec, i)); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errs
}

func (s *applySet) addObject(k int, item any, origin string) error {
	obj, ok := item.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: must be an object", origin)
	}
	name, _ := obj["name"].(string)
	if !applyNameRE.MatchString(name) {
		return fmt.Errorf("%s: name: required, must match %s", origin, applyNameRE)
	}
	if prev, dup := s.origins[k][name]; dup {
		return fmt.Errorf("%s: name: duplicate %s %q (also at %s)", origin, applyKinds[k].name, name, prev)
	}
	body, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("%s: %w", origin, err)
	}
	o := applyObject{name: name, body: body, origin: origin}
	switch k {
	case kWatch:
		o.labels = stringMap(obj["labels"])
	case kSub:
		o.sink, _ = obj["sink"].(string)
		if f, ok := obj["filter"].(map[string]any); ok {
			o.labels = stringMap(f["labels"])
		}
	}
	s.origins[k][name] = origin
	s.objects[k] = append(s.objects[k], o)
	return nil
}

// stringMap keeps the string values of a decoded object.
func stringMap(v any) map[string]string {
	m, _ := v.(map[string]any)
	out := map[string]string{}
	for k, x := range m {
		if s, ok := x.(string); ok {
			out[k] = s
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// parseSelector parses the -l values: key=value pairs that must all match.
func parseSelector(vals []string) (map[string]string, error) {
	sel := map[string]string{}
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

// matches reports whether labels carry every pair of sel.
func matches(labels, sel map[string]string) bool {
	for k, v := range sel {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// Changes in the report.
const (
	changeCreated   = "created"
	changeUpdated   = "updated"
	changeUnchanged = "unchanged"
	changeDeleted   = "deleted"
	changeFailed    = "failed"
)

// applyChange is one line of the report.
type applyChange struct {
	Kind   string      `json:"kind"`
	Name   string      `json:"name"`
	Change string      `json:"change"`
	Diff   []fieldDiff `json:"diff,omitempty"`
	Note   string      `json:"note,omitempty"`
	Error  string      `json:"error,omitempty"`
}

// fieldDiff is one changed write key, as a dotted path; null is absent.
type fieldDiff struct {
	Path string `json:"path"`
	Old  any    `json:"old"`
	New  any    `json:"new"`
}

type applyReport struct {
	DryRun  bool          `json:"dry_run"`
	Changes []applyChange `json:"changes"`
}

// applyRun is one execution of apply.
type applyRun struct {
	e       *cliEnv
	dry     bool
	prune   bool
	sel     map[string]string
	current [numKinds]map[string]map[string]any // name -> resource, as the API returned it
	created map[string]bool                     // sinks created by this run (dry run)
	report  applyReport
}

type applyFlags struct {
	files    []string
	dryRun   bool
	prune    bool
	selector []string
}

func newApplyCmd() *cobra.Command {
	var f applyFlags
	cmd := &cobra.Command{
		Use:   "apply -f FILE|DIR|- [--dry-run] [--prune -l key=value]",
		Short: "Create, update and prune watches, sinks and subscriptions from watch files",
		Long: `Apply sends every sink, watch and subscription of the watch files (docs/watch-file.md)
to the API with PUT, in that order, and reports what changed. Applying the same files
twice changes nothing. --dry-run asks the server to validate each object and prints
the plan without writing. --prune -l key=value also deletes the objects managed by the
API that are not in the files and are in the label scope (docs/cli.md, "Apply").`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sel, err := parseSelector(f.selector)
			if err != nil {
				return err
			}
			switch {
			case f.prune && len(sel) == 0:
				return errors.New("--prune needs a label scope: -l key=value")
			case !f.prune && len(sel) > 0:
				return errors.New("-l scopes --prune: add --prune or remove -l")
			}
			set, err := readApplySet(cmd.InOrStdin(), f.files)
			if err != nil {
				return err
			}
			e, err := newCLIEnv(cmd)
			if err != nil {
				return err
			}
			r := &applyRun{e: e, dry: f.dryRun, prune: f.prune, sel: sel, created: map[string]bool{}}
			r.report.DryRun = f.dryRun
			r.report.Changes = []applyChange{}
			runErr := r.run(cmd.Context(), set)
			if err := r.print(); err != nil {
				return err
			}
			return runErr
		},
	}
	addClientFlags(cmd)
	cmd.Flags().StringArrayVarP(&f.files, "file", "f", nil, "watch file, directory of watch files, or - for stdin (repeatable)")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "validate every object and print the plan without writing")
	cmd.Flags().BoolVar(&f.prune, "prune", false, "delete API objects in the -l scope that are not in the files")
	cmd.Flags().StringArrayVarP(&f.selector, "selector", "l", nil, "label scope of --prune, as key=value (repeatable; all must match)")
	_ = cmd.MarkFlagRequired("file")
	_ = cmd.MarkFlagFilename("file", "yaml", "yml", "json")
	return cmd
}

// run applies set. A real run stops at the first failure; a dry run reports
// every failure and goes on.
func (r *applyRun) run(ctx context.Context, set *applySet) error {
	if r.prune {
		r.warnOutOfScope(set)
	}
	if err := r.load(ctx); err != nil {
		return err
	}
	for k := range applyKinds {
		for _, o := range set.objects[k] {
			if err := r.put(ctx, k, o); err != nil {
				return err
			}
		}
	}
	if r.prune {
		dels := r.pruneSet(set)
		for k := numKinds - 1; k >= 0; k-- {
			for _, name := range dels[k] {
				if err := r.delete(ctx, k, name); err != nil {
					return err
				}
			}
		}
	}
	if n := r.count(changeFailed); n > 0 {
		return fmt.Errorf("%d of %d objects failed", n, len(r.report.Changes))
	}
	return nil
}

// warnOutOfScope warns about file objects a later prune with the same scope
// would not see: they are applied, but never pruned.
func (r *applyRun) warnOutOfScope(set *applySet) {
	for _, k := range []int{kWatch, kSub} {
		for _, o := range set.objects[k] {
			if !matches(o.labels, r.sel) {
				what := "labels"
				if k == kSub {
					what = "filter.labels"
				}
				_, _ = fmt.Fprintf(r.e.Err, "warning: %s: %s %q: its %s lack %s, so --prune with this scope will never delete it\n",
					o.origin, applyKinds[k].name, o.name, what, kv(r.sel))
			}
		}
	}
}

// load lists every sink, watch and subscription the server has.
func (r *applyRun) load(ctx context.Context) error {
	for k := range applyKinds {
		ak := &applyKinds[k]
		fetch := func(ctx context.Context, cursor *string) ([]map[string]any, *string, error) {
			var out struct {
				Items      []map[string]any `json:"items"`
				NextCursor *string          `json:"next_cursor"`
			}
			resp, err := ak.list(ctx, r.e.API, cursor) //nolint:bodyclose // Decode closes it
			_, err = apiclient.Decode(resp, err, &out)
			return out.Items, out.NextCursor, err
		}
		p, err := collect(ctx, fetch, "", true)
		if err != nil {
			return fmt.Errorf("list %ss: %w", ak.name, err)
		}
		r.current[k] = make(map[string]map[string]any, len(p.Items))
		for _, it := range p.Items {
			if n, ok := it["name"].(string); ok {
				r.current[k][n] = it
			}
		}
	}
	return nil
}

func (r *applyRun) add(c applyChange) { r.report.Changes = append(r.report.Changes, c) }

func (r *applyRun) count(change string) int {
	n := 0
	for _, c := range r.report.Changes {
		if c.Change == change {
			n++
		}
	}
	return n
}

// put sends one object. It returns an error only to stop a real run.
func (r *applyRun) put(ctx context.Context, k int, o applyObject) error {
	ak := &applyKinds[k]
	c := applyChange{Kind: ak.name, Name: o.name}
	if r.dry && k == kSub && r.created[o.sink] {
		// The server cannot check a reference to a sink that the dry run
		// did not create.
		c.Change = changeCreated
		if _, ok := r.current[k][o.name]; ok {
			c.Change = changeUpdated
		}
		c.Note = fmt.Sprintf("sink %q is created by this apply; not validated by the server", o.sink)
		r.add(c)
		return nil
	}
	var got map[string]any
	resp, err := ak.put(ctx, r.e.API, o.name, dryRunPtr(r.dry), bytes.NewReader(o.body)) //nolint:bodyclose // Decode closes it
	res, err := apiclient.Decode(resp, err, &got)
	if err != nil {
		c.Change, c.Error = changeFailed, err.Error()
		r.add(c)
		if r.dry {
			return nil
		}
		return fmt.Errorf("apply stopped at %s %q (%s); the objects before it were applied", ak.name, o.name, o.origin)
	}
	old, existed := r.current[k][o.name]
	c.Change = res.Change
	if c.Change == "" { // a server that does not send Angelia-Change
		c.Change = changeCreated
		if existed {
			c.Change = changeUpdated
		}
	}
	if existed && c.Change == changeUpdated {
		c.Diff = diffWriteKeys(old, got, ak.writeKeys)
		if len(c.Diff) == 0 {
			c.Note = "no difference in the fields the API shows"
		}
	}
	if k == kSink && c.Change == changeCreated {
		r.created[o.name] = true
	}
	r.add(c)
	return nil
}

// pruneSet returns the names to delete per kind, sorted. The scope, since
// only watches carry labels (spec v1):
//   - a watch: managed by the API, not in the files, its labels match;
//   - a subscription: managed by the API, not in the files, its
//     filter.labels match (it can only route events of watches in the scope);
//   - a sink: managed by the API, not in the files, used before the apply by
//     at least one subscription in the scope, and by no subscription after it.
//
// Objects managed by the watch file are never pruned.
func (r *applyRun) pruneSet(set *applySet) [numKinds][]string {
	var dels [numKinds][]string
	byAPI := func(m map[string]any) bool { return m["managed_by"] == "api" }
	subInScope := func(m map[string]any) bool {
		f, _ := m["filter"].(map[string]any)
		return byAPI(m) && matches(stringMap(f["labels"]), r.sel)
	}
	for name, w := range r.current[kWatch] {
		if byAPI(w) && !set.has(kWatch, name) && matches(stringMap(w["labels"]), r.sel) {
			dels[kWatch] = append(dels[kWatch], name)
		}
	}
	usedInScope, usedAfter := map[string]bool{}, map[string]bool{}
	for _, o := range set.objects[kSub] {
		usedAfter[o.sink] = true
	}
	for name, s := range r.current[kSub] {
		sink, _ := s["sink"].(string)
		switch {
		case set.has(kSub, name):
			// its sink after the apply is the file's, counted above
		case subInScope(s):
			dels[kSub] = append(dels[kSub], name)
		default:
			usedAfter[sink] = true
		}
		if subInScope(s) {
			usedInScope[sink] = true
		}
	}
	for name, k := range r.current[kSink] {
		if byAPI(k) && !set.has(kSink, name) && usedInScope[name] && !usedAfter[name] {
			dels[kSink] = append(dels[kSink], name)
		}
	}
	for k := range dels {
		sort.Strings(dels[k])
	}
	return dels
}

// delete removes one object. A dry run only reports it: the server would
// refuse a dry-run delete of a sink whose subscriptions are deleted in the
// same run.
func (r *applyRun) delete(ctx context.Context, k int, name string) error {
	ak := &applyKinds[k]
	c := applyChange{Kind: ak.name, Name: name, Change: changeDeleted}
	if r.dry {
		r.add(c)
		return nil
	}
	resp, err := ak.del(ctx, r.e.API, name) //nolint:bodyclose // Decode closes it
	if _, err := apiclient.Decode(resp, err, nil); err != nil {
		var ae *apiclient.Error
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return nil // deleted meanwhile
		}
		c.Change, c.Error = changeFailed, err.Error()
		r.add(c)
		return fmt.Errorf("prune stopped at %s %q", ak.name, name)
	}
	r.add(c)
	return nil
}

// diffWriteKeys compares the write keys of two resources, flattened to
// dotted paths.
func diffWriteKeys(old, cur map[string]any, keys []string) []fieldDiff {
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
	var out []fieldDiff
	for _, p := range paths {
		if !jsonSame(a[p], b[p]) {
			out = append(out, fieldDiff{Path: p, Old: a[p], New: b[p]})
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

func jsonSame(a, b any) bool { return compactJSON(a) == compactJSON(b) }

// compactJSON renders a value of the report; an absent one is "(none)".
func compactJSON(v any) string {
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

// print writes the report: JSON, or one line per object with its diff and a
// summary.
func (r *applyRun) print() error {
	if r.e.Format == formatJSON {
		return writeJSON(r.e.Out, r.report)
	}
	var b strings.Builder
	for _, c := range r.report.Changes {
		fmt.Fprintf(&b, "%s/%s %s", c.Kind, c.Name, c.Change)
		if c.Note != "" {
			fmt.Fprintf(&b, " (%s)", c.Note)
		}
		b.WriteString("\n")
		for _, d := range c.Diff {
			fmt.Fprintf(&b, "    %s: %s -> %s\n", cell(d.Path), cell(compactJSON(d.Old)), cell(compactJSON(d.New)))
		}
		if c.Error != "" {
			b.WriteString("    " + strings.ReplaceAll(c.Error, "\n", "\n    ") + "\n")
		}
	}
	parts := []string{}
	for _, ch := range []string{changeCreated, changeUpdated, changeUnchanged, changeDeleted, changeFailed} {
		if n := r.count(ch); n > 0 || ch != changeFailed {
			parts = append(parts, fmt.Sprintf("%d %s", n, ch))
		}
	}
	b.WriteString(strings.Join(parts, ", "))
	if r.dry {
		b.WriteString(" (dry run: nothing was written)")
	}
	b.WriteString("\n")
	_, err := io.WriteString(r.e.Out, b.String())
	return err
}
