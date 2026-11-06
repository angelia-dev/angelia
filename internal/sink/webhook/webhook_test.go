package webhook

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/sink"
)

type fakeResolver map[string][]netip.Addr

func (r fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := r[host]; ok {
		return a, nil
	}
	return nil, errors.New("no such host")
}

var loopback = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}

// guarded returns a netguard client with no proxy and the given allowlist.
func guarded(t *testing.T, allow []netip.Prefix, r netguard.Resolver) *http.Client {
	t.Helper()
	if r == nil {
		r = fakeResolver{}
	}
	c, err := netguard.New(netguard.Config{
		Allow: allow, Resolver: r, Proxy: func(*http.Request) (*url.URL, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return c.HTTPClient()
}

var fixedNow = time.Unix(1793880000, 0)

// testHTTPHosts lets the tests reach httptest servers over http://.
var testHTTPHosts = []string{"127.0.0.1", "localhost", "169.254.169.254"}

func newSink(t *testing.T, allow []netip.Prefix) *Sink {
	t.Helper()
	return New(guarded(t, allow, nil), Options{Now: func() time.Time { return fixedNow }, AllowHTTPHosts: testHTTPHosts})
}

const (
	eventJSON = `{"specversion":"1.0","id":"01JD0000000000000000000000","type":"dev.angelia.release.published.v1"}`
	key       = "01JD0000000000000000000000:01JD0000000000000000000001"
)

func request(url, secretEnv string, values map[string][]byte) sink.Request {
	return requestWith(url, "secret_env", secretEnv, values)
}

// requestRef is request for a sink whose secret_ref names a stored secret.
func requestRef(url, secretRef string, values map[string][]byte) sink.Request {
	return requestWith(url, "secret_ref", secretRef, values)
}

func requestWith(url, refKey, ref string, values map[string][]byte) sink.Request {
	cfg := fmt.Sprintf(`{"url":%q,"timeout":"10s"`, url)
	if ref != "" {
		cfg += fmt.Sprintf(`,%q:%q`, refKey, ref)
	}
	var vs map[string]secrets.Value
	for k, v := range values {
		if vs == nil {
			vs = map[string]secrets.Value{}
		}
		vs[k] = secrets.NewValue(v)
	}
	return sink.Request{
		Config:         sink.Config(cfg + "}"),
		Rendered:       sink.Rendered{Body: []byte(eventJSON), ContentType: sink.CloudEventsContentType},
		IdempotencyKey: key, Secrets: vs, DeliveryID: "d1", SubscriptionID: "s1", Attempt: 1,
	}
}

type captured struct {
	mu   sync.Mutex
	reqs []*http.Request
	body [][]byte
}

func (c *captured) last() (*http.Request, []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reqs) == 0 {
		return nil, nil
	}
	return c.reqs[len(c.reqs)-1], c.body[len(c.body)-1]
}

func (c *captured) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reqs)
}

func receiver(t *testing.T, h http.HandlerFunc) (*httptest.Server, *captured) {
	t.Helper()
	c := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.reqs = append(c.reqs, r)
		c.body = append(c.body, b)
		c.mu.Unlock()
		if h != nil {
			h(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

// The wire format of ADR 0023 section 1, signed with one secret.
func TestDeliverWireFormat(t *testing.T) {
	srv, got := receiver(t, nil)
	secret := genSecret(t, 32)
	s := newSink(t, loopback)
	res, err := s.Deliver(t.Context(), request(srv.URL+"/hook?x=1", "WH_CI", map[string][]byte{SecretPointer: []byte(secret + "\n")}))
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("Deliver: %+v, %v", res, err)
	}
	r, body := got.last()
	if r.Method != http.MethodPost || r.URL.RequestURI() != "/hook?x=1" {
		t.Errorf("request %s %s", r.Method, r.URL.RequestURI())
	}
	if string(body) != eventJSON {
		t.Errorf("body %q, want the event JSON byte for byte", body)
	}
	if ct := r.Header.Get("Content-Type"); ct != sink.CloudEventsContentType {
		t.Errorf("Content-Type %q", ct)
	}
	if r.Header.Get(HeaderID) != key || r.Header.Get(HeaderTimestamp) != "1793880000" {
		t.Errorf("webhook-id %q, webhook-timestamp %q", r.Header.Get(HeaderID), r.Header.Get(HeaderTimestamp))
	}
	if !strings.HasPrefix(r.Header.Get("User-Agent"), "angelia/") {
		t.Errorf("User-Agent %q, want netguard's", r.Header.Get("User-Agent"))
	}
	for k := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "ce-") {
			t.Errorf("binary-mode header %s sent", k)
		}
	}
	if err := Verify([]string{secret}, r.Header, body, fixedNow, DefaultTolerance); err != nil {
		t.Errorf("receiver cannot verify: %v", err)
	}
	if n := len(strings.Fields(r.Header.Get(HeaderSignature))); n != 1 {
		t.Errorf("%d signatures, want 1", n)
	}
}

// ADR 0024 §6: a secret_ref value signs exactly as a secret_env value, read
// from its own pointer; a value under the other pointer is not used.
func TestDeliverSecretRef(t *testing.T) {
	srv, got := receiver(t, nil)
	secret := genSecret(t, 32)
	s := newSink(t, loopback)
	if _, err := s.Deliver(t.Context(), requestRef(srv.URL, "ci-webhook", map[string][]byte{SecretRefPointer: []byte(secret)})); err != nil {
		t.Fatal(err)
	}
	r, body := got.last()
	if err := Verify([]string{secret}, r.Header, body, fixedNow, DefaultTolerance); err != nil {
		t.Errorf("receiver cannot verify a secret_ref signature: %v", err)
	}
	want, _ := Sign([]string{secret}, key, "1793880000", body)
	if r.Header.Get(HeaderSignature) != want {
		t.Errorf("webhook-signature %q, want the secret_env signature %q", r.Header.Get(HeaderSignature), want)
	}

	bad := "whsec_" + strings.Repeat("A", 8) + "-not-a-secret-value"
	for name, values := range map[string]map[string][]byte{
		"unresolved":    nil,
		"other pointer": {SecretPointer: []byte(secret)},
		"invalid":       {SecretRefPointer: []byte(bad)},
	} {
		_, err := s.Deliver(t.Context(), requestRef(srv.URL, "ci-webhook", values))
		if err == nil || sink.IsPermanent(err) || !strings.HasPrefix(err.Error(), "webhook: secret_ref ci-webhook: ") ||
			strings.Contains(err.Error(), "not-a-secret-value") {
			t.Errorf("%s: %v, want a retried error naming secret_ref ci-webhook only", name, err)
		}
	}
	if got.count() != 1 {
		t.Fatalf("receiver reached %d times, want 1", got.count())
	}
}

// AC 2 (#108): rotation. A value with three secrets gives three entries,
// in order; a receiver holding any one of them verifies.
func TestDeliverRotation(t *testing.T) {
	srv, got := receiver(t, nil)
	keys := []string{genSecret(t, 32), genSecret(t, 24), genSecret(t, 64)}
	s := newSink(t, loopback)
	if _, err := s.Deliver(t.Context(), request(srv.URL, "WH", map[string][]byte{SecretPointer: []byte(strings.Join(keys, " "))})); err != nil {
		t.Fatal(err)
	}
	r, body := got.last()
	entries := strings.Split(r.Header.Get(HeaderSignature), " ")
	if len(entries) != 3 {
		t.Fatalf("webhook-signature %q: want 3 space-separated entries", r.Header.Get(HeaderSignature))
	}
	for i, sec := range keys {
		if err := Verify([]string{sec}, r.Header, body, fixedNow, DefaultTolerance); err != nil {
			t.Errorf("secret %d: %v", i, err)
		}
		want, _ := Sign([]string{sec}, key, "1793880000", body)
		if entries[i] != want {
			t.Errorf("entry %d is not secret %d's", i, i)
		}
	}
	if len(r.Header.Get(HeaderSignature)) >= 200 {
		t.Errorf("header is %d bytes, want < 200", len(r.Header.Get(HeaderSignature)))
	}
}

func TestDeliverUnsignedAndTemplated(t *testing.T) {
	srv, got := receiver(t, nil)
	s := newSink(t, loopback)
	req := request(srv.URL, "", nil)
	req.Rendered = sink.Rendered{Body: []byte("text: 1.0"), ContentType: "text/plain; charset=utf-8", Templated: true}
	if _, err := s.Deliver(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	r, body := got.last()
	if _, ok := r.Header[http.CanonicalHeaderKey(HeaderSignature)]; ok {
		t.Error("unsigned sink sent webhook-signature")
	}
	if r.Header.Get(HeaderID) != key || string(body) != "text: 1.0" || r.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Errorf("templated: id %q body %q type %q", r.Header.Get(HeaderID), body, r.Header.Get("Content-Type"))
	}
	// An empty content type falls back to the CloudEvents media type.
	req.Rendered.ContentType = ""
	if _, err := s.Deliver(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if r, _ := got.last(); r.Header.Get("Content-Type") != sink.CloudEventsContentType {
		t.Errorf("default content type %q", r.Header.Get("Content-Type"))
	}
}

// ADR 0023 section 6: response handling is sink.HTTPStatus, and last_error
// is "webhook: <scheme://host>: HTTP <code> <text>".
func TestDeliverResponses(t *testing.T) {
	cases := []struct {
		code       int
		retryAfter string
		permanent  bool
		wait       time.Duration
	}{
		{http.StatusInternalServerError, "", false, 0},
		{http.StatusServiceUnavailable, "", false, 0},
		{http.StatusTooManyRequests, "120", false, 2 * time.Minute},
		{http.StatusRequestTimeout, "", false, 0},
		{http.StatusNotFound, "", true, 0},
		{http.StatusUnauthorized, "", true, 0},
		{http.StatusGone, "", true, 0},
		{http.StatusFound, "", true, 0},
		{http.StatusPermanentRedirect, "", true, 0},
	}
	for _, c := range cases {
		t.Run(fmt.Sprint(c.code), func(t *testing.T) {
			targetSrv, target := receiver(t, nil)
			srv, got := receiver(t, func(w http.ResponseWriter, _ *http.Request) {
				if c.retryAfter != "" {
					w.Header().Set("Retry-After", c.retryAfter)
				}
				if c.code >= 300 && c.code < 400 {
					w.Header().Set("Location", targetSrv.URL)
				}
				w.WriteHeader(c.code)
				_, _ = io.WriteString(w, "receiver body must not be stored")
			})
			// Each case gets its own client: a 429 pauses the host in
			// netguard's shared limiter.
			res, err := newSink(t, loopback).Deliver(t.Context(), request(srv.URL+"/secret-path", "", nil))
			if err == nil {
				t.Fatal("want an error")
			}
			if res.StatusCode != c.code || got.count() != 1 {
				t.Errorf("status %d, sent %d times", res.StatusCode, got.count())
			}
			if sink.IsPermanent(err) != c.permanent {
				t.Errorf("permanent = %v, want %v (%v)", sink.IsPermanent(err), c.permanent, err)
			}
			if w := sink.RetryAfter(err); w != c.wait {
				t.Errorf("RetryAfter = %v, want %v", w, c.wait)
			}
			want := fmt.Sprintf("webhook: %s: HTTP %d %s", srv.URL, c.code, http.StatusText(c.code))
			if msg := err.Error(); !strings.HasPrefix(msg, want) || strings.Contains(msg, "secret-path") || strings.Contains(msg, "receiver body") {
				t.Errorf("error %q, want prefix %q and no path or body", msg, want)
			}
			if target.count() != 0 {
				t.Error("redirect followed")
			}
		})
	}
}

// A large response body is read up to 64 KiB and discarded.
func TestDeliverLargeResponse(t *testing.T) {
	srv, _ := receiver(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 1<<20))
	})
	if _, err := newSink(t, loopback).Deliver(t.Context(), request(srv.URL, "", nil)); err != nil {
		t.Fatal(err)
	}
}

// Connection failures and timeouts are retried.
func TestDeliverTransportErrors(t *testing.T) {
	reset, _ := receiver(t, func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	})
	stop := make(chan struct{})
	slow, _ := receiver(t, func(http.ResponseWriter, *http.Request) { <-stop })
	t.Cleanup(func() { close(stop) })
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := "http://" + l.Addr().String()
	_ = l.Close()

	s := newSink(t, loopback)
	// The errors are fixed strings plus scheme and host: no path, query,
	// dialled address or cause text (review SR3, NIT5).
	for _, c := range []struct{ name, url, class string }{
		{"reset", reset.URL, "connection reset"},
		{"refused", refused, "connection refused"},
	} {
		_, err := s.Deliver(t.Context(), request(c.url+"/tok-SECRETPATH?k=v", "", nil))
		want := "webhook: " + c.url + ": " + c.class
		if err == nil || sink.IsPermanent(err) || err.Error() != want {
			t.Errorf("%s: %v, want retried %q", c.name, err, want)
			continue
		}
		if msg := err.Error(); strings.Contains(msg, "SECRETPATH") || strings.Contains(msg, "k=v") || strings.Contains(msg, "dial") {
			t.Errorf("%s: error holds the path, query or cause: %q", c.name, msg)
		}
		if sink.RedactURL(err) != want {
			t.Errorf("%s: RedactURL changes it to %q", c.name, sink.RedactURL(err))
		}
	}
	// The dispatcher's deadline bounds the request when it is shorter than
	// the configured timeout.
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = s.Deliver(ctx, request(slow.URL, "", nil))
	if err == nil || sink.IsPermanent(err) || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Errorf("timeout: %v after %v", err, time.Since(start))
	}
	// The configured timeout bounds it when it is shorter.
	req := request(slow.URL, "", nil)
	req.Config = sink.Config(fmt.Sprintf(`{"url":%q,"timeout":"1s"}`, slow.URL))
	start = time.Now()
	_, err = s.Deliver(t.Context(), req)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Errorf("config timeout: %v after %v", err, time.Since(start))
	}
}

// AC 4 (#108): private-network targets are refused unless allowlisted. The
// refusal is permanent and names the URL host, never the resolved address.
func TestDeliverSSRF(t *testing.T) {
	srv, got := receiver(t, nil)
	_, p, _ := net.SplitHostPort(srv.Listener.Addr().String())

	// Not allowlisted: a literal loopback URL, and a hostname that
	// resolves to a private address, are refused before any request.
	s := New(guarded(t, nil, fakeResolver{"hooks.internal.test": {netip.MustParseAddr("10.20.30.40")}}), Options{AllowHTTPHosts: testHTTPHosts})
	for _, c := range []struct{ url, host string }{
		{srv.URL, "127.0.0.1"},
		{"https://hooks.internal.test/x", "hooks.internal.test"},
		{"http://169.254.169.254/latest", "169.254.169.254"},
	} {
		_, err := s.Deliver(t.Context(), request(c.url, "", nil))
		want := "webhook: destination not allowed (" + c.host + ")"
		if !sink.IsPermanent(err) || err.Error() != want || !errors.Is(err, netguard.ErrBlocked) {
			t.Errorf("%s: %v, want permanent %q", c.url, err, want)
		}
		if strings.Contains(err.Error(), "10.20.30.40") {
			t.Errorf("error leaks the resolved address: %v", err)
		}
	}
	// DNS rebinding: public for the URL check, loopback at dial time.
	s = New(guarded(t, nil, fakeResolver{"localhost": {netip.MustParseAddr("203.0.113.7")}}), Options{AllowHTTPHosts: testHTTPHosts})
	_, err := s.Deliver(t.Context(), request("http://localhost:"+p+"/", "", nil))
	if !sink.IsPermanent(err) || err.Error() != "webhook: destination not allowed (localhost)" {
		t.Errorf("rebinding: %v", err)
	}
	if got.count() != 0 {
		t.Fatalf("receiver reached %d times", got.count())
	}
	// Allowlisted: delivered.
	if _, err := newSink(t, []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}).Deliver(t.Context(), request(srv.URL, "", nil)); err != nil {
		t.Fatalf("allowlisted: %v", err)
	}
	if got.count() != 1 {
		t.Fatalf("receiver reached %d times", got.count())
	}
}

// ADR 0023 sections 2 and 4: a secret that is not resolved or not valid is
// a retried error that names the reference, never the value.
func TestDeliverSecretErrors(t *testing.T) {
	srv, got := receiver(t, nil)
	s := newSink(t, loopback)
	bad := "whsec_" + strings.Repeat("A", 8) + "-not-a-secret-value"
	for name, values := range map[string]map[string][]byte{
		"unresolved": nil,
		"invalid":    {SecretPointer: []byte(bad)},
		"empty":      {SecretPointer: {}},
	} {
		_, err := s.Deliver(t.Context(), request(srv.URL, "WH_CI", values))
		if err == nil || sink.IsPermanent(err) || !strings.HasPrefix(err.Error(), "webhook: secret_env WH_CI: ") {
			t.Errorf("%s: %v, want a retried error naming WH_CI", name, err)
			continue
		}
		if strings.Contains(err.Error(), "not-a-secret-value") {
			t.Errorf("%s: error quotes the value: %v", name, err)
		}
	}
	if want := "webhook: secret_env WH_CI: invalid secret (want 1-3 whsec_ secrets of 24-64 bytes, padded standard base64)"; func() string {
		_, err := s.Deliver(t.Context(), request(srv.URL, "WH_CI", map[string][]byte{SecretPointer: []byte(bad)}))
		return err.Error()
	}() != want {
		t.Errorf("message differs from %q", want)
	}
	if got.count() != 0 {
		t.Fatalf("sent %d requests without a valid secret", got.count())
	}
}

// Review SR3: reached by host name, a failed connection names the host,
// never the address it resolved to.
func TestDeliverErrorHidesResolvedAddress(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	_ = l.Close()
	as, err := net.DefaultResolver.LookupNetIP(t.Context(), "ip", "localhost")
	if err != nil || len(as) == 0 {
		t.Skipf("localhost does not resolve here: %v", err)
	}
	for _, a := range as {
		if !a.Unmap().IsLoopback() {
			t.Skipf("localhost resolves to %s, not loopback", a)
		}
	}
	// CI runners resolve localhost to ::1 as well as 127.0.0.1, and the
	// dialler may try ::1 first: allow both loopbacks so either attempt ends
	// in "connection refused", not in a refusal by the guard.
	bothLoopbacks := append([]netip.Prefix{netip.MustParsePrefix("::1/128")}, loopback...)
	s := New(guarded(t, bothLoopbacks, fakeResolver{"localhost": {netip.MustParseAddr("127.0.0.1")}}), Options{AllowHTTPHosts: testHTTPHosts})
	_, err = s.Deliver(t.Context(), request("http://localhost:"+port+"/x", "", nil))
	if err == nil || sink.IsPermanent(err) || err.Error() != "webhook: http://localhost:"+port+": connection refused" {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "::1") {
		t.Fatalf("error leaks the dialled address: %v", err)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("the cause is not kept in the chain: %v", err)
	}
}

func TestFailureClass(t *testing.T) {
	for want, err := range map[string]error{
		"timeout":              context.DeadlineExceeded,
		"canceled":             context.Canceled,
		"DNS lookup failed":    &net.DNSError{Err: "no such host", Name: "h"},
		"connection reset":     io.ErrUnexpectedEOF,
		"TLS handshake failed": &tls.CertificateVerificationError{Err: errors.New("x")},
		"request failed":       errors.New("something else at 10.1.2.3:443"),
	} {
		if got := failureClass(err); got != want {
			t.Errorf("failureClass(%v) = %q, want %q", err, got, want)
		}
	}
	if got := failureClass(fmt.Errorf("netguard: resolve h: %w", errors.New("no such host"))); got != "DNS lookup failed" {
		t.Errorf("netguard resolve error: %q", got)
	}
}

// Review SR1: a non-canonical numeric host is refused when configured, and
// a netguard URL error no retry can fix is permanent; a DNS failure is not.
func TestDeliverInvalidURLIsPermanent(t *testing.T) {
	s := newSink(t, loopback)
	for _, u := range []string{"http://2130706433/", "http://0x7f000001/", "http://127.1/"} {
		if err := s.Validate(sink.Config(fmt.Sprintf(`{"url":%q}`, u))); err == nil {
			t.Errorf("Validate(%s): want an error", u)
		}
		if _, err := s.Deliver(t.Context(), request(u, "", nil)); !sink.IsPermanent(err) {
			t.Errorf("Deliver(%s): %v, want permanent", u, err)
		}
	}
	if err := requestError("https://h", "h", &url.Error{Op: "Post", URL: "https://h/", Err: fmt.Errorf("x: %w", netguard.ErrInvalidURL)}); !sink.IsPermanent(err) ||
		err.Error() != "webhook: https://h: invalid URL (scheme or host not allowed)" {
		t.Errorf("ErrInvalidURL: %v", err)
	}
	_, err := s.Deliver(t.Context(), request("https://no-such-host.test/", "", nil))
	if err == nil || sink.IsPermanent(err) || err.Error() != "webhook: https://no-such-host.test: DNS lookup failed" {
		t.Errorf("DNS failure: %v, want retried", err)
	}
}

// Review NIT3: http:// is checked against AllowHTTPHosts at every delivery,
// so removing a host stops cleartext to sinks stored earlier.
func TestDeliverHTTPAllowlist(t *testing.T) {
	srv, got := receiver(t, nil)
	s := New(guarded(t, loopback, nil), Options{})
	_, err := s.Deliver(t.Context(), request(srv.URL+"/p", "", nil))
	if !sink.IsPermanent(err) || !strings.Contains(err.Error(), "http:// is not allowed for this host") || strings.Contains(err.Error(), "/p") {
		t.Fatalf("got %v", err)
	}
	if got.count() != 0 {
		t.Fatal("cleartext request sent")
	}
	s = New(guarded(t, loopback, nil), Options{AllowHTTPHosts: []string{"127.0.0.1"}})
	if _, err := s.Deliver(t.Context(), request(srv.URL, "", nil)); err != nil || got.count() != 1 {
		t.Fatalf("allowlisted: %v, %d requests", err, got.count())
	}
}

// Review SR4: an unsigned sink logs one warning per sink id, with no URL
// path; a signed sink logs nothing.
func TestDeliverWarnsOncePerUnsignedSink(t *testing.T) {
	srv, _ := receiver(t, nil)
	var buf bytes.Buffer
	s := New(guarded(t, loopback, nil), Options{AllowHTTPHosts: testHTTPHosts, Logger: slog.New(slog.NewJSONHandler(&buf, nil))})
	for _, id := range []string{"k1", "k1", "k2", "k1"} {
		req := request(srv.URL+"/secret-path", "", nil)
		req.SinkID = id
		if _, err := s.Deliver(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}
	signed := request(srv.URL, "WH", map[string][]byte{SecretPointer: []byte(genSecret(t, 32))})
	signed.SinkID = "k3"
	if _, err := s.Deliver(t.Context(), signed); err != nil {
		t.Fatal(err)
	}
	logs := buf.String()
	if n := strings.Count(logs, "deliveries are unsigned"); n != 2 || !strings.Contains(logs, `"sink":"k1"`) || !strings.Contains(logs, `"sink":"k2"`) || strings.Contains(logs, "k3") {
		t.Fatalf("%d warnings:\n%s", n, logs)
	}
	if strings.Contains(logs, "secret-path") {
		t.Fatalf("warning holds the URL path:\n%s", logs)
	}
}

func TestDeliverRejectsBadRequests(t *testing.T) {
	srv, got := receiver(t, nil)
	s := newSink(t, loopback)
	for _, k := range []string{"", "a.b:c", "a b", "a\tb"} {
		req := request(srv.URL, "", nil)
		req.IdempotencyKey = k
		if _, err := s.Deliver(t.Context(), req); !sink.IsPermanent(err) {
			t.Errorf("id %q: %v, want permanent", k, err)
		}
	}
	req := request(srv.URL, "", nil)
	req.Config = sink.Config(`{"url":"ftp://x"}`)
	if _, err := s.Deliver(t.Context(), req); !sink.IsPermanent(err) {
		t.Errorf("bad config: %v, want permanent", err)
	}
	if got.count() != 0 {
		t.Fatalf("sent %d requests", got.count())
	}
}

func TestValidate(t *testing.T) {
	s := newSink(t, nil)
	for _, cfg := range []string{
		`{"url":"https://hooks.example.com/x"}`,
		`{"url":"http://hooks.lan:8080/x","secret_env":"WH_CI","timeout":"2m"}`,
		`{"url":"https://h.example/","timeout":"1s"}`,
		`{"url":"https://h.example/","secret_ref":"ci-webhook.v2"}`,
	} {
		if err := s.Validate(sink.Config(cfg)); err != nil {
			t.Errorf("%s: %v", cfg, err)
		}
	}
	for _, cfg := range []string{
		`not json`, `{}`, `{"url":"/relative"}`, `{"url":"ftp://h/"}`, `{"url":"https://u:p@h/"}`,
		`{"url":"https://h/","secret_env":"whsec_abc"}`, `{"url":"https://h/","secret_env":"1BAD"}`,
		`{"url":"https://h/","timeout":"500ms"}`, `{"url":"https://h/","timeout":"3m"}`, `{"url":"https://h/","timeout":"soon"}`,
		`{"url":"https://h/","secret":"whsec_inline"}`, `{"url":"https://h/","headers":{}}`,
		`{"url":"https://h/","secret_ref":"whsec_Inline"}`,
		`{"url":"https://h/","secret_env":"WH","secret_ref":"wh"}`,
	} {
		err := s.Validate(sink.Config(cfg))
		if err == nil {
			t.Errorf("%s: want an error", cfg)
			continue
		}
		if strings.Contains(err.Error(), "whsec_") {
			t.Errorf("%s: error echoes a value: %v", cfg, err)
		}
	}
}

func TestRegistry(t *testing.T) {
	r := sink.NewRegistry()
	if err := r.Register(newSink(t, nil)); err != nil {
		t.Fatal(err)
	}
	if s, err := r.Get("webhook"); err != nil || s.Kind() != Kind {
		t.Fatalf("Get(webhook) = %v, %v", s, err)
	}
	defer func() {
		if recover() == nil {
			t.Error("New(nil) did not panic")
		}
	}()
	New(nil, Options{})
}

// The sink uses a copy of the client: the caller's client still follows
// redirects.
func TestNewDoesNotChangeClient(t *testing.T) {
	hc := guarded(t, nil, nil)
	New(hc, Options{})
	if hc.CheckRedirect == nil {
		t.Fatal("caller's CheckRedirect replaced")
	}
	if err := hc.CheckRedirect(nil, nil); errors.Is(err, http.ErrUseLastResponse) {
		t.Fatal("caller's client stops following redirects")
	}
}
