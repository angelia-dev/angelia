package apiclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxStreamLine bounds one SSE line: an event record is one line of JSON.
const maxStreamLine = 4 << 20

// defaultRetry is the reconnect delay until the server sends a retry hint.
const defaultRetry = 5 * time.Second

// StreamOptions tune Stream.
type StreamOptions struct {
	// Reconnect makes Stream reconnect with Last-Event-ID after the stream
	// ends or breaks, waiting the server's retry hint. Without it Stream
	// returns when the stream ends.
	Reconnect bool
	// RetryDelay overrides the reconnect delay (the server's retry hint
	// otherwise, 5 seconds before one arrives).
	RetryDelay time.Duration
}

// Stream opens GET /v1/events/stream and calls fn for every event record, in
// order, until ctx is done, fn returns an error, or (without Reconnect) the
// stream ends. A refused request (401, 410, ...) is returned as an *Error and
// is never retried.
func (a *API) Stream(ctx context.Context, params StreamEventsParams, opts StreamOptions, fn func(EventRecord) error) error {
	retry := defaultRetry
	if opts.RetryDelay > 0 {
		retry = opts.RetryDelay
	}
	for {
		last, hint, err := a.streamOnce(ctx, &params, fn)
		if last != nil {
			params.LastEventID = last // a reconnect resumes after the last event seen
		}
		if hint > 0 && opts.RetryDelay <= 0 {
			retry = hint
		}
		var apiErr *Error
		switch {
		case ctx.Err() != nil:
			return nil // the caller stopped the stream
		case errors.As(err, &apiErr), errors.Is(err, errStop):
			return unwrapStop(err)
		case !opts.Reconnect:
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(retry):
		}
	}
}

// errStop wraps an error returned by the caller's callback.
var errStop = errors.New("stream stopped by callback")

type stopError struct{ err error }

func (e *stopError) Error() string { return e.err.Error() }
func (e *stopError) Unwrap() []error {
	return []error{errStop, e.err}
}

func unwrapStop(err error) error {
	var s *stopError
	if errors.As(err, &s) {
		return s.err
	}
	return err
}

// streamOnce reads one connection. It returns the seq of the last event
// delivered, the server's retry hint and why the connection ended (nil for a
// clean end of stream).
func (a *API) streamOnce(ctx context.Context, params *StreamEventsParams, fn func(EventRecord) error) (last *Seq, hint time.Duration, err error) {
	resp, err := a.stream.StreamEvents(ctx, params, func(_ context.Context, req *http.Request) error {
		req.Header.Set("Accept", "text/event-stream")
		return nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("open event stream: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, derr := Decode(resp, nil, nil) // closes the body; builds the *Error
		if derr == nil {
			derr = &Error{Status: resp.StatusCode}
		}
		return nil, 0, derr
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), maxStreamLine)
	var data []string
	var id string
	flush := func() error {
		defer func() { data, id = nil, "" }()
		if len(data) == 0 {
			return nil
		}
		var rec EventRecord
		if err := json.Unmarshal([]byte(strings.Join(data, "\n")), &rec); err != nil {
			// Retrying would read the same message again.
			return &stopError{err: fmt.Errorf("decode stream event: %w", err)}
		}
		if id != "" {
			if n, perr := strconv.ParseInt(id, 10, 64); perr == nil {
				rec.Seq = n
			}
		}
		if err := fn(rec); err != nil {
			return &stopError{err: err}
		}
		s := rec.Seq
		last = &s
		return nil
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if err := flush(); err != nil {
				return last, hint, err
			}
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		if field == "" { // a comment, e.g. the keep-alive
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			id = value
		case "data":
			data = append(data, value)
		case "retry":
			if ms, perr := strconv.Atoi(value); perr == nil && ms > 0 {
				hint = time.Duration(ms) * time.Millisecond
			}
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return last, hint, fmt.Errorf("read event stream: %w", err)
	}
	return last, hint, nil
}
