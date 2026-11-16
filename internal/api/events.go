package api

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/ulid"
)

// eventFilter is the filter of GET /v1/events and the event stream. A nil
// set keeps everything.
type eventFilter struct {
	watchIDs map[string]bool
	types    map[string]bool
	sources  map[string]bool
}

// newEventFilter resolves the watch filter: a name means the current id of
// the watch with that name, an id also matches deleted or renamed watches.
func (s *Server) newEventFilter(ctx context.Context, watches []string, types []ShortEventType, sources []string) (*eventFilter, error) {
	f := &eventFilter{}
	if len(watches) > 0 {
		f.watchIDs = map[string]bool{}
		for _, w := range watches {
			if ulid.Valid(w) {
				f.watchIDs[w] = true
				continue
			}
			row, err := s.opts.Store.WatchByName(ctx, w)
			switch {
			case isNotFound(err): // matches nothing
			case err != nil:
				return nil, err
			default:
				f.watchIDs[row.ID] = true
			}
		}
	}
	if len(types) > 0 {
		f.types = map[string]bool{}
		for _, t := range types {
			f.types[string(t)] = true
		}
	}
	if len(sources) > 0 {
		f.sources = map[string]bool{}
		for _, k := range sources {
			f.sources[k] = true
		}
	}
	return f, nil
}

func (f *eventFilter) match(e store.Event) bool {
	if f.watchIDs != nil && !f.watchIDs[e.WatchID] {
		return false
	}
	if f.types != nil && !f.types[event.ShortType(e.Type)] {
		return false
	}
	if f.sources != nil {
		var env struct {
			Data struct {
				Source string `json:"source"`
			} `json:"data"`
		}
		if json.Unmarshal(e.Payload, &env) != nil || !f.sources[env.Data.Source] {
			return false
		}
	}
	return true
}

// scanEvents reads the log after the cursor and returns up to limit events
// that match, with the highest seq it read (the cursor of the next request,
// spec docs/api.md decision 11) and whether the log goes on after it. It
// reads at most budget events, so a page may be empty with more true.
func (s *Server) scanEvents(ctx context.Context, after int64, limit, budget int, f *eventFilter) (items []store.Event, next int64, more bool, err error) {
	const batch = 1000
	next = after
	scanned := 0
scan:
	for scanned < budget {
		n := min(batch, budget-scanned)
		rows, err := s.opts.Store.ListEventsAfter(ctx, next, n)
		if err != nil {
			return nil, 0, false, err
		}
		for _, e := range rows {
			scanned++
			next = e.Seq
			if f.match(e) {
				if items = append(items, e); len(items) == limit {
					break scan
				}
			}
		}
		if len(rows) < n {
			break
		}
	}
	more, err = s.opts.Store.HasEventsAfter(ctx, next)
	return items, next, more, err
}

func eventRecordOf(e store.Event) eventRecordJSON {
	r := eventRecordJSON{Seq: e.Seq, Event: e.Payload}
	if e.RefEventID != "" {
		ref := e.RefEventID
		r.RefEventID = &ref
	}
	return r
}

// cursorCheck refuses a cursor into pruned events: a non-zero after below
// the highest pruned seq gets 410 cursor-expired (spec docs/api.md decision
// 12).
func (s *Server) cursorCheck(ctx context.Context, after int64) error {
	if after <= 0 {
		return nil // after=0 starts at the oldest event kept
	}
	return s.expiredCheck(ctx, after, fmt.Sprintf("events after %d were pruned; resume from oldest_seq", after))
}

// expiredCheck gets 410 cursor-expired, with detail, when pos is below the
// highest pruned seq. Its oldest_seq is max(oldest kept seq, highest pruned
// seq + 1): an event older than the highest pruned one can be kept while a
// delivery of it is pending, and a client resuming from that one would get
// 410 again.
func (s *Server) expiredCheck(ctx context.Context, pos int64, detail string) error {
	pruned, err := s.opts.Store.HighestPrunedSeq(ctx)
	if err != nil || pos >= pruned {
		return err
	}
	oldest := pruned + 1
	first, err := s.opts.Store.ListEventsAfter(ctx, 0, 1)
	if err != nil {
		return err
	}
	if len(first) > 0 && first[0].Seq > oldest {
		oldest = first[0].Seq
	}
	p := problem(SlugCursorExpired, "%s", detail)
	p.OldestSeq = &oldest
	return p
}

// scanEventsBackward reads the log newest first, below before (0: from the
// newest event), and returns up to limit events that match, the lowest and
// highest seq it read (0 and 0 when it read nothing) and whether older events
// are kept. It reads at most budget events, so a page may be empty with more
// true. Reading backwards stops at the oldest event kept: there is no
// expired cursor (spec docs/api.md decision 47).
func (s *Server) scanEventsBackward(ctx context.Context, before int64, limit, budget int, f *eventFilter) (items []store.Event, low, high int64, more bool, err error) {
	const batch = 1000
	next := before
	scanned := 0
scan:
	for scanned < budget {
		n := min(batch, budget-scanned)
		rows, err := s.opts.Store.ListEventsBefore(ctx, next, n)
		if err != nil {
			return nil, 0, 0, false, err
		}
		for _, e := range rows {
			scanned++
			if high == 0 {
				high = e.Seq
			}
			next, low = e.Seq, e.Seq
			if f.match(e) {
				if items = append(items, e); len(items) == limit {
					break scan
				}
			}
		}
		if len(rows) < n {
			break
		}
	}
	if low == 0 {
		return items, 0, 0, false, nil
	}
	older, err := s.opts.Store.ListEventsBefore(ctx, low, 1)
	return items, low, high, len(older) > 0, err
}

// listEventsDesc is GET /v1/events?order=desc.
func (s *Server) listEventsDesc(ctx context.Context, p ListEventsParams, limit int) (eventPageJSON, error) {
	var before int64
	if p.Before != nil {
		before = *p.Before
	}
	f, err := s.newEventFilter(ctx, deref(p.Watch), deref(p.Type), deref(p.Source))
	if err != nil {
		return eventPageJSON{}, err
	}
	var items []store.Event
	var low, high int64
	var more bool
	// before=0 names no position below it: an empty page, never the head of
	// the log (the store reads a beforeSeq of 0 as "from the newest").
	if p.Before == nil || before > 0 {
		if items, low, high, more, err = s.scanEventsBackward(ctx, before, limit, max(s.opts.EventScan, limit), f); err != nil {
			return eventPageJSON{}, err
		}
	}
	page := eventPageJSON{Items: make([]eventRecordJSON, 0, len(items)), NextAfter: high, HasMore: more}
	switch {
	case low > 0:
		page.NextBefore = &low
	case p.Before != nil: // nothing scanned below before
		page.NextBefore, page.NextAfter = &before, max(before-1, 0)
	}
	for _, e := range items {
		page.Items = append(page.Items, eventRecordOf(e))
	}
	return page, nil
}

// ListEvents implements StrictServerInterface.
func (s *Server) ListEvents(ctx context.Context, req ListEventsRequestObject) (ListEventsResponseObject, error) {
	desc := req.Params.Order != nil && *req.Params.Order == ListEventsParamsOrderDesc
	switch {
	case desc && (req.Params.After != nil || req.Params.Consumer != nil):
		return nil, problem(SlugBadRequest, "query parameter order: desc cannot be combined with after or consumer")
	case !desc && req.Params.Before != nil:
		return nil, problem(SlugBadRequest, "query parameter before: needs order=desc")
	}
	limit := 100
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
	}
	if desc {
		page, err := s.listEventsDesc(ctx, req.Params, limit)
		if err != nil {
			return nil, err
		}
		return jsonOK(page), nil
	}
	var after int64
	var f *eventFilter
	if req.Params.Consumer != nil {
		// Exclusive with after and the filters (checkParams): the
		// consumer's cursor and stored filter, as after=<acked_seq> with
		// that filter (spec docs/api.md decision 16).
		c, err := s.readConsumer(ctx, *req.Params.Consumer)
		if err != nil {
			return nil, err
		}
		if err := s.consumerCheck(ctx, c); err != nil {
			return nil, err
		}
		after = c.AckedSeq
		if f, err = s.consumerFilter(ctx, c); err != nil {
			return nil, err
		}
	} else {
		if req.Params.After != nil {
			after = *req.Params.After
		}
		if err := s.cursorCheck(ctx, after); err != nil {
			return nil, err
		}
		var err error
		f, err = s.newEventFilter(ctx, deref(req.Params.Watch), deref(req.Params.Type), deref(req.Params.Source))
		if err != nil {
			return nil, err
		}
	}
	items, next, more, err := s.scanEvents(ctx, after, limit, max(s.opts.EventScan, limit), f)
	if err != nil {
		return nil, err
	}
	page := eventPageJSON{Items: make([]eventRecordJSON, 0, len(items)), NextAfter: next, HasMore: more}
	for _, e := range items {
		page.Items = append(page.Items, eventRecordOf(e))
	}
	return jsonOK(page), nil
}

func deref[T any](p *[]T) []T {
	if p == nil {
		return nil
	}
	return slices.Clone(*p)
}

// GetEvent implements StrictServerInterface.
func (s *Server) GetEvent(ctx context.Context, req GetEventRequestObject) (GetEventResponseObject, error) {
	e, err := s.opts.Store.GetEvent(ctx, req.Id)
	if err != nil {
		return nil, storeProblem(err, "event", req.Id)
	}
	return jsonOK(eventRecordOf(e)), nil
}
