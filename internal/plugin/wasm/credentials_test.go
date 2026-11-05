package wasm_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/plugin/wasm"
	"github.com/angelia-dev/angelia/internal/source"
)

const testToken = "s3cret-token-0123456789"

func staticToken(v string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return v, nil }
}

// seenReq is what the echo upstream received.
type seenReq struct {
	Authorization, Range, AcceptEncoding, PrivateToken, APIKey string
}

// echoUpstream records every request per host and reflects the
// Authorization header everywhere it can, like a misbehaving or debugging
// upstream: in the body, a header value, a header name, the ETag and
// Last-Modified. It serves Range requests, and gzips the body on /gzip
// when asked, on /gzip-always regardless, and on /gzip-double behind an
// identity Content-Encoding header followed by a gzip one. ?to=URL redirects.
type echoUpstream struct {
	mu   sync.Mutex
	seen map[string][]seenReq
}

func (a *echoUpstream) handler(host string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := r.Header.Get("Authorization")
		a.mu.Lock()
		if a.seen == nil {
			a.seen = map[string][]seenReq{}
		}
		a.seen[host] = append(a.seen[host], seenReq{
			v, r.Header.Get("Range"), r.Header.Get("Accept-Encoding"), r.Header.Get("Private-Token"), r.Header.Get("X-Api-Key"),
		})
		if v == "" {
			v = r.Header.Get("Private-Token")
		}
		a.mu.Unlock()
		if loc := r.URL.Query().Get("to"); loc != "" {
			http.Redirect(w, r, loc, http.StatusFound) //nolint:gosec // G710: test upstream, redirects are the point.
			return
		}
		body := fmt.Appendf(nil, "you sent %q", v)
		if v != "" {
			w.Header().Set("X-Echo", v)
			w.Header().Set("X-Tok-"+strings.TrimPrefix(v, "Bearer "), "1")
			w.Header().Set("Etag", `"`+v+`"`)
			w.Header().Set("Last-Modified", v)
		}
		if r.URL.Path == "/gzip-double" {
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			_, _ = zw.Write(body)
			_ = zw.Close()
			w.Header().Add("Content-Encoding", "identity")
			w.Header().Add("Content-Encoding", "gzip")
			_, _ = w.Write(buf.Bytes()) //nolint:gosec // G705: test upstream echoing on purpose.
			return
		}
		if r.URL.Path == "/gzip-always" || (r.URL.Path == "/gzip" && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")) {
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			_, _ = zw.Write(body)
			_ = zw.Close()
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(buf.Bytes()) //nolint:gosec // G705: test upstream echoing on purpose.
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
	})
}

func (a *echoUpstream) got(host string) []seenReq {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.seen[host])
}

func (a *echoUpstream) auth(host string) []string {
	var out []string
	for _, s := range a.got(host) {
		out = append(out, s.Authorization)
	}
	return out
}

// newCredentialed returns the hostile plugin with allowed.test from its
// manifest, other.test added by the operator and a credential bound to
// allowed.test (port 443) unless opts has others.
func newCredentialed(t *testing.T, opts wasm.Options) (*echoUpstream, *wasm.Source) {
	t.Helper()
	rec := &echoUpstream{}
	_, hc := newTestNet(t, map[string]http.Handler{
		"allowed.test": rec.handler("allowed.test"),
		"other.test":   rec.handler("other.test"),
	})
	opts.HTTPClient = hc
	opts.AllowedHosts = []string{"other.test"}
	if opts.Credentials == nil {
		opts.Credentials = []wasm.Credential{{Host: "allowed.test", Scheme: "Bearer", Token: staticToken(testToken)}}
	}
	return rec, newHostile(t, hostileManifest(t, "allowed.test"), opts)
}

func fetch(t *testing.T, s *wasm.Source, u string, kv ...string) (string, error) {
	t.Helper()
	res, err := s.List(t.Context(), hostileTarget("fetch", append([]string{"url", u}, kv...)...), source.FetchState{})
	if err != nil {
		return "", err
	}
	if len(res.Versions) != 1 {
		t.Fatalf("versions = %+v", res.Versions)
	}
	if out, _ := json.Marshal(res); strings.Contains(string(out), testToken) {
		t.Errorf("result holds the token: %s", out)
	}
	b, _ := res.Versions[0].Metadata["body"].(string)
	return b, nil
}

// AC: credentials are injected by the host into requests to the host they
// are bound to, and only there.
func TestCredentialInjectedOnBoundHostOnly(t *testing.T) {
	rec, s := newCredentialed(t, wasm.Options{})

	for _, u := range []string{"https://allowed.test/a", "https://other.test/a", "https://allowed.test:8443/a"} {
		if _, err := fetch(t, s, u); err != nil {
			t.Fatal(err)
		}
	}
	if got := rec.auth("allowed.test"); !slices.Equal(got, []string{"Bearer " + testToken, ""}) {
		t.Errorf("allowed.test saw Authorization %q, want the credential on port 443 only", got)
	}
	if got := rec.auth("other.test"); !slices.Equal(got, []string{""}) {
		t.Errorf("other.test saw Authorization %q, want none", got)
	}
}

// A credential bound to host:port is sent on that port only.
func TestCredentialExplicitPort(t *testing.T) {
	rec, s := newCredentialed(t, wasm.Options{Credentials: []wasm.Credential{{Host: "allowed.test:8443", Token: staticToken(testToken)}}})
	for _, u := range []string{"https://allowed.test:8443/a", "https://allowed.test/a", "https://allowed.test:443/a"} {
		if _, err := fetch(t, s, u); err != nil {
			t.Fatal(err)
		}
	}
	if got := rec.auth("allowed.test"); !slices.Equal(got, []string{testToken, "", ""}) {
		t.Errorf("allowed.test saw Authorization %q, want the credential on port 8443 only", got)
	}
	if got := s.Hosts()[0].CredentialPorts; !slices.Equal(got, []string{"8443"}) {
		t.Errorf("CredentialPorts = %q", got)
	}
}

// AC: the host does not hand the credential to the plugin, and the plugin
// cannot send it to another allowed host.
func TestPluginCannotReadOrForwardCredential(t *testing.T) {
	logs := &logRecorder{}
	rec, s := newCredentialed(t, wasm.Options{Logger: slog.New(logs)})

	t.Run("echo in the body is redacted", func(t *testing.T) {
		b, err := fetch(t, s, "https://allowed.test/echo")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(b, testToken) || !strings.Contains(b, "[REDACTED]") {
			t.Errorf("plugin read body %q, want the token redacted", b)
		}
	})

	t.Run("redirect to another allowed host drops it", func(t *testing.T) {
		if _, err := fetch(t, s, "https://allowed.test/r?to=https://other.test/landing"); err != nil {
			t.Fatal(err)
		}
		if got := rec.auth("other.test"); slices.ContainsFunc(got, func(v string) bool { return v != "" }) {
			t.Errorf("other.test saw Authorization %q after a redirect, want none", got)
		}
	})

	t.Run("redirect to another port of the bound host drops it", func(t *testing.T) {
		before := len(rec.got("allowed.test"))
		if _, err := fetch(t, s, "https://allowed.test/r?to=https://allowed.test:8443/landing"); err != nil {
			t.Fatal(err)
		}
		if got := rec.auth("allowed.test")[before:]; !slices.Equal(got, []string{"Bearer " + testToken, ""}) {
			t.Errorf("allowed.test saw %q, want the credential on the first hop only", got)
		}
	})

	t.Run("redirect back to the bound host gets it again", func(t *testing.T) {
		before := len(rec.got("allowed.test"))
		if _, err := fetch(t, s, "https://other.test/r?to=https://allowed.test/back"); err != nil {
			t.Fatal(err)
		}
		if got := rec.auth("allowed.test")[before:]; !slices.Equal(got, []string{"Bearer " + testToken}) {
			t.Errorf("allowed.test saw %q", got)
		}
	})

	t.Run("not over plain http", func(t *testing.T) {
		before := len(rec.got("allowed.test"))
		_, err := fetch(t, s, "http://allowed.test:443/a")
		if err == nil || !strings.Contains(err.Error(), "only sent over https") || strings.Contains(err.Error(), testToken) {
			t.Fatalf("err = %v, want the credential refused over http", err)
		}
		if _, err := fetch(t, s, "http://allowed.test/a"); err != nil { // port 80: not bound
			t.Fatal(err)
		}
		if got := rec.auth("allowed.test")[before:]; !slices.Equal(got, []string{""}) {
			t.Errorf("allowed.test saw %q, want one request without the credential", got)
		}
	})

	for _, r := range logs.all() {
		var b strings.Builder
		b.WriteString(r.Message)
		r.Attrs(func(a slog.Attr) bool { b.WriteString(a.String()); return true })
		if strings.Contains(b.String(), testToken) {
			t.Errorf("log holds the token: %s", b.String())
		}
	}
}

// Review B1: a plugin must not read an echoed token in pieces through
// Range requests, each of which would escape redaction.
func TestCredentialRefusesRangeRequests(t *testing.T) {
	rec, s := newCredentialed(t, wasm.Options{})
	for _, h := range []string{"h:Range", "h:If-Range"} {
		_, err := fetch(t, s, "https://allowed.test/echo", h, "bytes=10-20")
		if err == nil || !strings.Contains(err.Error(), "Range requests are not allowed") {
			t.Fatalf("%s: err = %v, want the request refused", h, err)
		}
	}
	if got := rec.got("allowed.test"); len(got) != 0 {
		t.Errorf("Range requests reached the network: %+v", got)
	}
	// Without a credential Range is the plugin's business.
	b, err := fetch(t, s, "https://other.test/echo", "h:Range", "bytes=0-3")
	if err != nil || b != "you " {
		t.Errorf("unbound host Range = %q, %v", b, err)
	}
}

// Review B1: a plugin must not get the compressed body, which it could
// inflate itself after redaction.
func TestCredentialResponseIsDecodedBeforeRedaction(t *testing.T) {
	rec, s := newCredentialed(t, wasm.Options{})

	t.Run("the plugin's Accept-Encoding is dropped", func(t *testing.T) {
		b, err := fetch(t, s, "https://allowed.test/gzip", "h:Accept-Encoding", "gzip")
		if err != nil {
			t.Fatal(err)
		}
		if got := rec.got("allowed.test"); got[len(got)-1].AcceptEncoding != "" {
			t.Errorf("upstream saw Accept-Encoding %q from the plugin", got[len(got)-1].AcceptEncoding)
		}
		if !strings.Contains(b, "[REDACTED]") {
			t.Errorf("body = %q, want the decoded body, redacted", b)
		}
	})

	t.Run("an encoded response is refused", func(t *testing.T) {
		_, err := fetch(t, s, "https://allowed.test/gzip-always")
		if err == nil || !strings.Contains(err.Error(), "cannot be redacted") {
			t.Fatalf("err = %v, want the gzip response refused", err)
		}
	})

	t.Run("a second Content-Encoding header is checked too", func(t *testing.T) {
		_, err := fetch(t, s, "https://allowed.test/gzip-double")
		if err == nil || !strings.Contains(err.Error(), "cannot be redacted") {
			t.Fatalf("err = %v, want the identity+gzip response refused", err)
		}
	})

	t.Run("net/http negotiates and decodes", func(t *testing.T) {
		up := &echoUpstream{}
		srv := httptest.NewTLSServer(up.handler("srv"))
		t.Cleanup(srv.Close)
		u, _ := url.Parse(srv.URL)
		nc, err := netguard.New(netguard.Config{
			Transport:   srv.Client().Transport,
			Allow:       []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
			HostLimits:  map[string]netguard.HostLimit{"127.0.0.1": fastLimit},
			MaxAttempts: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		s := newHostile(t, hostileManifest(t, "127.0.0.1"), wasm.Options{
			HTTPClient:  nc.HTTPClient(),
			Credentials: []wasm.Credential{{Host: u.Host, Token: staticToken(testToken)}},
		})
		b, err := fetch(t, s, srv.URL+"/gzip", "h:Accept-Encoding", "gzip")
		if err != nil {
			t.Fatal(err)
		}
		if got := up.got("srv"); len(got) != 1 || got[0].AcceptEncoding != "gzip" || got[0].Authorization != testToken {
			t.Errorf("upstream saw %+v, want gzip negotiated by net/http and the credential", got)
		}
		if b != `you sent "[REDACTED]"` {
			t.Errorf("body = %q, want the decoded body, redacted", b)
		}
	})
}

// Review S2: headers (values and names), validators and error messages
// are redacted too. The hostile plugin returns only the body, so this goes
// through the HTTPForTest seam, the same path as the host function.
func TestRedactionOfHeadersValidatorsAndErrors(t *testing.T) {
	_, s := newCredentialed(t, wasm.Options{})
	out := s.HTTPForTest(t.Context(), []byte(`{"url":"https://allowed.test/echo"}`))
	var resp struct {
		Status     int                 `json:"status"`
		Headers    map[string][]string `json:"headers"`
		Body       []byte              `json:"body"`
		Validators map[string]string   `json:"validators"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("response = %s", out)
	}
	all := string(out) + string(resp.Body)
	if strings.Contains(strings.ToLower(all), strings.ToLower(testToken)) {
		t.Errorf("the plugin would receive the token: %s / %s", out, resp.Body)
	}
	if got := resp.Headers["x-echo"]; !slices.Equal(got, []string{"Bearer [REDACTED]"}) {
		t.Errorf("x-echo = %q", got)
	}
	if v := resp.Validators; v["etag"] != `"Bearer [REDACTED]"` || v["last_modified"] != "Bearer [REDACTED]" {
		t.Errorf("validators = %v", v)
	}
	if _, ok := resp.Headers["content-length"]; ok {
		t.Errorf("content-length kept after the body changed: %v", resp.Headers["content-length"])
	}

	// A transport error that quotes the header.
	nc, err := netguard.New(netguard.Config{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("upstream said %s", r.Header.Get("Authorization"))
		}),
		Resolver:    fakeResolver{"allowed.test": {netip.MustParseAddr("203.0.113.10")}},
		HostLimits:  map[string]netguard.HostLimit{"allowed.test": fastLimit},
		MaxAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	s2 := newHostile(t, hostileManifest(t, "allowed.test"), wasm.Options{
		HTTPClient:  nc.HTTPClient(),
		Credentials: []wasm.Credential{{Host: "allowed.test", Token: staticToken(testToken)}},
	})
	out = s2.HTTPForTest(t.Context(), []byte(`{"url":"https://allowed.test/"}`))
	if strings.Contains(string(out), testToken) || !strings.Contains(string(out), "upstream said [REDACTED]") {
		t.Errorf("error response = %s, want the token redacted", out)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCredentialTokenErrors(t *testing.T) {
	for name, tok := range map[string]func(context.Context) (string, error){
		"resolver fails": func(context.Context) (string, error) { return "", errors.New("environment variable X is not set") },
		"empty":          staticToken(""),
		"too short":      staticToken("1234567"),
		"header split":   staticToken("abcdefgh\r\nX-Evil: 1"),
	} {
		t.Run(name, func(t *testing.T) {
			rec, s := newCredentialed(t, wasm.Options{Credentials: []wasm.Credential{{Host: "allowed.test", Token: tok}}})
			_, err := fetch(t, s, "https://allowed.test/")
			if err == nil || !strings.Contains(err.Error(), "credential") {
				t.Fatalf("err = %v, want a credential error", err)
			}
			if strings.Contains(err.Error(), "X-Evil") || strings.Contains(err.Error(), "1234567") {
				t.Errorf("error echoes the token: %v", err)
			}
			if got := rec.got("allowed.test"); len(got) != 0 {
				t.Errorf("request reached the network: %+v", got)
			}
		})
	}
}

// AC: the operator's allowed_hosts are merged with the manifest's and
// shown when the plugin is installed.
func TestOperatorAllowedHostsMergedAndShown(t *testing.T) {
	logs := &logRecorder{}
	_, s := newCredentialed(t, wasm.Options{Logger: slog.New(logs)})
	want := []wasm.HostGrant{
		{Host: "allowed.test", Manifest: true, CredentialPorts: []string{"443"}},
		{Host: "other.test", Operator: true, CredentialPorts: []string{}},
	}
	got := s.Hosts()
	if got[1].CredentialPorts == nil {
		got[1].CredentialPorts = []string{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Hosts() = %+v, want %+v", got, want)
	}
	var shown bool
	for _, r := range logs.all() {
		if r.Message == "wasm plugin installed" {
			r.Attrs(func(a slog.Attr) bool {
				shown = shown || (a.Key == "allowed_hosts" && strings.Contains(a.Value.String(), "other.test"))
				return true
			})
		}
	}
	if !shown {
		t.Error("no \"wasm plugin installed\" log with the merged allowed_hosts")
	}
}

func TestInstallErrors(t *testing.T) {
	_, hc := newTestNet(t, nil)
	cred := func(host string) wasm.Credential { return wasm.Credential{Host: host, Token: staticToken(testToken)} }
	cases := map[string]wasm.Options{
		"bad operator host":       {AllowedHosts: []string{"*.example.com"}},
		"credential not listed":   {Credentials: []wasm.Credential{cred("other.test")}},
		"credential twice":        {Credentials: []wasm.Credential{cred("allowed.test"), cred("ALLOWED.test")}},
		"default port twice":      {Credentials: []wasm.Credential{cred("allowed.test"), cred("allowed.test:443")}},
		"no token func":           {Credentials: []wasm.Credential{{Host: "allowed.test"}}},
		"bad scheme":              {Credentials: []wasm.Credential{{Host: "allowed.test", Scheme: "Bearer x", Token: staticToken("x")}}},
		"bad credential":          {Credentials: []wasm.Credential{cred("https://allowed.test")}},
		"port zero":               {Credentials: []wasm.Credential{cred("allowed.test:0")}},
		"port leading zero":       {Credentials: []wasm.Credential{cred("allowed.test:0443")}},
		"port too large":          {Credentials: []wasm.Credential{cred("allowed.test:65536")}},
		"port not a number":       {Credentials: []wasm.Credential{cred("allowed.test:https")}},
		"header not a credential": {Credentials: []wasm.Credential{{Host: "allowed.test", Header: "X-Api-Key", Token: staticToken(testToken)}}},
		"header not a token":      {Credentials: []wasm.Credential{{Host: "allowed.test", Header: "Private Token", Token: staticToken(testToken)}}},
		"scheme on PRIVATE-TOKEN": {Credentials: []wasm.Credential{{Host: "allowed.test", Header: "PRIVATE-TOKEN", Scheme: "Bearer", Token: staticToken(testToken)}}},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			opts.HTTPClient = hc
			if s, err := wasm.New(t.Context(), hostileManifest(t, "allowed.test"), nil, opts); err == nil {
				_ = s.Close(context.Background())
				t.Fatal("New succeeded")
			}
		})
	}
}

// An injected credential counts as authenticated for netguard's limiter.
func TestInjectedCredentialUsesAuthenticatedBudget(t *testing.T) {
	rec := &echoUpstream{}
	n := &testNet{handlers: map[string]http.Handler{"allowed.test": rec.handler("allowed.test")}}
	nc, err := netguard.New(netguard.Config{
		Transport: n,
		Resolver:  fakeResolver{"allowed.test": {netip.MustParseAddr("203.0.113.10")}},
		HostLimits: map[string]netguard.HostLimit{"allowed.test": {
			Authenticated: netguard.Limit{Rate: 1e6, Burst: 1000},
			Anonymous:     netguard.Limit{Rate: 0.001, Burst: 1},
		}},
		MaxAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := newHostile(t, hostileManifest(t, "allowed.test"), wasm.Options{
		HTTPClient:  nc.HTTPClient(),
		Credentials: []wasm.Credential{{Host: "allowed.test", Token: staticToken(testToken)}},
	})
	s2 := newHostile(t, hostileManifest(t, "allowed.test"), wasm.Options{
		HTTPClient:  nc.HTTPClient(),
		Credentials: []wasm.Credential{{Host: "allowed.test", Header: "PRIVATE-TOKEN", Token: staticToken(testToken)}},
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for _, src := range []*wasm.Source{s, s2} {
		for i := range 3 {
			if _, err := src.List(ctx, hostileTarget("fetch", "url", "https://allowed.test/"), source.FetchState{}); err != nil {
				t.Fatalf("request %d: %v (anonymous budget used?)", i, err)
			}
		}
	}
}

// Q3 (#184): only credentials the host injects count as authenticated, so
// a plugin may not set a credential header itself, on any host: the
// request is refused with bad_request before it reaches netguard.
func TestPluginCredentialHeadersRefused(t *testing.T) {
	rec, s := newCredentialed(t, wasm.Options{ExtraAuthHeaders: []string{"X-Api-Key"}})
	for _, tc := range []struct{ host, header string }{
		{"allowed.test", "h:Authorization"},
		{"other.test", "h:Authorization"},
		{"other.test", "h:authorization"},
		{"other.test", "h:PRIVATE-TOKEN"},
		{"other.test", "h:private-token"},
		{"other.test", "h:X-Api-Key"}, // one of netguard's ExtraAuthHeaders
		{"other.test", "h:x-api-key"},
	} {
		_, err := fetch(t, s, "https://"+tc.host+"/a", tc.header, "dummy-value")
		if err == nil || !strings.Contains(err.Error(), "bad_request") || !strings.Contains(err.Error(), "reserved for credentials the host injects") {
			t.Errorf("%s on %s: err = %v, want bad_request", tc.header, tc.host, err)
		}
	}
	// An empty value is refused too: the header is reserved, whatever it holds.
	if _, err := fetch(t, s, "https://other.test/a", "h:Authorization", ""); err == nil {
		t.Error("an empty Authorization from the plugin was accepted")
	}
	if got := len(rec.got("allowed.test")) + len(rec.got("other.test")); got != 0 {
		t.Errorf("%d refused requests reached the network", got)
	}

	// A header that netguard does not count as a credential stays the
	// plugin's business.
	_, s2 := newCredentialed(t, wasm.Options{})
	if _, err := fetch(t, s2, "https://other.test/a", "h:X-Api-Key", "dummy-value"); err != nil {
		t.Errorf("X-Api-Key without ExtraAuthHeaders: %v", err)
	}
}

// A plugin-set credential header can no longer move a plugin onto the
// authenticated budget of a host: the request never reaches the limiter.
func TestPluginCannotClaimAuthenticatedBudget(t *testing.T) {
	rec := &echoUpstream{}
	n := &testNet{handlers: map[string]http.Handler{"allowed.test": rec.handler("allowed.test")}}
	nc, err := netguard.New(netguard.Config{
		Transport: n,
		Resolver:  fakeResolver{"allowed.test": {netip.MustParseAddr("203.0.113.10")}},
		HostLimits: map[string]netguard.HostLimit{"allowed.test": {
			Authenticated: netguard.Limit{Rate: 1e6, Burst: 1000},
			Anonymous:     netguard.Limit{Rate: 0.001, Burst: 1},
		}},
		MaxAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := newHostile(t, hostileManifest(t, "allowed.test"), wasm.Options{HTTPClient: nc.HTTPClient()})
	for _, h := range []string{"authorization", "h:PRIVATE-TOKEN"} {
		_, err := s.List(t.Context(), hostileTarget("fetch", "url", "https://allowed.test/", h, "dummy-value"), source.FetchState{})
		if err == nil || !strings.Contains(err.Error(), "bad_request") {
			t.Errorf("%s: err = %v, want bad_request", h, err)
		}
	}
	if got := rec.got("allowed.test"); len(got) != 0 {
		t.Errorf("requests with a plugin-set credential reached the upstream: %+v", got)
	}
}

// Q2 (#184): a credential can name another header netguard counts as a
// credential, such as GitLab's PRIVATE-TOKEN. It gets the same injection,
// https-only rule and redaction, and no Authorization is sent.
func TestCredentialCustomHeader(t *testing.T) {
	rec, s := newCredentialed(t, wasm.Options{Credentials: []wasm.Credential{
		{Host: "allowed.test", Header: "private-token", Token: staticToken(testToken)},
	}})
	b, err := fetch(t, s, "https://allowed.test/echo")
	if err != nil {
		t.Fatal(err)
	}
	got := rec.got("allowed.test")
	if len(got) != 1 || got[0].PrivateToken != testToken || got[0].Authorization != "" {
		t.Errorf("upstream saw %+v, want PRIVATE-TOKEN only", got)
	}
	if strings.Contains(b, testToken) || !strings.Contains(b, "[REDACTED]") {
		t.Errorf("body = %q, want the echoed token redacted", b)
	}
	if _, err := fetch(t, s, "https://other.test/a"); err != nil {
		t.Fatal(err)
	}
	if got := rec.got("other.test"); len(got) != 1 || got[0].PrivateToken != "" {
		t.Errorf("other.test saw %+v, want no credential", got)
	}
	if _, err := fetch(t, s, "http://allowed.test:443/a"); err == nil || !strings.Contains(err.Error(), "only sent over https") {
		t.Errorf("err = %v, want the credential refused over http", err)
	}

	// One of netguard's ExtraAuthHeaders is accepted too.
	rec2, s2 := newCredentialed(t, wasm.Options{
		ExtraAuthHeaders: []string{"X-Api-Key"},
		Credentials:      []wasm.Credential{{Host: "allowed.test", Header: "X-Api-Key", Token: staticToken(testToken)}},
	})
	if _, err := fetch(t, s2, "https://allowed.test/a"); err != nil {
		t.Fatal(err)
	}
	if got := rec2.got("allowed.test"); len(got) != 1 || got[0].APIKey != testToken {
		t.Errorf("upstream saw %+v, want X-Api-Key", got)
	}
}
