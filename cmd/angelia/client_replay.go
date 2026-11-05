package main

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/angelia-dev/angelia/internal/apiclient"
)

// replayFlags are the selectors of `angelia replay`.
type replayFlags struct {
	delivery, event, subscription string
	since, until                  string
	status                        []string
	dryRun                        bool
}

func newReplayCmd() *cobra.Command {
	var f replayFlags
	cmd := &cobra.Command{
		Use:   "replay (--delivery ID | --event ID | --subscription NAME --since T [--until T])",
		Short: "Queue dead (or delivered) deliveries again",
		Long: `Queue deliveries again: one delivery (--delivery), the deliveries of one
event (--event), or those of one subscription whose event time is in
[--since, --until). A replay queues the existing delivery again, with the same
id and idempotency key; its attempts keep counting and the retry budget
starts again. Only dead deliveries are replayed unless --status says
otherwise; pending and in-flight ones never are.

--since and --until take an RFC 3339 time (2026-11-02T00:00:00Z), "now", or a
duration back from now (24h, 90m, 7d). --until defaults to now.

The server records each replay in its audit log.`,
		Example: `  angelia replay --delivery 01JBX2A9QWERTY0123456789AB
  angelia replay --event 01JBX2A7C8D9E0F1G2H3J4K5M6
  angelia replay --subscription platform-ci --since 24h --dry-run
  angelia replay --subscription platform-ci --since 2026-11-02T00:00:00Z --until 2026-11-03T00:00:00Z --status dead,delivered`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			req, err := f.request(cmd, time.Now())
			if err != nil {
				return err
			}
			e, err := newCLIEnv(cmd)
			if err != nil {
				return err
			}
			var res apiclient.ReplayResult
			if f.delivery != "" {
				var d apiclient.Delivery
				resp, err := e.API.ReplayDelivery(cmd.Context(), f.delivery, apiclient.ReplayDeliveryJSONRequestBody{}) //nolint:bodyclose // Decode closes it
				if _, err := apiclient.Decode(resp, err, &d); err != nil {
					return err
				}
				res = apiclient.ReplayResult{Replayed: 1, Deliveries: []apiclient.Delivery{d}}
			} else {
				resp, err := e.API.CreateReplay(cmd.Context(), req) //nolint:bodyclose // Decode closes it
				if _, err := apiclient.Decode(resp, err, &res); err != nil {
					return err
				}
				if res.Deliveries == nil {
					res.Deliveries = []apiclient.Delivery{}
				}
			}
			return e.printReplay(res)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.delivery, "delivery", "", "replay this one delivery, by id")
	fl.StringVar(&f.event, "event", "", "replay the deliveries of this event id")
	fl.StringVar(&f.subscription, "subscription", "", "replay the deliveries of this subscription, by name or id (with --since)")
	fl.StringVar(&f.since, "since", "", "with --subscription: events from this time (RFC 3339, now, or a duration back such as 24h or 7d)")
	fl.StringVar(&f.until, "until", "", "with --since: events before this time (default now)")
	fl.StringSliceVar(&f.status, "status", nil, "replay deliveries in these states: dead, delivered (default dead)")
	fl.BoolVar(&f.dryRun, "dry-run", false, "show what would be replayed without queuing anything (not with --delivery)")
	_ = cmd.RegisterFlagCompletionFunc("status", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"dead", "delivered"}, cobra.ShellCompDirectiveNoFileComp
	})
	addClientFlags(cmd)
	return cmd
}

// request checks the selector flags and builds the bulk replay request. With
// --delivery it only checks that no other selector is given.
func (f *replayFlags) request(cmd *cobra.Command, now time.Time) (apiclient.ReplayRequest, error) {
	var req apiclient.ReplayRequest
	changed := cmd.Flags().Changed
	if f.delivery != "" {
		for _, other := range []string{"event", "subscription", "since", "until", "status", "dry-run"} {
			if changed(other) {
				return req, fmt.Errorf("--delivery cannot be combined with --%s: it replays one delivery, without a dry run", other)
			}
		}
		return req, nil
	}
	if (f.since != "" || f.until != "") && f.subscription == "" {
		return req, errors.New("--since and --until need --subscription")
	}
	if f.event == "" && f.subscription == "" {
		return req, errors.New("give --delivery ID, --event ID, or --subscription NAME with --since")
	}
	if f.subscription != "" && f.since == "" {
		if f.until != "" {
			return req, errors.New("--until needs --since")
		}
		if f.event == "" {
			return req, errors.New("--subscription needs --since (and --until, default now), or --event")
		}
	}
	if f.event != "" {
		req.Event = &f.event
	}
	if f.subscription != "" {
		req.Subscription = &f.subscription
	}
	if f.since != "" {
		since, err := parseReplayTime(f.since, now)
		if err != nil {
			return req, fmt.Errorf("--since: %w", err)
		}
		until := now.UTC().Truncate(time.Millisecond)
		if f.until != "" {
			if until, err = parseReplayTime(f.until, now); err != nil {
				return req, fmt.Errorf("--until: %w", err)
			}
		}
		if !until.After(since) {
			return req, fmt.Errorf("--until must be after --since (%s is not after %s)", fmtTime(until), fmtTime(since))
		}
		req.Since, req.Until = &since, &until
	}
	if len(f.status) > 0 {
		var st []apiclient.ReplayRequestStatus // without repeats: the API refuses them
		for _, s := range f.status {
			v := apiclient.ReplayRequestStatus(s)
			if !v.Valid() {
				return req, fmt.Errorf("--status: %q cannot be replayed; want dead or delivered", s)
			}
			if !slices.Contains(st, v) {
				st = append(st, v)
			}
		}
		req.Status = &st
	}
	if f.dryRun {
		req.DryRun = &f.dryRun
	}
	return req, nil
}

// parseReplayTime reads a --since or --until value: an RFC 3339 time, "now",
// or a positive duration back from now (Go syntax such as 24h or 90m, or
// whole days such as 7d). The result is UTC, cut to milliseconds as the API
// wants it.
func parseReplayTime(s string, now time.Time) (time.Time, error) {
	t, err := replayTime(s, now)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC().Truncate(time.Millisecond), nil
}

func replayTime(s string, now time.Time) (time.Time, error) {
	if s == "now" {
		return now, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 || n > 36500 {
			return time.Time{}, fmt.Errorf("%q is not a number of days", s)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return time.Time{}, fmt.Errorf("%q is not an RFC 3339 time, now, or a duration such as 24h or 7d", s)
		}
	}
	if d <= 0 {
		return time.Time{}, fmt.Errorf("%q: the duration back from now must be positive", s)
	}
	return now.Add(-d), nil
}

// printReplay prints what a replay did: in JSON the API's ReplayResult (a
// single replay is wrapped in the same shape), as a table a summary line and
// the deliveries listed.
func (e *cliEnv) printReplay(res apiclient.ReplayResult) error {
	if e.Format == formatJSON {
		return writeJSON(e.Out, res)
	}
	if _, err := fmt.Fprintln(e.Out, replaySummary(res)); err != nil {
		return err
	}
	if len(res.Deliveries) == 0 {
		return nil
	}
	return writeTable(e.Out, deliveryTable(res.Deliveries))
}

func replaySummary(res apiclient.ReplayResult) string {
	if res.Replayed == 0 {
		if res.DryRun {
			return "no delivery matched; nothing would be queued (dry run)"
		}
		return "no delivery matched; nothing was queued"
	}
	noun := "deliveries"
	if res.Replayed == 1 {
		noun = "delivery"
	}
	s := fmt.Sprintf("%d %s queued again", res.Replayed, noun)
	if res.DryRun {
		s = fmt.Sprintf("%d %s would be queued again (dry run)", res.Replayed, noun)
	}
	if res.Truncated {
		s += fmt.Sprintf("; only the first %d listed", len(res.Deliveries))
	}
	return s
}
