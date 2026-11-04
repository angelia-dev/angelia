package wasm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	extism "github.com/extism/go-sdk"

	"github.com/angelia-dev/angelia/internal/netguard"
)

// HostNamespace is the Wasm import module of the host functions.
const HostNamespace = "angelia:host/v0"

const (
	maxRequestBytes = 1 << 20 // one http_request, JSON and base64 body included
	maxLogEntry     = 64 << 10
	maxLogMessage   = 4 << 10
)

// HostNotAllowedError reports a request, or a redirect hop, to a host that
// is not in allowed_hosts. It wraps ErrHostNotAllowed.
type HostNotAllowedError struct {
	Host    string
	Allowed []string
}

func (e *HostNotAllowedError) Error() string {
	return fmt.Sprintf("wasm: host %q not in allowed_hosts %q", e.Host, e.Allowed)
}

// Unwrap lets errors.Is match ErrHostNotAllowed.
func (e *HostNotAllowedError) Unwrap() error { return ErrHostNotAllowed }

// allowlistTransport refuses any hop, the first one and every redirect,
// whose host is not allowed, before the base transport sees it. It records
// the refusal in the call's state, so the call fails even if the plugin
// ignores the error.
type allowlistTransport struct {
	base    http.RoundTripper
	allowed map[string]bool
	creds   map[string]Credential // by normalised host
}

func (t *allowlistTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := t.check(r.URL); err != nil {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		if cs, ok := r.Context().Value(callKey{}).(*callState); ok {
			cs.violate(err)
		}
		return nil, err
	}
	out, credentialed, err := t.inject(r)
	if err != nil {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return nil, err
	}
	resp, err := t.base.RoundTrip(out)
	if err == nil && credentialed {
		if err := checkEncoding(resp); err != nil {
			return nil, err
		}
	}
	return resp, err
}

func (t *allowlistTransport) check(u *url.URL) error {
	if s := strings.ToLower(u.Scheme); s != "http" && s != "https" {
		return fmt.Errorf("%w: scheme %q", ErrHostNotAllowed, u.Scheme)
	}
	// allowed_hosts holds ASCII names only: a non-ASCII name, which the
	// transport would IDNA-encode, is never compared loosely.
	if h := normHost(u.Hostname()); h == "" || !isASCII(h) || !t.allowed[h] {
		return &HostNotAllowedError{Host: u.Hostname(), Allowed: sortedKeys(t.allowed)}
	}
	return nil
}

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func (s *Source) hostFunctions() []extism.HostFunction {
	ptr := []extism.ValueType{extism.ValueTypePTR}
	httpFn := extism.NewHostFunctionWithStack("http_request", s.hostHTTP, ptr, ptr)
	httpFn.SetNamespace(HostNamespace)
	logFn := extism.NewHostFunctionWithStack("log", s.hostLog, ptr, nil)
	logFn.SetNamespace(HostNamespace)
	return []extism.HostFunction{httpFn, logFn}
}

// hostHTTP implements http_request(request) -> response, both JSON blocks
// in plugin memory. A failure to talk to the plugin's memory traps.
func (s *Source) hostHTTP(ctx context.Context, p *extism.CurrentPlugin, stack []uint64) {
	var resp wireHTTPResponse
	if n, err := p.Length(stack[0]); err != nil {
		panic(fmt.Errorf("http_request: %w", err))
	} else if n > maxRequestBytes {
		resp = failure(codeBadRequest, fmt.Sprintf("request of %d bytes exceeds %d", n, maxRequestBytes))
	} else {
		in, err := p.ReadBytes(stack[0])
		if err != nil {
			panic(fmt.Errorf("http_request: %w", err))
		}
		resp = s.handleHTTP(ctx, in)
	}
	out, err := json.Marshal(resp)
	if err != nil {
		panic(fmt.Errorf("http_request: %w", err))
	}
	off, err := p.WriteBytes(out)
	if err != nil {
		panic(fmt.Errorf("http_request: %w", err))
	}
	stack[0] = off
}

// handleHTTP runs one http_request and scrubs the injected tokens from
// what goes back to the plugin.
func (s *Source) handleHTTP(ctx context.Context, in []byte) wireHTTPResponse {
	resp := s.doHTTP(ctx, in)
	if cs, ok := ctx.Value(callKey{}).(*callState); ok {
		cs.scrub(&resp)
	}
	return resp
}

func failure(code, msg string) wireHTTPResponse {
	return wireHTTPResponse{Error: &wireHTTPError{Code: code, Message: msg}}
}

func (s *Source) doHTTP(ctx context.Context, in []byte) wireHTTPResponse {
	cs, ok := ctx.Value(callKey{}).(*callState)
	if !ok {
		return failure(codeTransport, "no call state") // cannot happen: every call sets it
	}
	if !cs.allowNet {
		err := fmt.Errorf("%w: http_request during validate", ErrPolicy)
		cs.violate(err)
		return failure(codeNotAllowedHere, err.Error())
	}
	if !cs.takeRequest(s.opts.MaxRequestsPerCall) {
		err := fmt.Errorf("%w: more than %d http_request calls", ErrPolicy, s.opts.MaxRequestsPerCall)
		cs.violate(err)
		return failure(codeTooManyRequests, err.Error())
	}
	var r wireHTTPRequest
	if err := json.Unmarshal(in, &r); err != nil {
		return failure(codeBadRequest, err.Error())
	}
	req, err := s.newRequest(ctx, r)
	if err != nil {
		return failure(codeBadRequest, err.Error())
	}

	resp, err := s.client.Do(req) //nolint:gosec // G704: every hop goes through allowlistTransport and the injected netguard client.
	if err != nil {
		var hna *HostNotAllowedError
		switch {
		case errors.As(err, &hna), errors.Is(err, ErrHostNotAllowed):
			return failure(codeHostNotAllowed, err.Error())
		case errors.Is(err, ErrPolicy): // the redirect hops exhausted the request budget
			return failure(codeTooManyRequests, err.Error())
		case errors.Is(err, netguard.ErrBlocked):
			cs.violate(err)
			s.log.WarnContext(ctx, "wasm plugin request blocked by the SSRF guard", "error", err)
			return failure(codeBlocked, err.Error())
		case ctx.Err() != nil:
			return failure(codeCanceled, err.Error())
		}
		return failure(codeTransport, err.Error())
	}
	defer func() { _ = resp.Body.Close() }()

	out := wireHTTPResponse{
		Status:  resp.StatusCode,
		Headers: make(map[string][]string, len(resp.Header)),
	}
	for k, v := range resp.Header {
		out.Headers[strings.ToLower(k)] = v
	}
	got := wireValidators{ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified")}
	if resp.StatusCode == http.StatusNotModified {
		out.NotModified = true
		if r.Validators != nil {
			out.Validators = *r.Validators
		}
		if got.ETag != "" {
			out.Validators.ETag = got.ETag
		}
		if got.LastModified != "" {
			out.Validators.LastModified = got.LastModified
		}
		return out
	}
	out.Validators = got
	body, err := io.ReadAll(io.LimitReader(resp.Body, s.opts.MaxResponseBytes+1))
	switch {
	case errors.Is(err, netguard.ErrBodyTooLarge), err == nil && int64(len(body)) > s.opts.MaxResponseBytes:
		return failure(codeTooLarge, fmt.Sprintf("response body exceeds %d bytes", s.opts.MaxResponseBytes))
	case err != nil && ctx.Err() != nil:
		return failure(codeCanceled, err.Error())
	case err != nil:
		return failure(codeTransport, err.Error())
	}
	out.Body = body
	return out
}

func (s *Source) newRequest(ctx context.Context, r wireHTTPRequest) (*http.Request, error) {
	method := strings.ToUpper(r.Method)
	switch method {
	case "":
		method = http.MethodGet
	case http.MethodGet, http.MethodHead, http.MethodPost:
	default:
		return nil, fmt.Errorf("method %q not allowed (GET, HEAD, POST)", r.Method)
	}
	var body io.Reader
	if len(r.Body) > 0 {
		body = bytes.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.URL, body)
	if err != nil {
		return nil, err
	}
	if req.URL.User != nil {
		return nil, errors.New("credentials in the URL are not allowed; use the Authorization header")
	}
	for k, v := range r.Headers {
		if !validHeader(k, v) {
			return nil, fmt.Errorf("invalid header %q", k)
		}
		ck := http.CanonicalHeaderKey(k)
		if reservedHeaders[ck] {
			return nil, fmt.Errorf("header %q is set by the host", k)
		}
		if _, dup := req.Header[ck]; dup {
			return nil, fmt.Errorf("header %q given twice", ck)
		}
		req.Header.Set(ck, v)
	}
	if v := r.Validators; v != nil {
		if !validHeader("If-None-Match", v.ETag) || !validHeader("If-Modified-Since", v.LastModified) {
			return nil, errors.New("invalid validators")
		}
		if v.ETag != "" && req.Header.Get("If-None-Match") == "" {
			req.Header.Set("If-None-Match", v.ETag)
		}
		if v.LastModified != "" && req.Header.Get("If-Modified-Since") == "" {
			req.Header.Set("If-Modified-Since", v.LastModified)
		}
	}
	return req, nil
}

// reservedHeaders are hop-by-hop or transport headers the plugin may not
// set: the transport owns them, and some would let a plugin smuggle
// requests or reach a proxy. User-Agent stays overridable (see ADR 0011).
var reservedHeaders = map[string]bool{
	"Connection":          true,
	"Content-Length":      true,
	"Expect":              true,
	"Host":                true,
	"Keep-Alive":          true,
	"Proxy-Authorization": true,
	"Proxy-Connection":    true,
	"Te":                  true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// validHeader reports whether name is an RFC 9110 token and value holds no
// control characters other than tab. The base transport checks this too,
// but a custom transport might not.
func validHeader(name, value string) bool {
	if name == "" {
		return false
	}
	for _, c := range []byte(name) {
		if c <= ' ' || c >= 0x7f || strings.IndexByte(`"(),/:;<=>?@[\]{}`, c) >= 0 {
			return false
		}
	}
	for _, c := range []byte(value) {
		if (c < ' ' && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}

// hostLog implements log(entry): one JSON block {level, message}.
// Argument blocks belong to the guest, which frees them after the call.
func (s *Source) hostLog(ctx context.Context, p *extism.CurrentPlugin, stack []uint64) {
	n, err := p.Length(stack[0])
	if err != nil {
		panic(fmt.Errorf("log: %w", err))
	}
	cs, ok := ctx.Value(callKey{}).(*callState)
	if !ok || !s.chargeLog(ctx, cs, int64(n)) { //nolint:gosec // G115: a block length fits in int64
		return
	}
	if n > maxLogEntry {
		s.log.WarnContext(ctx, "wasm plugin log entry dropped: too large", "bytes", n)
		return
	}
	in, err := p.ReadBytes(stack[0])
	if err != nil {
		panic(fmt.Errorf("log: %w", err))
	}
	var e wireLogEntry
	if err := json.Unmarshal(in, &e); err != nil {
		s.log.WarnContext(ctx, "wasm plugin log entry dropped: not JSON", "error", err)
		return
	}
	s.log.Log(ctx, slogLevel(e.Level), truncate(e.Message, maxLogMessage), "source", "plugin")
}

func slogLevel(l string) slog.Level {
	switch strings.ToLower(l) {
	case "trace", "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// truncate cuts s to at most n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}
