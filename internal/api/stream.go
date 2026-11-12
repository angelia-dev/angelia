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
	after, f, err := s.streamStart(ctx, req.Params)
	if err != nil {
		return nil, err // 404 or 410 before any stream starts
	}
	if !s.acquireStream() {
		return nil, problem(SlugUnavailable, "too many open event streams; retry later").
			withHeader("Retry-After", fmt.Sprint(RetryAfterSeconds))
	}
	return &eventStream{s: s, ctx: ctx, after: after, filter: f, token: stateFrom(ctx).token}, nil
}

// streamStart returns where a stream starts and its filter. Last-Event-ID
// takes precedence over after and over a consumer's acked_seq. A consumer is
// read once, here: the open stream keeps its filter as it is now (spec
// docs/api.md decision 16).
func (s *Server) streamStart(ctx context.Context, p StreamEventsParams) (int64, *eventFilter, error) {
	if p.Consumer == nil {
		var after int64
		if p.After != nil {
			after = *p.After
		}
		if p.LastEventID != nil {
			after = *p.LastEventID
		}
		if err := s.cursorCheck(ctx, after); err != nil {
			return 0, nil, err
		}
		f, err := s.newEventFilter(ctx, deref(p.Watch), deref(p.Type), deref(p.Source))
		return after, f, err
	}
	c, err := s.readConsumer(ctx, *p.Consumer)
	if err != nil {
		return 0, nil, err
	}
	after := c.AckedSeq
	if p.LastEventID != nil {
		after = *p.LastEventID
		err = s.cursorCheck(ctx, after)
	} else {
		err = s.consumerCheck(ctx, c)
	}
	if err != nil {
		return 0, nil, err
	}
	f, err := s.consumerFilter(ctx, c)
	return after, f, err
}

// acquireStream takes one of the MaxStreams slots; releaseStream gives it
// back when the stream ends. A caller that got a slot must hand it to an
// eventStream, whose VisitStreamEventsResponse releases it first thing.
func (s *Server) acquireStream() bool {
	for {
		n := s.streams.Load()
		if n >= int64(s.opts.MaxStreams) {
			return false
		}
		if s.streams.CompareAndSwap(n, n+1) {
			break
		}
	}
	if s.opts.Metrics != nil {
		s.opts.Metrics.EventStreamOpened()
	}
	return true
}

func (s *Server) releaseStream() {
	s.streams.Add(-1)
	if s.opts.Metrics != nil {
		s.opts.Metrics.EventStreamClosed()
	}
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
	defer s.releaseStream()
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
	waiter := s.hub.newWaiter()
	defer waiter.release()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	cursor := e.after
	woken := true // read the log once before waiting
	for {
		if woken {
			if err := e.flushNew(&cursor); err != nil {
				if !errors.Is(err, errStreamClosed) && e.ctx.Err() == nil {
					s.log.ErrorContext(e.ctx, "event stream", "request_id", stateFrom(e.ctx).id, "error", err)
				}
				return nil // the status is sent: close the stream
			}
		}
		// Caught up: the shared poller wakes the stream when the log goes
		// on after the cursor; the timer is only for the keep-alive and
		// the token recheck, and reads nothing.
		waiter.arm(cursor)
		timer.Reset(e.nextIdle())
		woken = false
		select {
		case <-e.ctx.Done():
			return nil
		case <-s.stop:
			return nil
		case <-waiter.wake:
			woken = true
		case <-timer.C:
		}
		if err := e.idle(); err != nil {
			return nil //nolint:nilerr // closed on purpose
		}
	}
}

// nextIdle is how long the stream may sleep before idle has work: the next
// keep-alive or token recheck.
func (e *eventStream) nextIdle() time.Duration {
	next := e.nextAuth
	if ka := e.lastWrite.Add(e.s.opts.KeepAlive); ka.Before(next) {
		next = ka
	}
	return max(time.Until(next), time.Millisecond)
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
