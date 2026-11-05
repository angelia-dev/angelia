package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	"github.com/angelia-dev/angelia/internal/apiclient"
	"github.com/angelia-dev/angelia/internal/apply"
)

// `angelia apply` (story #73) sends the objects of watch files (the format of
// docs/watch-file.md, unchanged) to the API: one PUT per object in dependency
// order (sinks, watches, subscriptions), then, with --prune, one DELETE per
// watch out of the files and in the label scope. The sequence is not atomic
// (spec docs/api.md, decision 5). The plan logic is in internal/apply; this
// file sends the requests and prints the report.

// applyListLimit is the largest page the API serves.
const applyListLimit = 500

// kindOps are the API calls of one resource type.
type kindOps struct {
	put  func(ctx context.Context, a *apiclient.API, name string, dry *bool, body io.Reader) (*http.Response, error)
	list func(ctx context.Context, a *apiclient.API, cursor *string) (*http.Response, error)
}

func applyOps() [apply.NumKinds]kindOps {
	return [apply.NumKinds]kindOps{
		apply.Sink: {
			put: func(ctx context.Context, a *apiclient.API, n string, dry *bool, b io.Reader) (*http.Response, error) {
				return a.PutSinkWithBody(ctx, n, &apiclient.PutSinkParams{DryRun: dry}, jsonContentType, b)
			},
			list: func(ctx context.Context, a *apiclient.API, cursor *string) (*http.Response, error) {
				limit := applyListLimit
				return a.ListSinks(ctx, &apiclient.ListSinksParams{Cursor: cursor, Limit: &limit})
			},
		},
		apply.Watch: {
			put: func(ctx context.Context, a *apiclient.API, n string, dry *bool, b io.Reader) (*http.Response, error) {
				return a.PutWatchWithBody(ctx, n, &apiclient.PutWatchParams{DryRun: dry}, jsonContentType, b)
			},
			list: func(ctx context.Context, a *apiclient.API, cursor *string) (*http.Response, error) {
				limit := applyListLimit
				return a.ListWatches(ctx, &apiclient.ListWatchesParams{Cursor: cursor, Limit: &limit})
			},
		},
		apply.Subscription: {
			put: func(ctx context.Context, a *apiclient.API, n string, dry *bool, b io.Reader) (*http.Response, error) {
				return a.PutSubscriptionWithBody(ctx, n, &apiclient.PutSubscriptionParams{DryRun: dry}, jsonContentType, b)
			},
			list: func(ctx context.Context, a *apiclient.API, cursor *string) (*http.Response, error) {
				limit := applyListLimit
				return a.ListSubscriptions(ctx, &apiclient.ListSubscriptionsParams{Cursor: cursor, Limit: &limit})
			},
		},
	}
}

// applyRun is one execution of apply.
type applyRun struct {
	e       *cliEnv
	dry     bool
	prune   bool
	sel     apply.Selector
	kinds   [apply.NumKinds]apply.KindInfo
	ops     [apply.NumKinds]kindOps
	current apply.Current
	created map[string]bool // sinks created by this run (dry run)
	report  apply.Report
}

type applyFlags struct {
	files      []string
	dryRun     bool
	prune      bool
	allowEmpty bool
	selector   []string
}

func newApplyCmd() *cobra.Command {
	var f applyFlags
	cmd := &cobra.Command{
		Use:   "apply -f FILE|DIR|- [--dry-run] [--prune -l key=value]",
		Short: "Create, update and prune watches, sinks and subscriptions from watch files",
		Long: `Apply sends every sink, watch and subscription of the watch files (docs/watch-file.md)
to the API with PUT, in that order, and reports what changed. Applying the same files
twice changes nothing. --dry-run asks the server to validate each object and prints
the plan without writing. --prune -l key=value also deletes the watches managed by the
API that carry those labels and are not in the files (docs/cli.md, "Apply").`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sel, err := apply.ParseSelector(f.selector)
			if err != nil {
				return err
			}
			switch {
			case f.prune && len(sel) == 0:
				return errors.New("--prune needs a label scope: -l key=value")
			case !f.prune && len(sel) > 0:
				return errors.New("-l scopes --prune: add --prune or remove -l")
			case f.allowEmpty && !f.prune:
				return errors.New("--prune-allow-empty only makes sense with --prune")
			}
			set, err := apply.Read(cmd.InOrStdin(), f.files)
			if err != nil {
				return err
			}
			if f.prune && set.WatchesInScope(sel) == 0 && !f.allowEmpty {
				msg := fmt.Sprintf("the files hold no watch with the labels %s, so --prune would delete every API watch in that scope; check -f and -l, or pass --prune-allow-empty", sel)
				if !f.dryRun {
					return errors.New("refusing to prune: " + msg)
				}
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning: "+msg)
			}
			e, err := newCLIEnv(cmd)
			if err != nil {
				return err
			}
			r := &applyRun{
				e: e, dry: f.dryRun, prune: f.prune, sel: sel, kinds: apply.Kinds(), ops: applyOps(), created: map[string]bool{},
				report: apply.Report{DryRun: f.dryRun, Changes: []apply.Change{}},
			}
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
	cmd.Flags().BoolVar(&f.prune, "prune", false, "delete API watches in the -l scope that are not in the files")
	cmd.Flags().BoolVar(&f.allowEmpty, "prune-allow-empty", false, "prune even when the files hold no watch in the scope")
	cmd.Flags().StringArrayVarP(&f.selector, "selector", "l", nil, "label scope of --prune, as key=value (repeatable; all must match)")
	_ = cmd.MarkFlagRequired("file")
	_ = cmd.MarkFlagFilename("file", "yaml", "yml", "json")
	return cmd
}

// run applies set. A real run stops at the first failure and then does not
// prune; a dry run reports every failure and goes on.
func (r *applyRun) run(ctx context.Context, set *apply.Set) error {
	if r.prune {
		for _, w := range set.OutOfScope(r.sel) {
			_, _ = fmt.Fprintln(r.e.Err, "warning: "+w)
		}
	}
	if err := r.load(ctx); err != nil {
		return err
	}
	for k := range r.kinds {
		for _, o := range set.Objects[k] {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("apply interrupted before %s %q: %w", r.kinds[k].Name, o.Name, err)
			}
			if err := r.put(ctx, k, o); err != nil {
				return err
			}
		}
	}
	if r.prune {
		err := r.pruneWatches(ctx, apply.PruneWatches(r.current, set, r.sel))
		r.report.Changes = append(r.report.Changes, apply.Kept(r.current, set, r.sel)...)
		if err != nil {
			return err
		}
	}
	if n := r.report.Count(apply.ChangeFailed); n > 0 {
		return fmt.Errorf("%d of %d objects failed", n, len(r.report.Changes))
	}
	return nil
}

// load lists every sink, watch and subscription the server has.
func (r *applyRun) load(ctx context.Context) error {
	for k := range r.kinds {
		list := r.ops[k].list
		fetch := func(ctx context.Context, cursor *string) ([]apply.Resource, *string, error) {
			var out struct {
				Items      []apply.Resource `json:"items"`
				NextCursor *string          `json:"next_cursor"`
			}
			resp, err := list(ctx, r.e.API, cursor) //nolint:bodyclose // Decode closes it
			_, err = apiclient.Decode(resp, err, &out)
			return out.Items, out.NextCursor, err
		}
		p, err := collect(ctx, fetch, "", true)
		if err != nil {
			return fmt.Errorf("list %ss: %w", r.kinds[k].Name, err)
		}
		r.current[k] = make(map[string]apply.Resource, len(p.Items))
		for _, it := range p.Items {
			if n, ok := it["name"].(string); ok {
				r.current[k][n] = it
			}
		}
	}
	return nil
}

func (r *applyRun) add(c apply.Change) { r.report.Changes = append(r.report.Changes, c) }

// put sends one object. It returns an error only to stop a real run.
func (r *applyRun) put(ctx context.Context, k int, o apply.Object) error {
	kind := r.kinds[k]
	c := apply.Change{Kind: kind.Name, Name: o.Name}
	if r.dry && k == apply.Subscription && r.created[o.Sink] {
		// The server cannot check a reference to a sink that the dry run
		// did not create.
		c.Change = apply.ChangeCreated
		if _, ok := r.current[k][o.Name]; ok {
			c.Change = apply.ChangeUpdated
		}
		c.Note = fmt.Sprintf("sink %q is created by this apply; not validated by the server", o.Sink)
		r.add(c)
		return nil
	}
	var got apply.Resource
	resp, err := r.ops[k].put(ctx, r.e.API, o.Name, dryRunPtr(r.dry), bytes.NewReader(o.Body)) //nolint:bodyclose // Decode closes it
	res, err := apiclient.Decode(resp, err, &got)
	if err != nil {
		c.Change, c.Error = apply.ChangeFailed, err.Error()
		r.add(c)
		if r.dry {
			return nil
		}
		return fmt.Errorf("apply stopped at %s %q (%s), the objects before it were applied: %w", kind.Name, o.Name, o.Origin, err)
	}
	old, existed := r.current[k][o.Name]
	c.Change = res.Change
	if c.Change == "" { // a server that does not send Angelia-Change
		c.Change = apply.ChangeCreated
		if existed {
			c.Change = apply.ChangeUpdated
		}
	}
	if existed && c.Change == apply.ChangeUpdated {
		c.Diff = apply.DiffWriteKeys(old, got, kind.WriteKeys)
		if len(c.Diff) == 0 {
			c.Note = "no difference in the fields the API shows"
		}
	}
	if k == apply.Sink && c.Change == apply.ChangeCreated {
		r.created[o.Name] = true
	}
	r.add(c)
	return nil
}

// pruneWatches deletes the planned watches. A dry run only reports them. A
// real run reads each watch again right before deleting it and keeps it if it
// left the scope meanwhile; the window between that read and the delete
// remains (no If-Match in API v0). On a failure it stops and lists the watches
// it did not delete.
func (r *applyRun) pruneWatches(ctx context.Context, names []string) error {
	for i, name := range names {
		c := apply.Change{Kind: "watch", Name: name, Change: apply.ChangeDeleted}
		if r.dry {
			r.add(c)
			continue
		}
		err := ctx.Err()
		if err == nil {
			c, err = r.deleteWatch(ctx, name)
		}
		if err != nil {
			r.add(apply.Change{Kind: "watch", Name: name, Change: apply.ChangeFailed, Error: err.Error()})
			for _, rest := range names[i+1:] {
				r.add(apply.Change{Kind: "watch", Name: rest, Change: apply.ChangeSkipped, Note: "not deleted: the prune stopped"})
			}
			left := strings.Join(names[i:], ", ")
			return fmt.Errorf("prune stopped at watch %q, not deleted: %s: %w", name, left, err)
		}
		r.add(c)
	}
	return nil
}

func (r *applyRun) deleteWatch(ctx context.Context, name string) (apply.Change, error) {
	c := apply.Change{Kind: "watch", Name: name, Change: apply.ChangeDeleted}
	var w apply.Resource
	resp, err := r.e.API.GetWatch(ctx, name) //nolint:bodyclose // Decode closes it
	if _, err := apiclient.Decode(resp, err, &w); err != nil {
		if isNotFound(err) {
			c.Change, c.Note = apply.ChangeGone, "deleted by someone else before this run"
			return c, nil
		}
		return c, err
	}
	if !apply.Prunable(w, r.sel) {
		c.Change, c.Note = apply.ChangeKept, "changed since the plan: no longer managed by the API or in the scope"
		return c, nil
	}
	resp, err = r.e.API.DeleteWatch(ctx, name, &apiclient.DeleteWatchParams{}) //nolint:bodyclose // Decode closes it
	if _, err := apiclient.Decode(resp, err, nil); err != nil {
		if isNotFound(err) {
			c.Change, c.Note = apply.ChangeGone, "deleted by someone else before this run"
			return c, nil
		}
		return c, err
	}
	return c, nil
}

func isNotFound(err error) bool {
	var ae *apiclient.Error
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// print writes the report: JSON, or one line per object with its diff, then
// the counts.
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
			fmt.Fprintf(&b, "    %s: %s -> %s\n", cell(d.Path), cell(apply.CompactJSON(d.Old)), cell(apply.CompactJSON(d.New)))
		}
		if c.Error != "" {
			b.WriteString("    " + strings.ReplaceAll(c.Error, "\n", "\n    ") + "\n")
		}
	}
	b.WriteString(r.report.Summary() + "\n")
	_, err := io.WriteString(r.e.Out, b.String())
	return err
}
