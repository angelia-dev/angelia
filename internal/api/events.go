package api

import (
	"context"
	"encoding/json"
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
// 12). Its oldest_seq is max(oldest kept seq, highest pruned seq + 1): an
// event older than the highest pruned one can be kept while a delivery of it
// is pending, and a client resuming from that one would get 410 again.
func (s *Server) cursorCheck(ctx context.Context, after int64) error {
	if after <= 0 {
		return nil // after=0 starts at the oldest event kept
	}
	pruned, err := s.opts.Store.HighestPrunedSeq(ctx)
	if err != nil || after >= pruned {
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
	p := problem(SlugCursorExpired, "events after %d were pruned; resume from oldest_seq", after)
	p.OldestSeq = &oldest
	return p
}

// ListEvents implements StrictServerInterface.
func (s *Server) ListEvents(ctx context.Context, req ListEventsRequestObject) (ListEventsResponseObject, error) {
	var after int64
	if req.Params.After != nil {
		after = *req.Params.After
	}
	if err := s.cursorCheck(ctx, after); err != nil {
		return nil, err
	}
	limit := 100
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
	}
	f, err := s.newEventFilter(ctx, deref(req.Params.Watch), deref(req.Params.Type), deref(req.Params.Source))
	if err != nil {
		return nil, err
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
