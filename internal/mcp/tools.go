package mcp

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/angelia-dev/angelia/internal/apiclient"
)

// Defaults and caps of spec docs/mcp.md "Tools".
const (
	defaultWatchLimit = 50
	defaultEventLimit = 20
	maxVersionPages   = 10
	jsonContentType   = "application/json"
)

// decodeArgs decodes arguments that passed the input schema and
// normalizeNumbers. It should not fail; if it does, the detail stays plain.
func decodeArgs(raw json.RawMessage, dst any) *toolError {
	if err := json.Unmarshal(raw, dst); err != nil {
		return invalidArguments(fieldError{Pointer: "", Detail: "the arguments do not have the expected types"})
	}
	return nil
}

func nonEmpty[T any](s []T) *[]T {
	if len(s) == 0 {
		return nil
	}
	return &s
}

type watchSummary struct {
	Name        string            `json:"name"`
	Source      string            `json:"source"`
	Coordinates map[string]string `json:"coordinates"`
	Labels      map[string]string `json:"labels"`
	Interval    string            `json:"interval"`
	Scheme      string            `json:"scheme"`
}

type watchList struct {
	Items      []watchSummary `json:"items"`
	NextCursor *string        `json:"next_cursor"`
}

func listWatches(ctx context.Context, a *apiclient.API, raw json.RawMessage) (any, *toolError) {
	var in struct {
		Cursor *string  `json:"cursor"`
		Limit  *int     `json:"limit"`
		Source []string `json:"source"`
		Label  []string `json:"label"`
	}
	if e := decodeArgs(raw, &in); e != nil {
		return nil, e
	}
	limit := defaultWatchLimit
	if in.Limit != nil {
		limit = *in.Limit
	}
	var out watchList
	resp, err := a.ListWatches(ctx, &apiclient.ListWatchesParams{ //nolint:bodyclose // call closes it
		Limit: &limit, Cursor: in.Cursor, Source: nonEmpty(in.Source), Label: nonEmpty(in.Label),
	})
	if _, e := call(resp, err, &out); e != nil {
		return nil, e
	}
	if out.Items == nil {
		out.Items = []watchSummary{}
	}
	for i := range out.Items {
		if out.Items[i].Labels == nil {
			out.Items[i].Labels = map[string]string{}
		}
	}
	return out, nil
}

type latest struct {
	Watch             string  `json:"watch"`
	Version           string  `json:"version"`
	NormalizedVersion string  `json:"normalized_version"`
	Prerelease        bool    `json:"prerelease"`
	PublishedAt       *string `json:"published_at"`
	FirstSeenAt       string  `json:"first_seen_at"`
}

// latestVersion reads the versions greatest first and returns the first one
// that is not removed, at most maxVersionPages pages deep (spec D12). The
// server filters yanked versions, and pre-releases unless asked for them;
// the tool checks both again.
func latestVersion(ctx context.Context, a *apiclient.API, raw json.RawMessage) (any, *toolError) {
	var in struct {
		Watch             string `json:"watch"`
		IncludePrerelease bool   `json:"include_prerelease"`
	}
	if e := decodeArgs(raw, &in); e != nil {
		return nil, e
	}
	no := false
	params := &apiclient.ListWatchVersionsParams{Yanked: &no}
	if !in.IncludePrerelease {
		params.Prerelease = &no
	}
	for range maxVersionPages {
		var page struct {
			Items []struct {
				latest
				Yanked    bool    `json:"yanked"`
				RemovedAt *string `json:"removed_at"`
			} `json:"items"`
			NextCursor *string `json:"next_cursor"`
		}
		resp, err := a.ListWatchVersions(ctx, in.Watch, params) //nolint:bodyclose // call closes it
		if _, e := call(resp, err, &page); e != nil {
			return nil, e
		}
		for _, v := range page.Items {
			if v.RemovedAt == nil && !v.Yanked && (in.IncludePrerelease || !v.Prerelease) {
				v.Watch = in.Watch
				return v.latest, nil
			}
		}
		if page.NextCursor == nil {
			return nil, newProblem(slugNoVersion, "watch %s has no version that is neither yanked nor removed%s", in.Watch, finalOnly(in.IncludePrerelease))
		}
		params.Cursor = page.NextCursor
	}
	return nil, newProblem(slugNoVersion, "watch %s: the search stopped after %d pages of versions without one that is neither yanked nor removed%s", in.Watch, maxVersionPages, finalOnly(in.IncludePrerelease))
}

func finalOnly(pre bool) string {
	if pre {
		return ""
	}
	return " among the final releases"
}

type eventPage struct {
	Items     json.RawMessage `json:"items"`
	NextAfter int64           `json:"next_after"`
	HasMore   bool            `json:"has_more"`
}

func eventsSince(ctx context.Context, a *apiclient.API, raw json.RawMessage) (any, *toolError) {
	var in struct {
		After    *int64   `json:"after"`
		Consumer *string  `json:"consumer"`
		Limit    *int     `json:"limit"`
		Watch    []string `json:"watch"`
		Type     []string `json:"type"`
		Source   []string `json:"source"`
	}
	if e := decodeArgs(raw, &in); e != nil {
		return nil, e
	}
	limit := defaultEventLimit
	if in.Limit != nil {
		limit = *in.Limit
	}
	params := &apiclient.ListEventsParams{Limit: &limit, Consumer: in.Consumer}
	if in.Consumer == nil { // the input schema allows nothing else with consumer
		params.After = in.After
		params.Watch = nonEmpty(in.Watch)
		params.Source = nonEmpty(in.Source)
		types := make([]apiclient.ShortEventType, len(in.Type))
		for i, t := range in.Type {
			types[i] = apiclient.ShortEventType(t)
		}
		params.Type = nonEmpty(types)
	}
	var out eventPage
	resp, err := a.ListEvents(ctx, params) //nolint:bodyclose // call closes it
	if _, e := call(resp, err, &out); e != nil {
		return nil, e
	}
	if len(out.Items) == 0 || string(out.Items) == "null" {
		out.Items = json.RawMessage("[]")
	}
	return out, nil
}

type addWatchResult struct {
	Watch  json.RawMessage `json:"watch"`
	Change string          `json:"change"`
}

// addWatch creates a watch and never changes one (spec D13): GET, then PUT
// when the name is free, or a dry-run PUT when it is taken.
func addWatch(ctx context.Context, a *apiclient.API, raw json.RawMessage) (any, *toolError) {
	var in map[string]json.RawMessage
	var name string
	if e := decodeArgs(raw, &in); e != nil {
		return nil, e
	}
	if e := decodeArgs(in["name"], &name); e != nil {
		return nil, e
	}
	delete(in, "name")
	body, err := json.Marshal(in)
	if err != nil {
		return nil, invalidArguments(fieldError{Pointer: "", Detail: "the arguments cannot be encoded as a watch"})
	}
	exists := func() *toolError {
		return newProblem(slugWatchExists, "a different watch named %s already exists; nothing was written", name)
	}

	var current json.RawMessage
	resp, err := a.GetWatch(ctx, name) //nolint:bodyclose // call closes it
	_, e := call(resp, err, &current)
	switch {
	case e == nil: // taken: unchanged if the request would not change it
		dry := true
		resp, err = a.PutWatchWithBody(ctx, name, &apiclient.PutWatchParams{DryRun: &dry}, jsonContentType, bytes.NewReader(body)) //nolint:bodyclose // call closes it
		change, e := call(resp, err, nil)
		switch {
		case e != nil:
			return nil, e
		case change == "unchanged":
			return addWatchResult{Watch: current, Change: change}, nil
		case change == "updated":
			return nil, exists()
		case change != "created":
			return nil, newProblem(slugAPIUnreachable, "the API answered the dry run with the change %q; nothing was written", change)
		}
		// created: the watch was deleted since the GET. Take the create
		// path once; its PUT tells again what happened.
	case e.status != 404 || e.slug != restProblemNotFound:
		return nil, e
	}
	return createWatch(ctx, a, name, body, exists)
}

// createWatch writes a watch whose name was free. Another client may create
// the watch between the GET and this PUT; the Angelia-Change header tells
// (spec docs/mcp.md, add_watch).
func createWatch(ctx context.Context, a *apiclient.API, name string, body []byte, exists func() *toolError) (any, *toolError) {
	var written json.RawMessage
	resp, err := a.PutWatchWithBody(ctx, name, &apiclient.PutWatchParams{}, jsonContentType, bytes.NewReader(body)) //nolint:bodyclose // call closes it
	change, e := call(resp, err, &written)
	switch {
	case e != nil:
		return nil, e
	case change == "created" || change == "unchanged":
		return addWatchResult{Watch: written, Change: change}, nil
	case change == "updated":
		return nil, exists()
	}
	return nil, newProblem(slugAPIUnreachable, "the API accepted the write of watch %s but answered with the change %q, so the watch may have been written; read it with list_watches", name, change)
}

type ackResult struct {
	Consumer string `json:"consumer"`
	AckedSeq int64  `json:"acked_seq"`
}

func ackEvent(ctx context.Context, a *apiclient.API, raw json.RawMessage) (any, *toolError) {
	var in struct {
		Consumer string  `json:"consumer"`
		Seq      *int64  `json:"seq,omitempty"`
		EventID  *string `json:"event_id,omitempty"`
	}
	if e := decodeArgs(raw, &in); e != nil {
		return nil, e
	}
	body, err := json.Marshal(struct {
		Seq     *int64  `json:"seq,omitempty"`
		EventID *string `json:"event_id,omitempty"`
	}{in.Seq, in.EventID})
	if err != nil {
		return nil, invalidArguments(fieldError{Pointer: "", Detail: "the arguments cannot be encoded"})
	}
	var c struct {
		AckedSeq int64 `json:"acked_seq"`
	}
	resp, err := a.AckConsumerWithBody(ctx, in.Consumer, jsonContentType, bytes.NewReader(body)) //nolint:bodyclose // call closes it
	if _, e := call(resp, err, &c); e != nil {
		return nil, e
	}
	return ackResult{Consumer: in.Consumer, AckedSeq: c.AckedSeq}, nil
}
