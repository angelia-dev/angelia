package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/angelia-dev/angelia/internal/apiclient"
)

// newListCmd builds `<kind> list`. setup registers the command's own filter
// flags and returns the function that fetches one page with them.
func newListCmd[T any](kind string, setup func(cmd *cobra.Command) func(a *apiclient.API, limit *int) fetchPage[T], tbl func([]T) table) *cobra.Command {
	var lf listFlags
	var mk func(a *apiclient.API, limit *int) fetchPage[T]
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List " + kind + "s",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := newCLIEnv(cmd)
			if err != nil {
				return err
			}
			p, err := collect(cmd.Context(), mk(e.API, lf.limitPtr()), lf.cursor, lf.all)
			if err != nil {
				return err
			}
			return e.printList(p, p.NextCursor, tbl(p.Items))
		},
	}
	lf.add(cmd)
	mk = setup(cmd)
	return cmd
}

// newGetCmd builds `<kind> get NAME`.
func newGetCmd[T any](kind, arg string, get func(ctx context.Context, a *apiclient.API, id string) (*http.Response, error), tbl func(T) table) *cobra.Command {
	return &cobra.Command{
		Use:   "get " + arg,
		Short: "Show one " + kind,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newCLIEnv(cmd)
			if err != nil {
				return err
			}
			var v T
			resp, err := get(cmd.Context(), e.API, args[0]) //nolint:bodyclose // Decode closes it
			if _, err := apiclient.Decode(resp, err, &v); err != nil {
				return err
			}
			return render(e.Out, e.Format, v, tbl(v))
		},
	}
}

// newPutCmd builds `<kind> put NAME -f FILE`, the idempotent upsert.
func newPutCmd[T any](kind string, put func(ctx context.Context, a *apiclient.API, name string, dryRun *bool, body io.Reader) (*http.Response, error), tbl func(T) table) *cobra.Command {
	var pf putFlags
	cmd := &cobra.Command{
		Use:   "put NAME -f FILE",
		Short: "Create or replace a " + kind + " from a JSON or YAML file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := pf.body(cmd)
			if err != nil {
				return err
			}
			e, err := newCLIEnv(cmd)
			if err != nil {
				return err
			}
			var v T
			resp, err := put(cmd.Context(), e.API, args[0], dryRunPtr(pf.dryRun), body) //nolint:bodyclose // Decode closes it
			res, err := apiclient.Decode(resp, err, &v)
			if err != nil {
				return err
			}
			if err := render(e.Out, e.Format, v, tbl(v)); err != nil {
				return err
			}
			e.reportChange(kind, args[0], res, pf.dryRun)
			return nil
		},
	}
	pf.add(cmd)
	return cmd
}

// newDeleteCmd builds `<kind> delete NAME`.
func newDeleteCmd(kind string, del func(ctx context.Context, a *apiclient.API, name string, dryRun *bool) (*http.Response, error)) *cobra.Command {
	var dry bool
	cmd := &cobra.Command{
		Use:     "delete NAME",
		Aliases: []string{"rm"},
		Short:   "Delete a " + kind,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newCLIEnv(cmd)
			if err != nil {
				return err
			}
			resp, err := del(cmd.Context(), e.API, args[0], dryRunPtr(dry)) //nolint:bodyclose // Decode closes it
			if _, err := apiclient.Decode(resp, err, nil); err != nil {
				return err
			}
			e.deleteReport(kind, args[0], dry)
			return nil
		},
	}
	cmd.Flags().BoolVar(&dry, "dry-run", false, "validate without deleting")
	return cmd
}

func newWatchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "watch",
		Aliases: []string{"watches"},
		Short:   "Manage watches through the API",
	}
	addClientFlags(cmd)
	cmd.AddCommand(
		newListCmd("watch", func(cmd *cobra.Command) func(*apiclient.API, *int) fetchPage[apiclient.Watch] {
			src := cmd.Flags().StringSlice("source", nil, "only watches with these source kinds")
			lbl := cmd.Flags().StringArray("label", nil, "only watches with this label, as key=value (repeatable)")
			return func(a *apiclient.API, limit *int) fetchPage[apiclient.Watch] {
				return func(ctx context.Context, cursor *string) ([]apiclient.Watch, *string, error) {
					p := &apiclient.ListWatchesParams{Limit: limit, Cursor: cursor, Source: sliceOrNil(*src), Label: sliceOrNil(*lbl)}
					var out apiclient.WatchPage
					resp, err := a.ListWatches(ctx, p) //nolint:bodyclose // Decode closes it
					_, err = apiclient.Decode(resp, err, &out)
					return out.Items, out.NextCursor, err
				}
			}
		}, watchTable),
		newGetCmd("watch", "NAME", func(ctx context.Context, a *apiclient.API, n string) (*http.Response, error) {
			return a.GetWatch(ctx, n)
		}, watchDetail),
		newPutCmd("watch", func(ctx context.Context, a *apiclient.API, n string, dry *bool, b io.Reader) (*http.Response, error) {
			return a.PutWatchWithBody(ctx, n, &apiclient.PutWatchParams{DryRun: dry}, jsonContentType, b)
		}, watchDetail),
		newDeleteCmd("watch", func(ctx context.Context, a *apiclient.API, n string, dry *bool) (*http.Response, error) {
			return a.DeleteWatch(ctx, n, &apiclient.DeleteWatchParams{DryRun: dry})
		}),
		newVersionsCmd(),
	)
	return cmd
}

func newVersionsCmd() *cobra.Command {
	var lf listFlags
	var prerelease, yanked string
	cmd := &cobra.Command{
		Use:   "versions NAME",
		Short: "List the known versions of a watch's target",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newCLIEnv(cmd)
			if err != nil {
				return err
			}
			pre, err := optBool("prerelease", prerelease)
			if err != nil {
				return err
			}
			yk, err := optBool("yanked", yanked)
			if err != nil {
				return err
			}
			fetch := func(ctx context.Context, cursor *string) ([]apiclient.Version, *string, error) {
				p := &apiclient.ListWatchVersionsParams{Limit: lf.limitPtr(), Cursor: cursor, Prerelease: pre, Yanked: yk}
				var out apiclient.VersionPage
				resp, err := e.API.ListWatchVersions(ctx, args[0], p) //nolint:bodyclose // Decode closes it
				_, err = apiclient.Decode(resp, err, &out)
				return out.Items, out.NextCursor, err
			}
			p, err := collect(cmd.Context(), fetch, lf.cursor, lf.all)
			if err != nil {
				return err
			}
			return e.printList(p, p.NextCursor, versionTable(p.Items))
		},
	}
	lf.add(cmd)
	cmd.Flags().StringVar(&prerelease, "prerelease", "", "true: only pre-releases; false: only final releases")
	cmd.Flags().StringVar(&yanked, "yanked", "", "true: only yanked versions; false: only versions not yanked")
	return cmd
}

func optBool(name, s string) (*bool, error) {
	switch s {
	case "":
		return nil, nil
	case "true":
		t := true
		return &t, nil
	case "false":
		f := false
		return &f, nil
	}
	return nil, fmt.Errorf("--%s: want true or false, got %q", name, s)
}

func newSinkCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "sink",
		Aliases: []string{"sinks"},
		Short:   "Manage sinks through the API",
	}
	addClientFlags(cmd)
	cmd.AddCommand(
		newListCmd("sink", func(cmd *cobra.Command) func(*apiclient.API, *int) fetchPage[apiclient.Sink] {
			kinds := cmd.Flags().StringSlice("kind", nil, "only sinks of these kinds")
			return func(a *apiclient.API, limit *int) fetchPage[apiclient.Sink] {
				return func(ctx context.Context, cursor *string) ([]apiclient.Sink, *string, error) {
					k := sliceOrNil(*kinds)
					p := &apiclient.ListSinksParams{Limit: limit, Cursor: cursor, Kind: k}
					var out apiclient.SinkPage
					resp, err := a.ListSinks(ctx, p) //nolint:bodyclose // Decode closes it
					_, err = apiclient.Decode(resp, err, &out)
					return out.Items, out.NextCursor, err
				}
			}
		}, sinkTable),
		newGetCmd("sink", "NAME", func(ctx context.Context, a *apiclient.API, n string) (*http.Response, error) {
			return a.GetSink(ctx, n)
		}, sinkDetail),
		newPutCmd("sink", func(ctx context.Context, a *apiclient.API, n string, dry *bool, b io.Reader) (*http.Response, error) {
			return a.PutSinkWithBody(ctx, n, &apiclient.PutSinkParams{DryRun: dry}, jsonContentType, b)
		}, sinkDetail),
		newDeleteCmd("sink", func(ctx context.Context, a *apiclient.API, n string, dry *bool) (*http.Response, error) {
			return a.DeleteSink(ctx, n, &apiclient.DeleteSinkParams{DryRun: dry})
		}),
	)
	return cmd
}

func newSubCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "sub",
		Aliases: []string{"subs", "subscription", "subscriptions"},
		Short:   "Manage subscriptions through the API",
	}
	addClientFlags(cmd)
	cmd.AddCommand(
		newListCmd("subscription", func(cmd *cobra.Command) func(*apiclient.API, *int) fetchPage[apiclient.Subscription] {
			sink := cmd.Flags().String("sink", "", "only subscriptions that deliver to this sink")
			return func(a *apiclient.API, limit *int) fetchPage[apiclient.Subscription] {
				return func(ctx context.Context, cursor *string) ([]apiclient.Subscription, *string, error) {
					p := &apiclient.ListSubscriptionsParams{Limit: limit, Cursor: cursor, Sink: strOrNil(*sink)}
					var out apiclient.SubscriptionPage
					resp, err := a.ListSubscriptions(ctx, p) //nolint:bodyclose // Decode closes it
					_, err = apiclient.Decode(resp, err, &out)
					return out.Items, out.NextCursor, err
				}
			}
		}, subTable),
		newGetCmd("subscription", "NAME", func(ctx context.Context, a *apiclient.API, n string) (*http.Response, error) {
			return a.GetSubscription(ctx, n)
		}, subDetail),
		newPutCmd("subscription", func(ctx context.Context, a *apiclient.API, n string, dry *bool, b io.Reader) (*http.Response, error) {
			return a.PutSubscriptionWithBody(ctx, n, &apiclient.PutSubscriptionParams{DryRun: dry}, jsonContentType, b)
		}, subDetail),
		newDeleteCmd("subscription", func(ctx context.Context, a *apiclient.API, n string, dry *bool) (*http.Response, error) {
			return a.DeleteSubscription(ctx, n, &apiclient.DeleteSubscriptionParams{DryRun: dry})
		}),
	)
	return cmd
}

func strOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func sliceOrNil[T ~string](s []T) *[]T {
	if len(s) == 0 {
		return nil
	}
	return &s
}

// kv formats a string map as sorted key=value pairs.
func kv(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func joinStrs[T ~string](p *[]T) string {
	if p == nil {
		return ""
	}
	s := make([]string, len(*p))
	for i, v := range *p {
		s[i] = string(v)
	}
	return strings.Join(s, ",")
}

func watchTable(ws []apiclient.Watch) table {
	t := table{Header: []string{"NAME", "SOURCE", "COORDINATES", "INTERVAL", "SCHEME", "MANAGED", "LAST CHECKED", "ERRORS"}}
	for _, w := range ws {
		t.Rows = append(t.Rows, []string{
			w.Name, w.Source, kv(w.Coordinates), w.Interval, w.Scheme, string(w.ManagedBy),
			timeCell(w.Status.LastCheckedAt), fmt.Sprint(w.Status.ErrorCount),
		})
	}
	return t
}

func watchDetail(w apiclient.Watch) table {
	pol := w.Policy
	return detail(
		"name", w.Name,
		"id", w.Id,
		"managed by", string(w.ManagedBy),
		"source", w.Source,
		"coordinates", kv(w.Coordinates),
		"interval", w.Interval,
		"scheme", w.Scheme,
		"labels", kv(w.Labels),
		"policy.prerelease", string(deref(pol.Prerelease)),
		"policy.include", strings.Join(deref(pol.Include), ","),
		"policy.exclude", strings.Join(deref(pol.Exclude), ","),
		"policy.constraint", deref(pol.Constraint),
		"policy.bump", joinStrs(pol.Bump),
		"policy.cooldown", deref(pol.Cooldown),
		"policy.baseline", fmt.Sprint(deref(pol.Baseline)),
		"baseline done", fmt.Sprint(w.Status.BaselineDone),
		"effective interval", w.Status.EffectiveInterval,
		"last checked", timeCell(w.Status.LastCheckedAt),
		"next check", timeCell(w.Status.NextCheckAt),
		"errors", fmt.Sprint(w.Status.ErrorCount),
	)
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func versionTable(vs []apiclient.Version) table {
	t := table{Header: []string{"VERSION", "PRERELEASE", "YANKED", "PUBLISHED", "FIRST SEEN", "REMOVED"}}
	for _, v := range vs {
		t.Rows = append(t.Rows, []string{
			v.Version, fmt.Sprint(v.Prerelease), fmt.Sprint(v.Yanked),
			timeCell(v.PublishedAt), fmtTime(v.FirstSeenAt), timeCell(v.RemovedAt),
		})
	}
	return t
}

func sinkTable(ss []apiclient.Sink) table {
	t := table{Header: []string{"NAME", "KIND", "MANAGED", "ID"}}
	for _, s := range ss {
		t.Rows = append(t.Rows, []string{s.Name, s.Kind, string(s.ManagedBy), s.Id})
	}
	return t
}

func sinkDetail(s apiclient.Sink) table {
	t := detail("name", s.Name, "id", s.Id, "managed by", string(s.ManagedBy), "kind", s.Kind,
		"url redacted", fmt.Sprint(s.UrlRedacted), "masked fields", strings.Join(s.MaskedFields, ","))
	keys := make([]string, 0, len(s.Config))
	for k := range s.Config {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Rows = append(t.Rows, []string{"config." + k, fmt.Sprint(s.Config[k])})
	}
	return t
}

func subTable(ss []apiclient.Subscription) table {
	t := table{Header: []string{"NAME", "SINK", "TYPES", "SOURCES", "MANAGED"}}
	for _, s := range ss {
		t.Rows = append(t.Rows, []string{s.Name, s.Sink, joinStrs(s.Filter.Type), joinStrs(s.Filter.Source), string(s.ManagedBy)})
	}
	return t
}

func subDetail(s apiclient.Subscription) table {
	f := s.Filter
	return detail(
		"name", s.Name,
		"id", s.Id,
		"managed by", string(s.ManagedBy),
		"sink", s.Sink,
		"filter.type", joinStrs(f.Type),
		"filter.source", joinStrs(f.Source),
		"filter.bump", joinStrs(f.Bump),
		"filter.package", deref(f.Package),
		"filter.labels", kv(deref(f.Labels)),
		"policy.prerelease", string(deref(s.Policy.Prerelease)),
		"policy.cooldown", deref(s.Policy.Cooldown),
		"template", s.Template,
	)
}
