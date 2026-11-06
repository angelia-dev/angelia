// Package sink defines the Sink contract (architecture.md section 3.5, ADR
// 0016) and a Registry of Sink implementations by kind. Implementations
// live in subpackages, one per kind (internal/sink/webhook, ...).
//
// A sink sends one delivery; the dispatcher (internal/dispatch) owns
// leasing, retries and the dead state. A sink tells the dispatcher how an
// attempt failed through its error: wrap it with Permanent, or return
// HTTPStatus for an HTTP response, and the dispatcher stops retrying.
package sink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/secrets"
)

// Sink delivers events to one kind of destination.
//
// Implementations that do HTTP receive their *http.Client through their
// constructor and never use http.DefaultClient, so the netguard client can
// be substituted. They must not follow redirects (the client's
// CheckRedirect returns http.ErrUseLastResponse): a 3xx is reported with
// HTTPStatus, which makes it permanent.
//
// Errors are stored in delivery.last_error and logged. They must not carry
// configuration secrets (tokens, passwords, signing keys). The dispatcher
// strips the path and query of the URLs in them (RedactURL), because chat
// webhook URLs carry their secret in the path, but it cannot find anything
// else.
type Sink interface {
	// Kind is the registry key: "webhook", "github-dispatch", ...
	Kind() string
	// Validate checks a sink's configuration when it is stored. It does no
	// network I/O.
	Validate(cfg Config) error
	// Deliver sends one attempt of one delivery. It must honour ctx: the
	// dispatcher cancels it before the delivery's lease expires, so that two
	// dispatchers never send the same delivery at the same time. A nil
	// error means delivered. Any error is retried with backoff, unless
	// IsPermanent reports it permanent.
	Deliver(ctx context.Context, req Request) (Result, error)
}

// Config is a sink's configuration (sink.config_json),
// opaque to the core: only the sink of its kind interprets it.
type Config json.RawMessage

// Request is one attempt of one delivery.
type Request struct {
	// Config is the configuration of the delivery's sink, read when the
	// delivery was leased.
	Config Config
	// Event is the release event, the same on every attempt.
	Event ReleaseEvent
	// Rendered is the message built from the subscription's template, or
	// the CloudEvents JSON when it has none.
	Rendered Rendered
	// Secrets holds the values of the secret references in Config, resolved
	// by the core for this attempt (ADR 0023 section 4, ADR 0024): the key is
	// the RFC 6901 JSON Pointer of the reference key in Config, such as
	// "/secret_env", the value is the secret. Every reference resolved, or
	// Deliver is not called. Sinks read secrets only from here, never from
	// the environment, and never log, store or return them. A Value prints
	// as [REDACTED] under fmt and slog; Reveal it only to sign or build the
	// request. A stored secret bound to hosts was resolved for the URL the
	// kind marks x-angelia-destination, so a sink sends it only there and
	// follows no redirect (ADR 0017 §7, ADR 0024 §2). A sink that sends a
	// bound secret anywhere else (a redirect, a second request) needs the
	// per-request netguard check of ADR 0024 §2 first.
	Secrets map[string]secrets.Value
	// IdempotencyKey is the same on every attempt and replay of a delivery:
	// IdempotencyKey(event id, subscription id). Sinks pass it on, for
	// example as a header, so that consumers can drop duplicates.
	IdempotencyKey string
	// DeliveryID, SubscriptionID and SinkID identify the delivery and its
	// sink, for logs.
	DeliveryID, SubscriptionID, SinkID string
	// Attempt counts from 1. An attempt cut short by a crash counts.
	Attempt int64
}

// ReleaseEvent is the event a delivery carries.
type ReleaseEvent struct {
	// JSON is the stored CloudEvents JSON (event.payload_json), byte for
	// byte; Envelope is the same event decoded.
	JSON     json.RawMessage
	Envelope event.Envelope
}

// Rendered is the message to send.
type Rendered struct {
	Body        []byte
	ContentType string
	// Templated is true when Body comes from the subscription's template,
	// false when it is the CloudEvents JSON of the event.
	Templated bool
}

// CloudEventsContentType is the media type of a CloudEvents JSON event in
// structured mode, the default message.
const CloudEventsContentType = "application/cloudevents+json; charset=utf-8"

// Result describes a delivered attempt, for logs.
type Result struct {
	// StatusCode is the HTTP status of the response; 0 when the sink does
	// not speak HTTP.
	StatusCode int
}

// IdempotencyKey is the idempotency key of the delivery of an event to a
// subscription: "<event id>:<subscription id>" (architecture.md 3.5).
func IdempotencyKey(eventID, subscriptionID string) string {
	return eventID + ":" + subscriptionID
}

// permanentError marks an error that retrying cannot fix.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent marks err as permanent: the dispatcher sets the delivery dead
// at once instead of retrying. Permanent(nil) is nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

// IsPermanent reports whether err, or an error it wraps, was marked with
// Permanent.
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

// StatusError is an HTTP response that is not a success.
type StatusError struct {
	Code int
	// RetryAfter is the response's Retry-After delay; 0 when absent. The
	// dispatcher waits at least this long before the next attempt.
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	if redirect(e.Code) {
		return fmt.Sprintf("sink: HTTP %d %s: redirect not followed; update the sink URL", e.Code, http.StatusText(e.Code))
	}
	return fmt.Sprintf("sink: HTTP %d %s", e.Code, http.StatusText(e.Code))
}

func redirect(code int) bool { return code >= 300 && code <= 399 }

// HTTPStatus classifies an HTTP response status. A 2xx is a success (nil).
// Permanent: a 4xx other than 408 Request Timeout and 429 Too Many
// Requests (the request itself is wrong: bad URL, credentials or payload,
// and sending it again cannot help), and a 3xx (sinks do not follow
// redirects, so the configured URL must change). Retried: 5xx, 408, 429,
// and 1xx or any other unexpected code. retryAfter is the parsed
// Retry-After header, or 0.
func HTTPStatus(code int, retryAfter time.Duration) error {
	if code >= 200 && code <= 299 {
		return nil
	}
	err := &StatusError{Code: code, RetryAfter: max(retryAfter, 0)}
	clientError := code >= 400 && code <= 499 && code != http.StatusRequestTimeout && code != http.StatusTooManyRequests
	if clientError || redirect(code) {
		return Permanent(err)
	}
	return err
}

// RetryAfter returns the Retry-After delay carried by err (a StatusError),
// or 0.
func RetryAfter(err error) time.Duration {
	var se *StatusError
	if errors.As(err, &se) {
		return se.RetryAfter
	}
	return 0
}

var (
	// ErrUnknownKind is returned when no sink is registered for a kind.
	ErrUnknownKind = errors.New("sink: unknown kind")
	// ErrDuplicateKind is returned when a kind is registered twice.
	ErrDuplicateKind = errors.New("sink: kind already registered")
)
