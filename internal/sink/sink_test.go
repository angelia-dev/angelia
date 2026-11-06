package sink

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Permanent errors: 4xx except 408 and 429 (issue #107).
func TestHTTPStatus(t *testing.T) {
	for _, tc := range []struct {
		code     int
		ok, perm bool
	}{
		{200, true, false},
		{204, true, false},
		{299, true, false},
		{400, false, true},
		{401, false, true},
		{403, false, true},
		{404, false, true},
		{410, false, true},
		{422, false, true},
		{499, false, true},
		{408, false, false},
		{429, false, false},
		{500, false, false},
		{502, false, false},
		{503, false, false},
		{301, false, true},
		{302, false, true},
		{308, false, true},
		{100, false, false},
	} {
		err := HTTPStatus(tc.code, time.Minute)
		if (err == nil) != tc.ok {
			t.Errorf("HTTPStatus(%d) = %v, want ok=%v", tc.code, err, tc.ok)
			continue
		}
		if err == nil {
			continue
		}
		if IsPermanent(err) != tc.perm {
			t.Errorf("IsPermanent(HTTPStatus(%d)) = %v, want %v", tc.code, !tc.perm, tc.perm)
		}
		var se *StatusError
		if !errors.As(err, &se) || se.Code != tc.code {
			t.Errorf("HTTPStatus(%d) does not wrap a StatusError: %v", tc.code, err)
		}
	}
	if got := RetryAfter(HTTPStatus(429, time.Minute)); got != time.Minute {
		t.Errorf("RetryAfter(429) = %v", got)
	}
	if got := RetryAfter(HTTPStatus(503, -time.Second)); got != 0 {
		t.Errorf("negative Retry-After = %v, want 0", got)
	}
	if got := RetryAfter(errors.New("x")); got != 0 {
		t.Errorf("RetryAfter(plain) = %v", got)
	}
	if got := HTTPStatus(404, 0).Error(); got != "sink: HTTP 404 Not Found" {
		t.Errorf("message = %q", got)
	}
}

func TestRedirectMessage(t *testing.T) {
	err := HTTPStatus(301, 0)
	if !IsPermanent(err) || err.Error() != "sink: HTTP 301 Moved Permanently: redirect not followed; update the sink URL" {
		t.Errorf("301: %v permanent=%v", err, IsPermanent(err))
	}
	for _, code := range []int{100, 600, 0} {
		if err := HTTPStatus(code, 0); err == nil || IsPermanent(err) {
			t.Errorf("unexpected code %d: %v, want retried", code, err)
		}
	}
}

func TestRedactURL(t *testing.T) {
	const slack = "https://hooks.chat.example/services/team/channel/not-a-real-token" //nolint:gosec // G101: synthetic placeholder URL
	ue := &url.Error{Op: "Post", URL: slack, Err: errors.New("dial tcp: i/o timeout")}
	for _, tc := range []struct {
		err  error
		want string
	}{
		{ue, `Post "https://hooks.chat.example/[redacted]": dial tcp: i/o timeout`},
		{fmt.Errorf("webhook: %w", ue), `webhook: Post "https://hooks.chat.example/[redacted]": dial tcp: i/o timeout`},
		{errors.New("posting to https://user:pw@discord.com/api/webhooks/1/abc?wait=true failed"), "posting to https://discord.com/[redacted] failed"},
		{errors.New("GET https://example.com/ ok, and http://example.org:8080 too"), "GET https://example.com ok, and http://example.org:8080 too"},
		{errors.New("bad url unix:///run/x.sock"), "bad url unix://[redacted]"},
		// A colon after a URL with a port is punctuation (#108 sink errors).
		{errors.New("webhook: http://127.0.0.1:8080: HTTP 500 Internal Server Error"), "webhook: http://127.0.0.1:8080: HTTP 500 Internal Server Error"},
		{errors.New("webhook: https://h.example/p/abc: connection refused"), "webhook: https://h.example/[redacted]: connection refused"},
		{errors.New("no url here"), "no url here"},
		{nil, ""},
	} {
		if got := RedactURL(tc.err); got != tc.want {
			t.Errorf("RedactURL(%v)\n got %q\nwant %q", tc.err, got, tc.want)
		}
		if strings.Contains(RedactURL(tc.err), "XXXX") || strings.Contains(RedactURL(tc.err), "abc") {
			t.Errorf("secret left in %q", RedactURL(tc.err))
		}
	}
	// A url.Error whose URL has a space is still redacted.
	spaced := &url.Error{Op: "Post", URL: "https://hooks.chat.example/services/a b", Err: errors.New("x")}
	if got := RedactURL(spaced); strings.Contains(got, "services") {
		t.Errorf("spaced URL: %q", got)
	}
}

func TestPermanent(t *testing.T) {
	if Permanent(nil) != nil {
		t.Error("Permanent(nil) != nil")
	}
	base := errors.New("bad config")
	err := fmt.Errorf("deliver: %w", Permanent(base))
	if !IsPermanent(err) || !errors.Is(err, base) || err.Error() != "deliver: bad config" {
		t.Errorf("wrapped permanent: %v perm=%v", err, IsPermanent(err))
	}
	if IsPermanent(base) || IsPermanent(nil) {
		t.Error("plain error reported permanent")
	}
}

func TestIdempotencyKey(t *testing.T) {
	if got := IdempotencyKey("01JBX2A7C8D9E0F1G2H3J4K5M6", "sub1"); got != "01JBX2A7C8D9E0F1G2H3J4K5M6:sub1" {
		t.Errorf("IdempotencyKey = %q", got)
	}
}

type fakeSink struct{ kind string }

func (f *fakeSink) Kind() string          { return f.kind }
func (f *fakeSink) Validate(Config) error { return nil }
func (f *fakeSink) Deliver(context.Context, Request) (Result, error) {
	return Result{}, nil
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&fakeSink{kind: "webhook"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(&fakeSink{kind: "webhook"}); !errors.Is(err, ErrDuplicateKind) {
		t.Errorf("duplicate: %v", err)
	}
	if err := r.Register(&fakeSink{}); err == nil {
		t.Error("empty kind accepted")
	}
	if err := r.Register(nil); err == nil {
		t.Error("nil accepted")
	}
	var typedNil *fakeSink
	if err := r.Register(typedNil); err == nil {
		t.Error("typed nil accepted")
	}
	if s, err := r.Get("webhook"); err != nil || s.Kind() != "webhook" {
		t.Errorf("Get = %v, %v", s, err)
	}
	if _, err := r.Get("nats"); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("Get unknown: %v", err)
	}
	var zero Registry
	if err := zero.Register(&fakeSink{kind: "nats"}); err != nil {
		t.Errorf("zero-value registry: %v", err)
	}
}
