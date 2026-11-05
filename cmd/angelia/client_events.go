package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/angelia-dev/angelia/internal/apiclient"
)

// envelope is the part of a release event (CloudEvents envelope) the table
// shows. The JSON output keeps the envelope as the server sent it.
type envelope struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Subject string `json:"subject"`
	Time    string `json:"time"`
	Data    struct {
		Package         string `json:"package"`
		Version         string `json:"version"`
		PreviousVersion string `json:"previous_version"`
		Bump            string `json:"bump"`
		Watch           struct {
			Name string `json:"name"`
		} `json:"watch"`
	} `json:"data"`
}

func parseEnvelope(raw json.RawMessage) envelope {
	var e envelope
	_ = json.Unmarshal(raw, &e) // a malformed envelope shows as empty cells; -o json shows it raw
	return e
}

// shortType drops the "dev.angelia." prefix and the ".v<major>" suffix.
func shortType(t string) string {
	t = strings.TrimPrefix(t, "dev.angelia.")
	if i := strings.LastIndex(t, ".v"); i > 0 {
		t = t[:i]
	}
	return t
}

var eventHeader = []string{"SEQ", "EVENT", "TYPE", "WATCH", "PACKAGE", "VERSION", "TIME"}

func eventRow(r apiclient.EventRecord) []string {
	e := parseEnvelope(r.Event)
	return []string{fmt.Sprint(r.Seq), e.ID, shortType(e.Type), e.Data.Watch.Name, e.Data.Package, e.Data.Version, e.Time}
}

func eventTable(rs []apiclient.EventRecord) table {
	t := table{Header: eventHeader}
	for _, r := range rs {
		t.Rows = append(t.Rows, eventRow(r))
	}
	return t
}

func eventDetail(r apiclient.EventRecord) table {
	e := parseEnvelope(r.Event)
	return detail(
		"seq", fmt.Sprint(r.Seq),
		"event", e.ID,
		"type", shortType(e.Type),
		"subject", e.Subject,
		"watch", e.Data.Watch.Name,
		"package", e.Data.Package,
		"version", e.Data.Version,
		"previous version", e.Data.PreviousVersion,
		"bump", e.Data.Bump,
		"time", e.Time,
		"yanks event", deref(r.RefEventId),
	)
}

// eventFilterFlags are the filters shared by `event list` and `event stream`.
type eventFilterFlags struct {
	watch, typ, source []string
	after              int64
}

func (f *eventFilterFlags) add(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringSliceVar(&f.watch, "watch", nil, "only events of these watches, by name or id")
	fl.StringSliceVar(&f.typ, "type", nil, "only these event types (release.published, release.yanked, release.prerelease)")
	fl.StringSliceVar(&f.source, "source", nil, "only events of watches with these source kinds")
	fl.Int64Var(&f.after, "after", 0, "only events with a seq greater than this (0: from the oldest kept)")
	_ = cmd.RegisterFlagCompletionFunc("type", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"release.published", "release.yanked", "release.prerelease"}, cobra.ShellCompDirectiveNoFileComp
	})
}

func shortTypes(s []string) *[]apiclient.ShortEventType {
	if len(s) == 0 {
		return nil
	}
	out := make([]apiclient.ShortEventType, len(s))
	for i, v := range s {
		out[i] = apiclient.ShortEventType(v)
	}
	return &out
}

func newEventCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "event",
		Aliases: []string{"events"},
		Short:   "Read release events through the API",
	}
	addClientFlags(cmd)
	cmd.AddCommand(newEventListCmd(), newEventGetCmd(), newEventStreamCmd())
	return cmd
}

func newEventListCmd() *cobra.Command {
	var ff eventFilterFlags
	var limit int
	var all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Pull events after a cursor (--after)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := newCLIEnv(cmd)
			if err != nil {
				return err
			}
			out := apiclient.EventPage{Items: []apiclient.EventRecord{}, NextAfter: ff.after}
			for {
				after := out.NextAfter
				p := &apiclient.ListEventsParams{After: &after, Watch: sliceOrNil(ff.watch), Type: shortTypes(ff.typ), Source: sliceOrNil(ff.source)}
				if limit > 0 {
					p.Limit = &limit
				}
				var pg apiclient.EventPage
				resp, err := e.API.ListEvents(cmd.Context(), p) //nolint:bodyclose // Decode closes it
				if _, err := apiclient.Decode(resp, err, &pg); err != nil {
					return err
				}
				out.Items = append(out.Items, pg.Items...)
				grew := pg.NextAfter > after
				out.NextAfter, out.HasMore = pg.NextAfter, pg.HasMore
				if !all || !pg.HasMore {
					break
				}
				if !grew {
					return fmt.Errorf("the server returned next_after %d after %d; stopping", pg.NextAfter, after)
				}
			}
			if err := render(e.Out, e.Format, out, eventTable(out.Items)); err != nil {
				return err
			}
			if e.Format == formatTable && out.HasMore {
				_, _ = fmt.Fprintf(e.Err, "more results: --after %d (or --all)\n", out.NextAfter)
			}
			return nil
		},
	}
	ff.add(cmd)
	cmd.Flags().IntVar(&limit, "limit", 0, "page size (the server's default when 0)")
	cmd.Flags().BoolVar(&all, "all", false, "keep pulling until there are no more events")
	return cmd
}

func newEventGetCmd() *cobra.Command {
	return newGetCmd("event", "ID", func(ctx context.Context, a *apiclient.API, id string) (*http.Response, error) {
		return a.GetEvent(ctx, id)
	}, eventDetail)
}

func newEventStreamCmd() *cobra.Command {
	var ff eventFilterFlags
	var noReconnect bool
	cmd := &cobra.Command{
		Use:   "stream",
		Short: "Follow events as they are committed (server-sent events)",
		Long: `Follow events as they are committed. The events after --after come first,
then each new event. With -o json every event record is one line of JSON.
The stream reconnects after a break and resumes after the last event seen;
stop it with Ctrl-C.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := newCLIEnv(cmd)
			if err != nil {
				return err
			}
			p := apiclient.StreamEventsParams{Watch: sliceOrNil(ff.watch), Type: shortTypes(ff.typ), Source: sliceOrNil(ff.source)}
			if ff.after > 0 {
				p.After = &ff.after
			}
			printEvent := streamPrinter(e)
			opts := apiclient.StreamOptions{Reconnect: !noReconnect, OnReconnect: func(err error, wait time.Duration) {
				reason := "stream ended"
				if err != nil {
					reason = err.Error()
				}
				_, _ = fmt.Fprintf(e.Err, "reconnecting in %s: %s\n", wait.Round(time.Millisecond), reason)
			}}
			return e.API.Stream(cmd.Context(), p, opts, printEvent)
		},
	}
	ff.add(cmd)
	cmd.Flags().BoolVar(&noReconnect, "no-reconnect", false, "exit when the stream ends instead of reconnecting")
	return cmd
}

// streamPrinter prints one event at a time. The table has a fixed layout so
// that rows can be written as they arrive.
func streamPrinter(e *cliEnv) func(apiclient.EventRecord) error {
	if e.Format == formatJSON {
		enc := json.NewEncoder(e.Out)
		enc.SetEscapeHTML(false)
		return func(r apiclient.EventRecord) error { return enc.Encode(r) }
	}
	widths := []int{8, 26, 18, 20, 20, 16}
	line := func(cells []string) error {
		var b strings.Builder
		for i, c := range cells {
			c = cell(c)
			if i < len(widths) {
				c = truncate(c, widths[i])
				fmt.Fprintf(&b, "%-*s  ", widths[i], c)
			} else {
				b.WriteString(c)
			}
		}
		_, err := fmt.Fprintln(e.Out, strings.TrimRight(b.String(), " "))
		return err
	}
	started := false
	return func(r apiclient.EventRecord) error {
		if !started {
			started = true
			if err := line(eventHeader); err != nil {
				return err
			}
		}
		return line(eventRow(r))
	}
}

func newDeliveryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "delivery",
		Aliases: []string{"deliveries"},
		Short:   "Inspect deliveries through the API",
	}
	addClientFlags(cmd)
	cmd.AddCommand(
		newListCmd("delivery", func(cmd *cobra.Command) func(*apiclient.API, *int) fetchPage[apiclient.Delivery] {
			status := cmd.Flags().StringSlice("status", nil, "only deliveries in these states (pending, in_flight, delivered, dead)")
			subscription := cmd.Flags().String("subscription", "", "only deliveries of this subscription, by name or id")
			event := cmd.Flags().String("event", "", "only deliveries of this event id")
			_ = cmd.RegisterFlagCompletionFunc("status", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
				return []string{"pending", "in_flight", "delivered", "dead"}, cobra.ShellCompDirectiveNoFileComp
			})
			return func(a *apiclient.API, limit *int) fetchPage[apiclient.Delivery] {
				return func(ctx context.Context, cursor *string) ([]apiclient.Delivery, *string, error) {
					p := &apiclient.ListDeliveriesParams{Limit: limit, Cursor: cursor, Subscription: strOrNil(*subscription), Event: strOrNil(*event)}
					if len(*status) > 0 {
						st := make([]apiclient.DeliveryStatus, len(*status))
						for i, s := range *status {
							st[i] = apiclient.DeliveryStatus(s)
						}
						p.Status = &st
					}
					var out apiclient.DeliveryPage
					resp, err := a.ListDeliveries(ctx, p) //nolint:bodyclose // Decode closes it
					_, err = apiclient.Decode(resp, err, &out)
					return out.Items, out.NextCursor, err
				}
			}
		}, deliveryTable),
		newGetCmd("delivery", "ID", func(ctx context.Context, a *apiclient.API, id string) (*http.Response, error) {
			return a.GetDelivery(ctx, id)
		}, deliveryDetail),
	)
	return cmd
}

func deliveryTable(ds []apiclient.Delivery) table {
	t := table{Header: []string{"ID", "EVENT", "SUBSCRIPTION", "STATUS", "ATTEMPTS", "NEXT ATTEMPT", "LAST ERROR"}}
	for _, d := range ds {
		t.Rows = append(t.Rows, []string{
			d.Id, d.EventId, deliverySubscription(d), string(d.Status), fmt.Sprint(d.Attempts),
			timeCell(d.NextAttemptAt), truncate(deref(d.LastError), 60),
		})
	}
	return t
}

func deliveryDetail(d apiclient.Delivery) table {
	return detail(
		"id", d.Id,
		"event", d.EventId,
		"subscription", deliverySubscription(d),
		"subscription id", d.SubscriptionId,
		"idempotency key", d.IdempotencyKey,
		"status", string(d.Status),
		"attempts", fmt.Sprint(d.Attempts),
		"next attempt", timeCell(d.NextAttemptAt),
		"held until", timeCell(d.HeldUntil),
		"last error", deref(d.LastError),
	)
}

// deliverySubscription is the subscription's name, or its id once the
// subscription was deleted (name is null then).
func deliverySubscription(d apiclient.Delivery) string {
	if d.SubscriptionName != nil {
		return *d.SubscriptionName
	}
	return d.SubscriptionId + " (deleted)"
}
