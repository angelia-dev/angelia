// Package generic is the shared part of the sources whose upstream URL is a
// watch coordinate (http-json, http-regex; ADR 0038): the URL rules of
// decision D52, the guarded fetch with its redirect rule and its 2 MiB body
// cap, the sanitising of raw version strings (D53) and the content-hash
// cursor (D56).
//
// Every request still goes through the injected *http.Client, normally the
// netguard client (SSRF guard per hop and at dial time, per-host limiter,
// retries). This package only narrows what that client accepts: it never
// builds its own transport.
package generic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
)

// Limits of decisions D52, D53 and D54.
const (
	// MaxURLBytes is the longest url coordinate.
	MaxURLBytes = 2048
	// MaxRedirects is the most redirects one fetch follows.
	MaxRedirects = 5
	// MaxBodyBytes is the largest response body a fetch reads; it is below
	// netguard's own cap (netguard.DefaultMaxBodyBytes).
	MaxBodyBytes = 2 << 20
	// MaxVersions is the most distinct versions one fetch may yield.
	MaxVersions = 1000
	// MaxVersionBytes is the longest raw version kept.
	MaxVersionBytes = 256
	// MaxNameBytes is the longest name coordinate.
	MaxNameBytes = 128
	// MaxMatches is the most raw values one fetch may extract before
	// de-duplication; above it the list would be cut, so it is an error.
	MaxMatches = 10 * MaxVersions
)

// ErrTooManyMatches is returned by an extractor above MaxMatches.
var ErrTooManyMatches = fmt.Errorf("the expression matched more than %d values; narrow it", MaxMatches)

// ValidateName checks the name coordinate (D54): 1 to 128 bytes of valid
// UTF-8, printable, no white space. It becomes data.package of the events.
func ValidateName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: needs a name coordinate (the package name in events)", source.ErrInvalidTarget)
	case len(name) > MaxNameBytes:
		return fmt.Errorf("%w: name longer than %d bytes", source.ErrInvalidTarget, MaxNameBytes)
	case !utf8.ValidString(name):
		return fmt.Errorf("%w: name is not valid UTF-8", source.ErrInvalidTarget)
	}
	for _, r := range name {
		if !unicode.IsPrint(r) || unicode.IsSpace(r) {
			return fmt.Errorf("%w: name %q has white space or a non-printable character", source.ErrInvalidTarget, name)
		}
	}
	return nil
}

// OnlyKeys fails when t has a coordinate outside allowed.
func OnlyKeys(t source.Target, allowed ...string) error {
	for k := range t.Coordinates {
		if !slices.Contains(allowed, k) {
			return fmt.Errorf("%w: unknown coordinate %q (allowed: %s)", source.ErrInvalidTarget, k, strings.Join(allowed, ", "))
		}
	}
	return nil
}

// httpAllowed reports whether host may be reached over plain http: it is in
// allowHTTP (http.allow_http_hosts), compared case-insensitively.
func httpAllowed(allowHTTP []string, host string) bool {
	return slices.ContainsFunc(allowHTTP, func(h string) bool { return strings.EqualFold(h, host) })
}

// ValidateURL checks the url coordinate (D52) and returns it parsed. It must
// be absolute, at most MaxURLBytes, with a host, no user information, no
// fragment, no white space or control character and a host that is not an
// IP address in a non-canonical form (2130706433, 127.1); the scheme is
// https, or http only for a host in allowHTTP. Errors wrap
// source.ErrInvalidTarget. No I/O: the SSRF guard checks the addresses when
// the request is sent.
func ValidateURL(raw string, allowHTTP []string) (*url.URL, error) {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: url: "+format, append([]any{source.ErrInvalidTarget}, a...)...)
	}
	if raw == "" {
		return nil, bad("needs a url coordinate")
	}
	if len(raw) > MaxURLBytes {
		return nil, bad("longer than %d bytes", MaxURLBytes)
	}
	if strings.IndexFunc(raw, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 || !utf8.ValidString(raw) {
		return nil, bad("has white space, a control character or invalid UTF-8; percent-encode it")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, bad("not a URL")
	}
	switch {
	case u.Opaque != "" || u.Host == "" || u.Hostname() == "":
		return nil, bad("must be an absolute https:// URL with a host")
	case u.User != nil:
		return nil, bad("user information (user:password@) is not allowed; these sources send no credential")
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return nil, bad("must not have a fragment")
	case netguard.NonCanonicalIP(u.Hostname()):
		return nil, bad("the host is an IP address in a non-canonical form; write it as a.b.c.d")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !httpAllowed(allowHTTP, u.Hostname()) {
			return nil, bad("must be https:// (http:// is accepted only for hosts in http.allow_http_hosts)")
		}
	default:
		return nil, bad("scheme must be https")
	}
	return u, nil
}

// CanonicalURL returns the one spelling of a URL that ValidateURL accepted:
// scheme and host lower-cased, the scheme's default port removed, an empty
// path written "/". The path keeps its escaping and the query is kept byte
// for byte, since servers may treat them case- and order-sensitively. It is
// idempotent.
func CanonicalURL(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]" // an IPv6 literal without a port
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	out := scheme + "://" + host + path
	if u.RawQuery != "" || u.ForceQuery {
		out += "?" + u.RawQuery
	}
	return out
}

// Fetcher sends the GET of one fetch through an injected client. It is safe
// for concurrent use.
type Fetcher struct {
	hc        *http.Client
	allowHTTP []string
}

// NewFetcher returns a Fetcher over hc, normally the HTTPClient of the
// netguard client. allowHTTP is http.allow_http_hosts. hc is not modified:
// the Fetcher keeps a shallow copy whose CheckRedirect follows at most
// MaxRedirects hops and none to plain http outside allowHTTP, and whose
// transport stops reading a body after MaxBodyBytes. The copy's transport
// wraps hc's, so every request still goes through it.
func NewFetcher(hc *http.Client, allowHTTP []string) *Fetcher {
	if hc == nil {
		return &Fetcher{allowHTTP: slices.Clone(allowHTTP)}
	}
	f := &Fetcher{allowHTTP: slices.Clone(allowHTTP)}
	cp := *hc
	orig := cp.CheckRedirect
	cp.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > MaxRedirects {
			return fmt.Errorf("stopped after %d redirects", MaxRedirects)
		}
		if req.URL.User != nil {
			return fmt.Errorf("refusing a redirect to %s with user information", req.URL.Host)
		}
		if strings.EqualFold(req.URL.Scheme, "http") && !httpAllowed(f.allowHTTP, req.URL.Hostname()) {
			return fmt.Errorf("refusing a redirect to plain http://%s (not in http.allow_http_hosts)", req.URL.Host)
		}
		if orig != nil {
			return orig(req, via)
		}
		return nil
	}
	base := cp.Transport
	if base == nil {
		base = http.DefaultTransport // what hc itself would use
	}
	cp.Transport = capTransport{base: base, limit: MaxBodyBytes}
	f.hc = &cp
	return f
}

// Fetch GETs u with the Accept header accept and the validators of st. A
// 304 is a Result with NotModified; a status other than 2xx or 304 is the
// *netguard.StatusError; a body over MaxBodyBytes is an error wrapping
// netguard.ErrBodyTooLarge. Errors name the URL's host only.
func (f *Fetcher) Fetch(ctx context.Context, u *url.URL, accept string, st source.FetchState) (*netguard.Result, error) {
	if f.hc == nil {
		return nil, errors.New("source has no HTTP client")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", u.Host, err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	res, err := netguard.Fetch(ctx, f.hc, req, netguard.Validators{ETag: st.ETag, LastModified: st.LastModified})
	if err != nil {
		if errors.Is(err, netguard.ErrBodyTooLarge) {
			return nil, fmt.Errorf("fetch %s: the response is larger than %d bytes: %w", u.Host, MaxBodyBytes, err)
		}
		return nil, fmt.Errorf("fetch %s: %w", u.Host, err)
	}
	return res, nil
}

// capTransport stops each response body after limit bytes.
type capTransport struct {
	base  http.RoundTripper
	limit int64
}

func (c capTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := c.base.RoundTrip(r)
	if err != nil || resp.Body == nil {
		return resp, err
	}
	resp.Body = &cappedBody{rc: resp.Body, remaining: c.limit, limit: c.limit}
	return resp, nil
}

type cappedBody struct {
	rc        io.ReadCloser
	remaining int64
	limit     int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		var probe [1]byte
		n, err := b.rc.Read(probe[:])
		if n > 0 {
			return 0, fmt.Errorf("%w (%d bytes)", netguard.ErrBodyTooLarge, b.limit)
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.rc.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func (b *cappedBody) Close() error { return b.rc.Close() }

// ErrNoVersions is returned by Versions when nothing usable was extracted.
var ErrNoVersions = errors.New("the expression matched no usable version")

// ErrTooManyVersions is returned by Versions above MaxVersions.
var ErrTooManyVersions = fmt.Errorf("the expression matched more than %d distinct versions; narrow it", MaxVersions)

// Versions turns extracted strings into versions (D53), in document order:
// values that are empty, longer than MaxVersionBytes, not valid UTF-8 or
// that contain a control character are skipped (skipped counts them), and
// duplicates keep their first position. More than MaxVersions distinct
// values is ErrTooManyVersions, none is ErrNoVersions: a user-written
// expression that matches nothing must show on the watch's status rather
// than look like "no new release".
func Versions(raw []string) (out []source.Version, skipped int, err error) {
	seen := make(map[string]bool, len(raw))
	for _, s := range raw {
		if !usable(s) {
			skipped++
			continue
		}
		if seen[s] {
			continue
		}
		if len(seen) == MaxVersions {
			return nil, skipped, ErrTooManyVersions
		}
		seen[s] = true
		out = append(out, source.Version{Raw: s})
	}
	if len(out) == 0 {
		if skipped > 0 {
			return nil, skipped, fmt.Errorf("%w (%d value(s) skipped: empty, longer than %d bytes, invalid UTF-8 or with control characters)", ErrNoVersions, skipped, MaxVersionBytes)
		}
		return nil, 0, ErrNoVersions
	}
	return out, skipped, nil
}

func usable(s string) bool {
	if s == "" || len(s) > MaxVersionBytes || !utf8.ValidString(s) {
		return false
	}
	return strings.IndexFunc(s, unicode.IsControl) < 0
}

// Cursor is the content hash of a version list (D56): "sha256:" and the hex
// digest of the raw values joined by "\n" (raw values hold no control
// character, so the join is unambiguous).
func Cursor(vs []source.Version) string {
	h := sha256.New()
	for i, v := range vs {
		if i > 0 {
			_, _ = io.WriteString(h, "\n")
		}
		_, _ = io.WriteString(h, v.Raw)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// Result builds the List result of a 200 response (D56): the versions with
// the response's validators and the content hash as cursor, or NotModified
// when the hash equals the incoming cursor.
func Result(vs []source.Version, v netguard.Validators, prev source.FetchState) source.Result {
	st := source.FetchState{ETag: v.ETag, LastModified: v.LastModified, Cursor: Cursor(vs)}
	if prev.Cursor != "" && prev.Cursor == st.Cursor {
		return source.Result{State: st, NotModified: true}
	}
	return source.Result{Versions: vs, State: st}
}

// NotModified builds the List result of a 304: the validators merged by
// netguard.Fetch and the incoming cursor.
func NotModified(v netguard.Validators, prev source.FetchState) source.Result {
	return source.Result{State: source.FetchState{ETag: v.ETag, LastModified: v.LastModified, Cursor: prev.Cursor}, NotModified: true}
}
