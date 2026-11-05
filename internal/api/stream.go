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

	w         http.ResponseWriter
	rc        *http.ResponseController
	lastWrite time.Time
	nextAuth  time.Time
}

// errStreamClosed ends a stream on purpose: the client went away, the
// server drains, or the token was revoked.
var errStreamClosed = errors.New("api: stream closed")

// VisitStreamEventsResponse implements StreamEventsResponseObject.
func (e *eventStream) VisitStreamEventsResponse(w http.ResponseWriter) error {
	s := e.s
	e.w, e.rc = w, http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	e.nextAuth = time.Now().Add(s.opts.AuthRecheck)
	if err := e.send([]byte("retry: 5000\n\n")); err != nil {
		return nil //nolint:nilerr // the client went away: nothing to report
	}
	ticker := time.NewTicker(s.opts.StreamPoll)
	defer ticker.Stop()
	cursor := e.after
	for {
		if err := e.flushNew(&cursor); err != nil {
			if !errors.Is(err, errStreamClosed) && e.ctx.Err() == nil {
				s.log.ErrorContext(e.ctx, "event stream", "request_id", stateFrom(e.ctx).id, "error", err)
			}
			return nil // the status is sent: close the stream
		}
		select {
		case <-e.ctx.Done():
			return nil
		case <-s.stop:
			return nil
		case <-ticker.C:
		}
		if err := e.idle(); err != nil {
			return nil //nolint:nilerr // closed on purpose
		}
	}
}

func (e *eventStream) send(b []byte) error {
	_ = e.rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout)) // not every writer supports deadlines
	if _, err := e.w.Write(b); err != nil {
		return errStreamClosed
	}
	e.lastWrite = time.Now()
	if e.rc.Flush() != nil {
		return errStreamClosed
	}
	return nil
}

// idle runs between two reads of the log: it ends the stream when the client
// went away, the server drains or the token was revoked, and sends a
// keep-alive comment once the stream has been silent for KeepAlive.
func (e *eventStream) idle() error {
	select {
	case <-e.ctx.Done():
		return errStreamClosed
	case <-e.s.stop:
		return errStreamClosed
	default:
	}
	if !time.Now().Before(e.nextAuth) {
		p, err := e.s.opts.Auth.Authenticate(e.ctx, e.token)
		if err != nil || p.Scope < ScopeRead {
			return errStreamClosed // revoked
		}
		e.nextAuth = time.Now().Add(e.s.opts.AuthRecheck)
	}
	if time.Since(e.lastWrite) >= e.s.opts.KeepAlive {
		return e.send([]byte(": keep-alive\n\n"))
	}
	return nil
}

// flushNew sends every matching event after the cursor and moves it. A long
// catch-up reads the log in bounded scans and runs idle between them, so it
// stops on a drain and stays alive while nothing matches.
func (e *eventStream) flushNew(cursor *int64) error {
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
			if err := e.send(b.Bytes()); err != nil {
				return err
			}
		}
		*cursor = next
		if !more {
			return nil
		}
		if err := e.idle(); err != nil {
			return err
		}
	}
}
