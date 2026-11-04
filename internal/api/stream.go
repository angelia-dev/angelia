package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// StreamEvents implements StrictServerInterface: GET /v1/events/stream as
// server-sent events (spec docs/api.md decision 15).
func (s *Server) StreamEvents(ctx context.Context, req StreamEventsRequestObject) (StreamEventsResponseObject, error) {
	var after int64
	if req.Params.After != nil {
		after = *req.Params.After
	}
	if req.Params.LastEventID != nil { // takes precedence over after
		after = *req.Params.LastEventID
	}
	if err := s.cursorCheck(ctx, after); err != nil {
		return nil, err // 410 before any stream starts
	}
	f, err := s.newEventFilter(ctx, deref(req.Params.Watch), deref(req.Params.Type), deref(req.Params.Source))
	if err != nil {
		return nil, err
	}
	return &eventStream{s: s, ctx: ctx, after: after, filter: f, token: stateFrom(ctx).token}, nil
}

// eventStream writes the stream. It ends when the client goes away, the
// server drains, or the token stops authenticating.
type eventStream struct {
	s      *Server
	ctx    context.Context
	after  int64
	filter *eventFilter
	token  string
}

// errStreamClosed ends a stream on purpose.
var errStreamClosed = errors.New("api: stream closed")

// VisitStreamEventsResponse implements StreamEventsResponseObject.
func (e *eventStream) VisitStreamEventsResponse(w http.ResponseWriter) error {
	s := e.s
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	lastWrite := time.Now()
	send := func(b []byte) error {
		_ = rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout)) // not every writer supports deadlines
		if _, err := w.Write(b); err != nil {
			return err
		}
		lastWrite = time.Now()
		return rc.Flush()
	}
	if err := send([]byte("retry: 5000\n\n")); err != nil {
		return nil //nolint:nilerr // the client went away: nothing to report
	}
	ticker := time.NewTicker(s.opts.StreamPoll)
	defer ticker.Stop()
	nextAuth := time.Now().Add(s.opts.AuthRecheck)
	cursor := e.after
	for {
		if err := e.flushNew(&cursor, send); err != nil {
			if e.ctx.Err() != nil || errors.Is(err, errStreamClosed) {
				return nil
			}
			s.log.ErrorContext(e.ctx, "event stream", "request_id", stateFrom(e.ctx).id, "error", err)
			return nil // the status is sent: close the stream
		}
		select {
		case <-e.ctx.Done():
			return nil
		case <-s.stop:
			return nil
		case <-ticker.C:
		}
		if !time.Now().Before(nextAuth) {
			p, err := s.opts.Auth.Authenticate(e.ctx, e.token)
			if err != nil || p.Scope < ScopeRead {
				return nil // revoked: close the stream
			}
			nextAuth = time.Now().Add(s.opts.AuthRecheck)
		}
		if time.Since(lastWrite) >= s.opts.KeepAlive {
			if err := send([]byte(": keep-alive\n\n")); err != nil {
				return nil //nolint:nilerr // the client went away
			}
		}
	}
}

// flushNew sends every matching event after the cursor and moves it.
func (e *eventStream) flushNew(cursor *int64, send func([]byte) error) error {
	for {
		items, next, more, err := e.s.scanEvents(e.ctx, *cursor, 100, e.s.opts.EventScan, e.filter)
		if err != nil {
			return err
		}
		for _, ev := range items {
			var b bytes.Buffer
			fmt.Fprintf(&b, "id: %d\ndata: ", ev.Seq)
			enc := json.NewEncoder(&b)
			enc.SetEscapeHTML(false)
			if err := enc.Encode(eventRecordOf(ev)); err != nil { // ends the line
				return err
			}
			b.WriteString("\n")
			if err := send(b.Bytes()); err != nil {
				return errStreamClosed
			}
		}
		*cursor = next
		if !more {
			return nil
		}
	}
}
