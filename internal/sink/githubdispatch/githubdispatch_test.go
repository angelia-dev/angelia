package githubdispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/sink"
)

//nolint:gosec // G101: a synthetic token for the tests.
const token = "ghp_TESTTOKENTESTTOKENTESTTOKENTESTTOKEN01"

var fixedNow = time.Unix(1793880000, 0)

// guarded returns a netguard client that trusts srv's certificate, with the
// loopback prefix allowed (or not).
func guarded(t *testing.T, srv *httptest.Server, allow bool) *http.Client {
	t.Helper()
	cfg := netguard.Config{
		MaxAttempts: 1, Transport: srv.Client().Transport,
		Proxy: func(*http.Request) (*url.URL, error) { return nil, nil },
	}
	if allow {
		cfg.Allow = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	}
	c, err := netguard.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c.HTTPClient()
}

func newSink(t *testing.T, srv *httptest.Server) *Sink {
	t.Helper()
	return New(guarded(t, srv, true), Options{Now: func() time.Time { return fixedNow }})
}

func ptr(s string) *string { return &s }

func testEnvelope() event.Envelope {
	name := "requests-watch"
	return event.Envelope{
		SpecVersion: "1.0", Type: "dev.angelia.release.published.v1", Source: "https://angelia.example/i",
		ID: "01JD0000000000000000000000", Subject: "pypi:requests@2.33.0", Time: "2026-11-05T10:00:00Z",
		DataContentType: event.DataContentType, DataSchema: event.DataSchema,
		Data: event.Data{
			Source: "pypi", Package: "requests", Version: "2.33.0", NormalizedVersion: "2.33.0", Scheme: "pep440",
			PreviousVersion: ptr("2.32.5"), Bump: ptr("minor"), DetectedAt: "2026-11-05T10:00:00Z",
			PURL: ptr("pkg:pypi/requests@2.33.0"), URLs: event.URLs{Release: ptr("https://pypi.org/project/requests/2.33.0/")},
			Watch: event.Watch{ID: "01JD00000000000000000WATCH", Name: &name, Labels: map[string]string{"team": "core"}},
		},
	}
}

func request(t *testing.T, cfg string, env event.Envelope) sink.Request {
	t.Helper()
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return sink.Request{
		Config: sink.Config(cfg), Event: sink.ReleaseEvent{JSON: b, Envelope: env},
		Rendered:       sink.Rendered{Body: b, ContentType: sink.CloudEventsContentType},
		Secrets:        map[string]secrets.Value{TokenPointer: secrets.NewValueString(token)},
		IdempotencyKey: env.ID + ":01JD0000000000000000000001", DeliveryID: "d1", SubscriptionID: "s1", Attempt: 1,
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

// github is a TLS server playing the GitHub API; h is nil for 204.
func github(t *testing.T, h http.HandlerFunc) (*httptest.Server, *captured) {
	t.Helper()
	c := &captured{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.reqs = append(c.reqs, r)
		c.body = append(c.body, b)
		c.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func repoCfg(base string, extra string) string {
	return fmt.Sprintf(`{"repo":"octo-org/octo-repo","api_base_url":%q,"token_env":"GH_TOKEN"%s}`, base, extra)
}

func wfCfg(base string, extra string) string {
	return fmt.Sprintf(`{"mode":"workflow_dispatch","repo":"octo-org/octo-repo","api_base_url":%q,"token_env":"GH_TOKEN","workflow":"release.yml","ref":"main"%s}`, base, extra)
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, b)
	}
	return m
}

// AC 1 of #113 (wire format): repository_dispatch with the default
// event_type and the fixed flat client_payload.
func TestRepositoryDispatchWire(t *testing.T) {
	srv, got := github(t, nil)
	s := newSink(t, srv)
	res, err := s.Deliver(t.Context(), request(t, repoCfg(srv.URL, ""), testEnvelope()))
	if err != nil || res.StatusCode != http.StatusNoContent {
		t.Fatalf("Deliver = %+v, %v", res, err)
	}
	r, body := got.last()
	if r.Method != http.MethodPost || r.URL.Path != "/repos/octo-org/octo-repo/dispatches" {
		t.Errorf("%s %s", r.Method, r.URL.Path)
	}
	for k, want := range map[string]string{
		"Authorization": "Bearer " + token, "Accept": "application/vnd.github+json",
		"X-Github-Api-Version": "2022-11-28", "Content-Type": "application/json",
	} {
		if got := r.Header.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	want := `{"event_type":"angelia.release.published","client_payload":{"event_id":"01JD0000000000000000000000",` +
		`"source":"pypi","package":"requests","version":"2.33.0","previous_version":"2.32.5","bump":"minor",` +
		`"purl":"pkg:pypi/requests@2.33.0","release_url":"https://pypi.org/project/requests/2.33.0/","watch":"requests-watch"}}`
	if string(body) != want {
		t.Errorf("body\n got %s\nwant %s", body, want)
	}
}

// previous_version is null when unknown, and the watch id stands in for a
// missing watch name.
func TestRepositoryDispatchNulls(t *testing.T) {
	srv, got := github(t, nil)
	env := testEnvelope()
	env.Data.PreviousVersion, env.Data.Bump, env.Data.PURL, env.Data.URLs.Release, env.Data.Watch.Name = nil, nil, nil, nil, nil
	if _, err := newSink(t, srv).Deliver(t.Context(), request(t, repoCfg(srv.URL, ""), env)); err != nil {
		t.Fatal(err)
	}
	_, body := got.last()
	cp := decode(t, body)["client_payload"].(map[string]any)
	for _, k := range []string{"previous_version", "bump", "purl", "release_url"} {
		if v, ok := cp[k]; !ok || v != nil {
			t.Errorf("%s = %v (present %v), want null", k, v, ok)
		}
	}
	if cp["watch"] != "01JD00000000000000000WATCH" {
		t.Errorf("watch = %v", cp["watch"])
	}
	if _, ok := cp["truncated"]; ok {
		t.Error("truncated is set although nothing was cut")
	}
}

// event_type is a template with the subscription context.
func TestEventTypeTemplate(t *testing.T) {
	srv, got := github(t, nil)
	cfg := repoCfg(srv.URL, `,"event_type":"release-{{.data.source}}-{{.data.bump}}"`)
	if _, err := newSink(t, srv).Deliver(t.Context(), request(t, cfg, testEnvelope())); err != nil {
		t.Fatal(err)
	}
	if _, body := got.last(); decode(t, body)["event_type"] != "release-pypi-minor" {
		t.Errorf("body %s", body)
	}
}

// AC 1 of #113: the payload fits GitHub's limits: at most 10 top-level keys
// of client_payload, 65535 characters, whatever the event holds; every
// string is cut to 2048 bytes at a UTF-8 boundary and truncated says so.
func TestClientPayloadFitsGitHubLimits(t *testing.T) {
	huge := strings.Repeat("é", 4000) // 8000 bytes, two bytes per character
	ctrl := strings.Repeat("\x01", 4000)
	cases := map[string]string{
		"ascii":     strings.Repeat("a", 200<<10),
		"multibyte": huge,
		"control":   ctrl, // escaped to six characters each
		"emoji":     strings.Repeat("😀", 3000),
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			srv, got := github(t, nil)
			env := testEnvelope()
			d := &env.Data
			d.Package, d.Version, d.Source = v, v, v
			d.PreviousVersion, d.Bump, d.PURL, d.URLs.Release = &v, &v, &v, &v
			d.Watch.Name = &v
			env.ID = v
			if _, err := newSink(t, srv).Deliver(t.Context(), request(t, repoCfg(srv.URL, ""), env)); err != nil {
				t.Fatal(err)
			}
			_, body := got.last()
			if len(body) > 65535 {
				t.Errorf("body is %d bytes, GitHub accepts 65535", len(body))
			}
			if et := decode(t, body)["event_type"].(string); et != DefaultEventType {
				t.Errorf("event_type %q", et)
			}
			cp := decode(t, body)["client_payload"].(map[string]any)
			if len(cp) > 10 {
				t.Errorf("client_payload has %d top-level keys, GitHub accepts 10", len(cp))
			}
			if cp["truncated"] != true {
				t.Errorf("truncated = %v, want true", cp["truncated"])
			}
			for k, e := range cp {
				s, ok := e.(string)
				if !ok {
					continue
				}
				if len(s) > MaxPayloadStringBytes || !utf8.ValidString(s) || strings.ContainsRune(s, utf8.RuneError) {
					t.Errorf("%s: %d bytes, valid UTF-8 %v", k, len(s), utf8.ValidString(s))
				}
			}
		})
	}
}

// A value of exactly 2048 bytes is not cut and does not set truncated; 2049
// is cut to 2048 (or fewer, at a character boundary).
func TestTruncationBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, v   string
		wantLen   int
		truncated bool
	}{
		{"exact", strings.Repeat("a", 2048), 2048, false},
		{"over", strings.Repeat("a", 2049), 2048, true},
		{"mid-character", strings.Repeat("a", 2047) + "é", 2047, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, got := github(t, nil)
			env := testEnvelope()
			env.Data.Version = tc.v
			if _, err := newSink(t, srv).Deliver(t.Context(), request(t, repoCfg(srv.URL, ""), env)); err != nil {
				t.Fatal(err)
			}
			_, body := got.last()
			cp := decode(t, body)["client_payload"].(map[string]any)
			if n := len(cp["version"].(string)); n != tc.wantLen {
				t.Errorf("version is %d bytes, want %d", n, tc.wantLen)
			}
			if _, ok := cp["truncated"]; ok != tc.truncated {
				t.Errorf("truncated present = %v, want %v", ok, tc.truncated)
			}
		})
	}
}

func TestCut(t *testing.T) {
	if s, c := cut("héllo", 2); s != "h" || !c {
		t.Errorf("cut = %q, %v", s, c)
	}
	if s, c := cut("héllo", 3); s != "hé" || !c {
		t.Errorf("cut = %q, %v", s, c)
	}
	if s, c := cut("abc", 3); s != "abc" || c {
		t.Errorf("cut = %q, %v", s, c)
	}
}

func TestWorkflowDispatch(t *testing.T) {
	srv, got := github(t, nil)
	cfg := wfCfg(srv.URL, `,"inputs":{"version":"{{.data.version}}","package":"{{.data.package}}","note":"literal"}`)
	if _, err := newSink(t, srv).Deliver(t.Context(), request(t, cfg, testEnvelope())); err != nil {
		t.Fatal(err)
	}
	r, body := got.last()
	if r.URL.Path != "/repos/octo-org/octo-repo/actions/workflows/release.yml/dispatches" {
		t.Errorf("path %s", r.URL.Path)
	}
	want := `{"ref":"main","inputs":{"note":"literal","package":"requests","version":"2.33.0"}}`
	if string(body) != want {
		t.Errorf("body\n got %s\nwant %s", body, want)
	}
}

func TestWorkflowDispatchNumericIDAndNoInputs(t *testing.T) {
	srv, got := github(t, nil)
	cfg := strings.Replace(wfCfg(srv.URL, ""), `"release.yml"`, `"1234567"`, 1)
	if _, err := newSink(t, srv).Deliver(t.Context(), request(t, cfg, testEnvelope())); err != nil {
		t.Fatal(err)
	}
	r, body := got.last()
	if r.URL.Path != "/repos/octo-org/octo-repo/actions/workflows/1234567/dispatches" || string(body) != `{"ref":"main","inputs":{}}` {
		t.Errorf("%s %s", r.URL.Path, body)
	}
}

// Inputs are cut to 1024 bytes at a UTF-8 boundary, with no marker, and the
// body stays below GitHub's limit even when escaping multiplies it.
func TestWorkflowInputsCut(t *testing.T) {
	srv, got := github(t, nil)
	env := testEnvelope()
	env.Data.Version = strings.Repeat("é", 2000)
	env.Data.Package = strings.Repeat("\x01", 2000)
	inputs := []string{}
	for i := range MaxInputs {
		inputs = append(inputs, fmt.Sprintf(`"i%d":"{{.data.package}}"`, i))
	}
	cfg := wfCfg(srv.URL, `,"inputs":{"v":"{{.data.version}}",`+strings.Join(inputs, ",")+`}`)
	if err := newSink(t, srv).Validate(sink.Config(cfg)); err == nil {
		t.Fatal("26 inputs accepted")
	}
	cfg = wfCfg(srv.URL, `,"inputs":{"v":"{{.data.version}}",`+strings.Join(inputs[:MaxInputs-1], ",")+`}`)
	if _, err := newSink(t, srv).Deliver(t.Context(), request(t, cfg, env)); err != nil {
		t.Fatal(err)
	}
	_, body := got.last()
	if len(body) > 65535 {
		t.Errorf("body is %d bytes", len(body))
	}
	in := decode(t, body)["inputs"].(map[string]any)
	v := in["v"].(string)
	if len(v) > MaxInputBytes || !utf8.ValidString(v) || len(in) != MaxInputs {
		t.Errorf("v is %d bytes (valid %v), %d inputs", len(v), utf8.ValidString(v), len(in))
	}
	if strings.Contains(string(body), "truncated") {
		t.Error("inputs carry a marker")
	}
}

func TestWorkflowInputExactly1024(t *testing.T) {
	srv, got := github(t, nil)
	env := testEnvelope()
	env.Data.Version = strings.Repeat("a", 1024)
	cfg := wfCfg(srv.URL, `,"inputs":{"v":"{{.data.version}}"}`)
	if _, err := newSink(t, srv).Deliver(t.Context(), request(t, cfg, env)); err != nil {
		t.Fatal(err)
	}
	_, body := got.last()
	if v := decode(t, body)["inputs"].(map[string]any)["v"].(string); len(v) != 1024 {
		t.Errorf("v is %d bytes", len(v))
	}
}

// GitHub Enterprise Server: the API lives under /api/v3, and the token goes
// there.
func TestEnterpriseBaseURL(t *testing.T) {
	srv, got := github(t, nil)
	cfg := repoCfg(srv.URL+"/api/v3/", "")
	if _, err := newSink(t, srv).Deliver(t.Context(), request(t, cfg, testEnvelope())); err != nil {
		t.Fatal(err)
	}
	if r, _ := got.last(); r.URL.Path != "/api/v3/repos/octo-org/octo-repo/dispatches" {
		t.Errorf("path %s", r.URL.Path)
	}
}

func TestDefaultBaseURL(t *testing.T) {
	p, err := parseConfig(sink.Config(`{"repo":"o/r","token_ref":"gh-token"}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if p.APIBaseURL != DefaultAPIBaseURL || p.Mode != ModeRepositoryDispatch || p.EventType != DefaultEventType || p.timeout != DefaultTimeout {
		t.Errorf("defaults: %+v", p)
	}
}

func TestTokenRef(t *testing.T) {
	srv, got := github(t, nil)
	req := request(t, fmt.Sprintf(`{"repo":"o/r","api_base_url":%q,"token_ref":"gh-token"}`, srv.URL), testEnvelope())
	req.Secrets = map[string]secrets.Value{TokenRefPointer: secrets.NewValueString(token + "\n")}
	if _, err := newSink(t, srv).Deliver(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if r, _ := got.last(); r.Header.Get("Authorization") != "Bearer "+token {
		t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
	}
}

// AC 2 of #113: 422 and 404 are permanent, 5xx is retried; a rate-limited
// 403 or 429 is retried after Retry-After or x-ratelimit-reset (spec
// decision 38.4); any other 4xx and a redirect are permanent.
func TestResponses(t *testing.T) {
	reset := fmt.Sprint(fixedNow.Add(90 * time.Second).Unix())
	for _, tc := range []struct {
		name      string
		code      int
		header    map[string]string
		permanent bool
		retry     time.Duration
		ok        bool
	}{
		{"204", 204, nil, false, 0, true},
		{"200", 200, nil, false, 0, true},
		{"404", 404, nil, true, 0, false},
		{"422", 422, nil, true, 0, false},
		{"401", 401, nil, true, 0, false},
		{"403 plain", 403, nil, true, 0, false},
		{"403 remaining", 403, map[string]string{"X-RateLimit-Remaining": "4999", "X-RateLimit-Reset": reset}, true, 0, false},
		{"403 exhausted", 403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset}, false, 90 * time.Second, false},
		{"403 retry-after", 403, map[string]string{"Retry-After": "60"}, false, time.Minute, false},
		{"403 exhausted, reset past", 403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1"}, false, 0, false},
		{"403 retry-after wins when longer", 403, map[string]string{"Retry-After": "120", "X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset}, false, 2 * time.Minute, false},
		{"429", 429, map[string]string{"Retry-After": "30"}, false, 30 * time.Second, false},
		{"429 reset", 429, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset}, false, 90 * time.Second, false},
		{"429 bare", 429, nil, false, 0, false},
		{"408", 408, nil, false, 0, false},
		{"500", 500, nil, false, 0, false},
		{"502", 502, map[string]string{"Retry-After": "5"}, false, 5 * time.Second, false},
		{"503", 503, nil, false, 0, false},
		{"302", 302, map[string]string{"Location": "https://elsewhere.example/x"}, true, 0, false},
		{"400", 400, nil, true, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, got := github(t, func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(`{"message":"` + token + `"}`)) // a body is never echoed
			})
			res, err := newSink(t, srv).Deliver(t.Context(), request(t, repoCfg(srv.URL, ""), testEnvelope()))
			if res.StatusCode != tc.code {
				t.Errorf("StatusCode = %d", res.StatusCode)
			}
			if tc.ok {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("no error")
			}
			if sink.IsPermanent(err) != tc.permanent {
				t.Errorf("permanent = %v, want %v (%v)", sink.IsPermanent(err), tc.permanent, err)
			}
			if got := sink.RetryAfter(err); got != tc.retry {
				t.Errorf("RetryAfter = %v, want %v", got, tc.retry)
			}
			if !strings.HasPrefix(err.Error(), "github-dispatch: "+srv.URL+": HTTP ") {
				t.Errorf("error %q", err)
			}
			if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "/repos/") || strings.Contains(err.Error(), "octo-org") {
				t.Errorf("error leaks the token or the path: %q", err)
			}
			if got.count() != 1 {
				t.Errorf("%d requests", got.count())
			}
		})
	}
}

func TestStatusHints(t *testing.T) {
	for code, want := range map[int]string{404: "repository, workflow or token without access", 422: "refused the payload or inputs", 401: "token is not accepted", 403: "actions: write"} {
		srv, _ := github(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
		_, err := newSink(t, srv).Deliver(t.Context(), request(t, repoCfg(srv.URL, ""), testEnvelope()))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%d: %v", code, err)
		}
	}
}

// The token goes to api_base_url only: a redirect is not followed.
func TestRedirectNotFollowed(t *testing.T) {
	other, otherGot := github(t, nil)
	srv, _ := github(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusTemporaryRedirect)
	})
	_, err := newSink(t, srv).Deliver(t.Context(), request(t, repoCfg(srv.URL, ""), testEnvelope()))
	if err == nil || !sink.IsPermanent(err) || !strings.Contains(err.Error(), "redirect not followed") {
		t.Fatalf("err = %v", err)
	}
	if otherGot.count() != 0 {
		t.Error("the redirect target was contacted")
	}
}

// A template that fails to render is permanent, and says where, not what.
func TestTemplateFailuresArePermanent(t *testing.T) {
	srv, got := github(t, nil)
	for name, cfg := range map[string]string{
		"missing key":      repoCfg(srv.URL, `,"event_type":"x-{{.data.pakage}}"`),
		"too long":         repoCfg(srv.URL, `,"event_type":"`+strings.Repeat("x", 101)+`"`),
		"blank":            repoCfg(srv.URL, `,"event_type":"   "`),
		"input missing":    wfCfg(srv.URL, `,"inputs":{"a":"{{.data.pakage}}"}`),
		"null guard":       repoCfg(srv.URL, `,"event_type":"{{.data.notes.body}}"`),
		"bad input syntax": wfCfg(srv.URL, `,"inputs":{"a":"{{"}`),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newSink(t, srv).Deliver(t.Context(), request(t, cfg, testEnvelope()))
			if err == nil || !sink.IsPermanent(err) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	if got.count() != 0 {
		t.Errorf("%d requests were sent", got.count())
	}
}

// The rendered event_type is checked at delivery, in characters.
func TestEventTypeCharacterLimit(t *testing.T) {
	srv, got := github(t, nil)
	cfg := repoCfg(srv.URL, `,"event_type":"`+strings.Repeat("é", 100)+`"`)
	if _, err := newSink(t, srv).Deliver(t.Context(), request(t, cfg, testEnvelope())); err != nil {
		t.Fatalf("100 characters: %v", err)
	}
	if got.count() != 1 {
		t.Error("not sent")
	}
}

func TestTokenErrors(t *testing.T) {
	srv, got := github(t, nil)
	cfg := repoCfg(srv.URL, "")
	for name, mod := range map[string]func(*sink.Request){
		"unresolved": func(r *sink.Request) { r.Secrets = nil },
		"empty":      func(r *sink.Request) { r.Secrets[TokenPointer] = secrets.NewValueString("  \n") },
		"space":      func(r *sink.Request) { r.Secrets[TokenPointer] = secrets.NewValueString("ghp_a b") },
		"newline":    func(r *sink.Request) { r.Secrets[TokenPointer] = secrets.NewValueString("ghp_secretpart\nX-Evil: 1") },
		"non-ascii":  func(r *sink.Request) { r.Secrets[TokenPointer] = secrets.NewValueString("ghp_é") },
	} {
		t.Run(name, func(t *testing.T) {
			req := request(t, cfg, testEnvelope())
			mod(&req)
			_, err := newSink(t, srv).Deliver(t.Context(), req)
			if err == nil || sink.IsPermanent(err) {
				t.Fatalf("err = %v, want a retried error", err)
			}
			if !strings.Contains(err.Error(), "token_env GH_TOKEN") || strings.Contains(err.Error(), "secretpart") || strings.Contains(err.Error(), "ghp_") {
				t.Errorf("error %q", err)
			}
		})
	}
	if got.count() != 0 {
		t.Error("a request was sent without a valid token")
	}
}

// Transport errors are retried with a fixed description; an SSRF refusal is
// permanent and names the host only.
func TestTransportErrors(t *testing.T) {
	srv, _ := github(t, nil)
	t.Run("blocked", func(t *testing.T) {
		s := New(guarded(t, srv, false), Options{})
		_, err := s.Deliver(t.Context(), request(t, repoCfg(srv.URL, ""), testEnvelope()))
		if err == nil || !sink.IsPermanent(err) || !errors.Is(err, netguard.ErrBlocked) || err.Error() != "github-dispatch: destination not allowed (127.0.0.1)" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("refused", func(t *testing.T) {
		dead := httptest.NewTLSServer(nil)
		cfg := repoCfg(dead.URL, "")
		hc := guarded(t, dead, true)
		dead.Close()
		_, err := New(hc, Options{}).Deliver(t.Context(), request(t, cfg, testEnvelope()))
		if err == nil || sink.IsPermanent(err) || err.Error() != "github-dispatch: "+dead.URL+": connection refused" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		slow, _ := github(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		cfg := repoCfg(slow.URL, `,"timeout":"1s"`)
		_, err := newSink(t, slow).Deliver(t.Context(), request(t, cfg, testEnvelope()))
		if err == nil || sink.IsPermanent(err) || !strings.HasSuffix(err.Error(), ": timeout") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("reset", func(t *testing.T) {
		rst, _ := github(t, func(w http.ResponseWriter, _ *http.Request) {
			if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = c.Close()
			}
		})
		_, err := newSink(t, rst).Deliver(t.Context(), request(t, repoCfg(rst.URL, ""), testEnvelope()))
		if err == nil || sink.IsPermanent(err) || !strings.HasSuffix(err.Error(), ": connection reset") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("tls", func(t *testing.T) {
		// The client does not trust this server's certificate.
		plain := httptest.NewTLSServer(nil)
		defer plain.Close()
		c, err := netguard.New(netguard.Config{
			MaxAttempts: 1, Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
			Proxy: func(*http.Request) (*url.URL, error) { return nil, nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		_, derr := New(c.HTTPClient(), Options{}).Deliver(t.Context(), request(t, repoCfg(plain.URL, ""), testEnvelope()))
		if derr == nil || sink.IsPermanent(derr) || !strings.HasSuffix(derr.Error(), ": TLS handshake failed") {
			t.Fatalf("err = %v", derr)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Nanosecond)
		defer cancel()
		<-ctx.Done()
		_, err := newSink(t, srv).Deliver(ctx, request(t, repoCfg(srv.URL, ""), testEnvelope()))
		if err == nil || sink.IsPermanent(err) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestFailureClass(t *testing.T) {
	for want, err := range map[string]error{
		"canceled":           context.Canceled,
		"DNS lookup failed":  errors.New("netguard: resolve host: no such host"),
		"connection reset":   io.ErrUnexpectedEOF,
		"request failed":     errors.New("something else on 10.0.0.1:443"),
		"connection refused": fmt.Errorf("dial tcp 10.0.0.1:443: %w", errConnRefused),
	} {
		if got := failureClass(err); got != want {
			t.Errorf("failureClass(%v) = %q, want %q", err, got, want)
		}
	}
}

// Every error of every scenario keeps the token, the Authorization header
// and the repository path out of its text.
func TestErrorsNeverHoldTheToken(t *testing.T) {
	var all []string
	for _, code := range []int{200, 204, 401, 403, 404, 422, 429, 500, 302} {
		srv, _ := github(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Echo", r.Header.Get("Authorization")) // a server that echoes the credential
			w.WriteHeader(code)
		})
		_, err := newSink(t, srv).Deliver(t.Context(), request(t, repoCfg(srv.URL, ""), testEnvelope()))
		if err != nil {
			all = append(all, err.Error())
		}
	}
	for _, e := range all {
		if strings.Contains(e, token) || strings.Contains(e, "Bearer") || strings.Contains(e, "GH_TOKEN=") {
			t.Errorf("error holds a credential: %q", e)
		}
	}
	if len(all) < 6 {
		t.Fatalf("%d errors captured", len(all))
	}
}

func TestRegistryAndNew(t *testing.T) {
	srv, _ := github(t, nil)
	r := sink.NewRegistry()
	if err := r.Register(newSink(t, srv)); err != nil {
		t.Fatal(err)
	}
	if s, err := r.Get("github-dispatch"); err != nil || s.Kind() != Kind {
		t.Fatalf("Get = %v, %v", s, err)
	}
	hc := guarded(t, srv, true)
	New(hc, Options{})
	if err := hc.CheckRedirect(nil, nil); errors.Is(err, http.ErrUseLastResponse) {
		t.Fatal("the caller's client stops following redirects")
	}
	defer func() {
		if recover() == nil {
			t.Error("New(nil) did not panic")
		}
	}()
	New(nil, Options{})
}

func TestDeliverRejectsInvalidStoredConfig(t *testing.T) {
	srv, got := github(t, nil)
	_, err := newSink(t, srv).Deliver(t.Context(), request(t, `{"repo":"nope"}`, testEnvelope()))
	if err == nil || !sink.IsPermanent(err) {
		t.Fatalf("err = %v", err)
	}
	if got.count() != 0 {
		t.Error("sent")
	}
}

func TestEventFallsBackToEnvelope(t *testing.T) {
	srv, got := github(t, nil)
	req := request(t, repoCfg(srv.URL, `,"event_type":"x-{{.data.source}}"`), testEnvelope())
	req.Event.JSON = nil
	if _, err := newSink(t, srv).Deliver(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if _, body := got.last(); decode(t, body)["event_type"] != "x-pypi" {
		t.Errorf("%s", body)
	}
}

// guardedNoServer is a netguard client for tests that never connect.
func guardedNoServer(t *testing.T) *http.Client {
	t.Helper()
	c, err := netguard.New(netguard.Config{Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return c.HTTPClient()
}

func TestEmptyInputValue(t *testing.T) {
	srv, got := github(t, nil)
	cfg := wfCfg(srv.URL, `,"inputs":{"a":""}`)
	if err := newSink(t, srv).Validate(sink.Config(cfg)); err != nil {
		t.Fatal(err)
	}
	if _, err := newSink(t, srv).Deliver(t.Context(), request(t, cfg, testEnvelope())); err != nil {
		t.Fatal(err)
	}
	if _, body := got.last(); string(body) != `{"ref":"main","inputs":{"a":""}}` {
		t.Errorf("%s", body)
	}
}
