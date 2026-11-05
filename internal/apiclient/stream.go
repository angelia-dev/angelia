package apiclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// maxStreamLine bounds one SSE line: an event record is one line of JSON.
const maxStreamLine = 4 << 20

const (
	// defaultRetry is the reconnect delay until the server sends a retry hint.
	defaultRetry = 5 * time.Second
	// minRetry and maxRetry clamp the server's retry hint and Retry-After.
	minRetry = 100 * time.Millisecond
	maxRetry = 5 * time.Minute
	// DefaultIdleTimeout is three times the 15 s keep-alive of the server.
	DefaultIdleTimeout = 45 * time.Second
)

// StreamOptions tune Stream.
type StreamOptions struct {
	// Reconnect makes Stream reconnect with Last-Event-ID after the stream
	// ends or breaks, forever, waiting the server's retry hint. Without it
	// Stream returns when the stream ends. A 4xx answer other than 429 (401,
	// 410, ...) is never retried.
	Reconnect bool
	// RetryDelay overrides the reconnect delay (the server's retry hint
	// otherwise, 5 seconds before one arrives).
	RetryDelay time.Duration
	// IdleTimeout reconnects when nothing, not even a keep-alive comment,
	// arrived for this long. Zero means DefaultIdleTimeout.
	IdleTimeout time.Duration
	// OnReconnect is called before each wait: err is why the connection
	// ended (nil for a clean end of stream) and wait how long Stream sleeps.
	OnReconnect func(err error, wait time.Duration)
}

func clampRetry(d time.Duration) time.Duration {
	return min(max(d, minRetry), maxRetry)
}

// Stream opens GET /v1/events/stream and calls fn for every event record, in
// order, until ctx is done, fn returns an error, or (without Reconnect) the
// stream ends. With Reconnect it retries connection errors, idle connections,
// 5xx and 429 answers (honouring Retry-After) without limit.
func (a *API) Stream(ctx context.Context, params StreamEventsParams, opts StreamOptions, fn func(EventRecord) error) error {
	retry := defaultRetry
	if opts.RetryDelay > 0 {
		retry = opts.RetryDelay
	}
	idle := opts.IdleTimeout
	if idle <= 0 {
		idle = DefaultIdleTimeout
	}
	for {
		last, hint, retryAfter, err := a.streamOnce(ctx, &params, idle, fn)
		if last != nil {
			params.LastEventID = last // a reconnect resumes after the last event seen
		}
		if hint > 0 && opts.RetryDelay <= 0 {
			retry = clampRetry(hint)
		}
		var apiErr *Error
		switch {
		case ctx.Err() != nil:
			return nil // the caller stopped the stream
		case errors.Is(err, errStop):
			return unwrapStop(err)
		case errors.As(err, &apiErr) && !retryableStatus(apiErr.Status):
			return err
		case !opts.Reconnect:
			return err
		}
		wait := retry
		if retryAfter > 0 {
			wait = clampRetry(retryAfter)
		}
		if opts.OnReconnect != nil {
			opts.OnReconnect(err, wait)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

func retryableStatus(code int) bool {
	return code >= 500 || code == http.StatusTooManyRequests
}

// errStop marks an error that ends Stream even with Reconnect.
var errStop = errors.New("stream stopped")

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

// parseRetryAfter reads a Retry-After header in seconds (the HTTP-date form
// is not used by the server).
func parseRetryAfter(h string) time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && n > 0 {
		return time.Duration(min(n, int(maxRetry/time.Second))) * time.Second
	}
	return 0
}

// splitSSE splits lines ended by LF, CRLF or a bare CR, as the SSE
// specification allows.
func splitSSE(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		if data[i] == '\r' {
			if i+1 == len(data) && !atEOF {
				return 0, nil, nil // maybe a CRLF: read more
			}
			if i+1 < len(data) && data[i+1] == '\n' {
				return i + 2, data[:i], nil
			}
		}
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// streamOnce reads one connection. It returns the seq of the last event
// delivered, the server's retry hint, a Retry-After from an error answer and
// why the connection ended (nil for a clean end of stream).
func (a *API) streamOnce(ctx context.Context, params *StreamEventsParams, idle time.Duration, fn func(EventRecord) error) (last *Seq, hint, retryAfter time.Duration, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var idled atomic.Bool
	timer := time.AfterFunc(idle, func() { idled.Store(true); cancel() })
	defer timer.Stop()

	resp, err := a.stream.StreamEvents(ctx, params, func(_ context.Context, req *http.Request) error {
		req.Header.Set("Accept", "text/event-stream")
		return nil
	})
	if err != nil {
		if idled.Load() {
			err = fmt.Errorf("no data for %s", idle)
		}
		return nil, 0, 0, fmt.Errorf("open event stream: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		ra := parseRetryAfter(resp.Header.Get("Retry-After"))
		_, derr := Decode(resp, nil, nil) // closes the body; builds the *Error
		if derr == nil {
			derr = &Error{Status: resp.StatusCode}
		}
		return nil, 0, ra, derr
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), maxStreamLine)
	sc.Split(splitSSE)
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
		timer.Reset(idle)
		line := sc.Text()
		if line == "" {
			if err := flush(); err != nil {
				return last, hint, 0, err
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
				hint = time.Duration(min(ms, int(maxRetry/time.Millisecond))) * time.Millisecond
			}
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		switch {
		case errors.Is(err, bufio.ErrTooLong):
			return last, hint, 0, &stopError{err: fmt.Errorf("event stream: a line is longer than %d bytes", maxStreamLine)}
		case idled.Load():
			return last, hint, 0, fmt.Errorf("event stream: no data for %s", idle)
		}
		return last, hint, 0, fmt.Errorf("read event stream: %w", err)
	}
	return last, hint, 0, nil
}
