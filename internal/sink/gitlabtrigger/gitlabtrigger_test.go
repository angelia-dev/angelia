package gitlabtrigger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
const token = "glptt-TESTTOKENTESTTOKENTESTTOKEN0123456789"

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

// guardedNoServer is a netguard client for tests that never connect.
func guardedNoServer(t *testing.T) *http.Client {
	t.Helper()
	c, err := netguard.New(netguard.Config{Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }})
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
		SpecVersion: "1.0", Type: event.TypePublished, Source: "https://angelia.example/i",
		ID: "01JD0000000000000000000000", Subject: "pypi:requests@2.33.0", Time: "2026-11-05T10:00:00Z",
		DataContentType: event.DataContentType, DataSchema: event.DataSchema,
		Data: event.Data{
			Source: "pypi", Package: "requests", Version: "2.33.0", NormalizedVersion: "2.33.0", Scheme: "pep440",
			PreviousVersion: ptr("2.32.5"), Bump: ptr("minor"), DetectedAt: "2026-11-05T10:00:00Z",
			PURL: ptr("pkg:pypi/requests@2.33.0"), URLs: event.URLs{Release: ptr("https://pypi.org/project/requests/2.33.0/?a=1&b=2")},
			Watch: event.Watch{ID: "01JD00000000000000000WATCH", Name: &name, Labels: map[string]string{"team": "core"}},
		},
	}
}

const idemKey = "01JD0000000000000000000000:01JD0000000000000000000001"

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
		IdempotencyKey: idemKey, DeliveryID: "d1", SubscriptionID: "s1", Attempt: 1,
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

// gitlab is a TLS server playing the GitLab API; h is nil for 201.
func gitlab(t *testing.T, h http.HandlerFunc) (*httptest.Server, *captured) {
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1,"status":"created","web_url":"https://gitlab.example/g/p/-/pipelines/1"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func cfgOf(base, extra string) string {
	return fmt.Sprintf(`{"project":"platform/deploys/app","ref":"main","base_url":%q,"token_env":"GL_TOKEN"%s}`, base, extra)
}

type wireBody struct {
	Token     string            `json:"token"`
	Ref       string            `json:"ref"`
	Variables map[string]string `json:"variables"`
	Inputs    map[string]string `json:"inputs"`
}

func decode(t *testing.T, b []byte) (wireBody, map[string]any) {
	t.Helper()
	var w wireBody
	var m map[string]any
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, b)
	}
	_ = json.Unmarshal(b, &m)
	return w, m
}

// 90-B wire format: one POST to the trigger endpoint of the project (a
// subgroup path is one encoded segment), JSON, the token only in the body,
// an empty query.
func TestWire(t *testing.T) {
	srv, got := gitlab(t, nil)
	res, err := newSink(t, srv).Deliver(t.Context(), request(t, cfgOf(srv.URL, ""), testEnvelope()))
	if err != nil || res.StatusCode != http.StatusCreated {
		t.Fatalf("Deliver = %+v, %v", res, err)
	}
	r, body := got.last()
	if r.Method != http.MethodPost || r.URL.EscapedPath() != "/api/v4/projects/platform%2Fdeploys%2Fapp/trigger/pipeline" {
		t.Errorf("%s %s", r.Method, r.URL.EscapedPath())
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		t.Errorf("query = %q", r.URL.RawQuery)
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	for _, h := range []string{"Authorization", "Private-Token", "Job-Token"} {
		if r.Header.Get(h) != "" {
			t.Errorf("header %s is set", h)
		}
	}
	for k, vs := range r.Header {
		for _, v := range vs {
			if strings.Contains(v, token) {
				t.Errorf("header %s holds the token", k)
			}
		}
	}
	if strings.Contains(r.URL.String(), token) {
		t.Error("the URL holds the token")
	}
	w, raw := decode(t, body)
	if w.Token != token || w.Ref != "main" {
		t.Errorf("body %s", body)
	}
	if _, ok := raw["inputs"]; ok {
		t.Error("empty inputs are sent")
	}
}

func TestNumericProjectAndRelativeRoot(t *testing.T) {
	srv, got := gitlab(t, nil)
	cfg := fmt.Sprintf(`{"project":"4711","ref":"main","base_url":%q,"token_ref":"gl"}`, srv.URL+"/gitlab/api/v4/")
	req := request(t, cfg, testEnvelope())
	req.Secrets = map[string]secrets.Value{TokenRefPointer: secrets.NewValueString(token + "\n")}
	if _, err := newSink(t, srv).Deliver(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	r, body := got.last()
	if r.URL.EscapedPath() != "/gitlab/api/v4/projects/4711/trigger/pipeline" {
		t.Errorf("path %s", r.URL.EscapedPath())
	}
	if w, _ := decode(t, body); w.Token != token {
		t.Errorf("token %q: it must be trimmed", w.Token)
	}
}

// D71: with neither variables nor inputs, the ANGELIA_* variables, in a
// golden body (the JSON is not HTML-escaped: the release URL keeps its &).
func TestDefaultVariablesGolden(t *testing.T) {
	srv, got := gitlab(t, nil)
	if _, err := newSink(t, srv).Deliver(t.Context(), request(t, cfgOf(srv.URL, ""), testEnvelope())); err != nil {
		t.Fatal(err)
	}
	_, body := got.last()
	want := `{"token":"` + token + `","ref":"main","variables":{` +
		`"ANGELIA_BUMP":"minor","ANGELIA_EVENT_ID":"01JD0000000000000000000000","ANGELIA_EVENT_TYPE":"release.published",` +
		`"ANGELIA_IDEMPOTENCY_KEY":"` + idemKey + `","ANGELIA_PACKAGE":"requests","ANGELIA_PREVIOUS_VERSION":"2.32.5",` +
		`"ANGELIA_PURL":"pkg:pypi/requests@2.33.0","ANGELIA_RELEASE_URL":"https://pypi.org/project/requests/2.33.0/?a=1&b=2",` +
		`"ANGELIA_SOURCE":"pypi","ANGELIA_VERSION":"2.33.0","ANGELIA_WATCH":"requests-watch"}}`
	if string(body) != want {
		t.Errorf("body\n got %s\nwant %s", body, want)
	}
}

// A yanked event has the short type release.yanked; null fields are empty
// strings; the watch id stands in for a missing name.
func TestDefaultVariablesYankedAndNulls(t *testing.T) {
	srv, got := gitlab(t, nil)
	env := testEnvelope()
	env.Type = event.TypeYanked
	env.Data.Yanked = true
	env.Data.PreviousVersion, env.Data.Bump, env.Data.PURL, env.Data.URLs.Release, env.Data.Watch.Name = nil, nil, nil, nil, nil
	if _, err := newSink(t, srv).Deliver(t.Context(), request(t, cfgOf(srv.URL, ""), env)); err != nil {
		t.Fatal(err)
	}
	_, body := got.last()
	w, _ := decode(t, body)
	for k, want := range map[string]string{
		"ANGELIA_EVENT_TYPE": "release.yanked", "ANGELIA_PREVIOUS_VERSION": "", "ANGELIA_BUMP": "", "ANGELIA_PURL": "",
		"ANGELIA_RELEASE_URL": "", "ANGELIA_WATCH": "01JD00000000000000000WATCH",
	} {
		if v, ok := w.Variables[k]; !ok || v != want {
			t.Errorf("%s = %q (present %v), want %q", k, v, ok, want)
		}
	}
	if len(w.Variables) != 11 {
		t.Errorf("%d variables, want 11", len(w.Variables))
	}
	if _, ok := w.Variables["ANGELIA_TRUNCATED"]; ok {
		t.Error("ANGELIA_TRUNCATED is set although nothing was cut")
	}
}

// Every default value is cut to 2048 bytes at a UTF-8 boundary, and
// ANGELIA_TRUNCATED=true says so.
func TestDefaultVariablesCut(t *testing.T) {
	srv, got := gitlab(t, nil)
	env := testEnvelope()
	env.Data.Version = strings.Repeat("é", 1500) // 3000 bytes; 2048 is a boundary
	env.Data.Package = strings.Repeat("a", 2047) + "é"
	env.Data.Bump = ptr(strings.Repeat("b", 2048)) // exactly the limit: not cut
	if _, err := newSink(t, srv).Deliver(t.Context(), request(t, cfgOf(srv.URL, ""), env)); err != nil {
		t.Fatal(err)
	}
	_, body := got.last()
	w, _ := decode(t, body)
	if v := w.Variables["ANGELIA_VERSION"]; len(v) != 2048 || !utf8.ValidString(v) || v != strings.Repeat("é", 1024) {
		t.Errorf("version: %d bytes", len(v))
	}
	if v := w.Variables["ANGELIA_PACKAGE"]; v != strings.Repeat("a", 2047) || !utf8.ValidString(v) {
		t.Errorf("package: %d bytes, cut inside a character", len(v))
	}
	if v := w.Variables["ANGELIA_BUMP"]; len(v) != 2048 {
		t.Errorf("bump: %d bytes", len(v))
	}
	if w.Variables["ANGELIA_TRUNCATED"] != "true" {
		t.Error("ANGELIA_TRUNCATED is not true")
	}
}

func TestCut(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
		cut  bool
	}{
		{"abc", 3, "abc", false}, {"abcd", 3, "abc", true}, {"aéb", 2, "a", true}, {"aéb", 3, "aé", true}, {"é", 0, "", true},
	} {
		if got, c := cut(tc.in, tc.n); got != tc.want || c != tc.cut {
			t.Errorf("cut(%q, %d) = %q, %v", tc.in, tc.n, got, c)
		}
	}
}

// D71: with variables and/or inputs set, only those are sent, as rendered
// templates; the defaults are not added.
func TestUserVariablesAndInputs(t *testing.T) {
	srv, got := gitlab(t, nil)
	extra := `,"variables":{"RELEASE_VERSION":"{{.data.version}}","LITERAL":"x","EMPTY":""},"inputs":{"package":"{{.data.package}}","bump-level":"{{.data.bump}}"}`
	if _, err := newSink(t, srv).Deliver(t.Context(), request(t, cfgOf(srv.URL, extra), testEnvelope())); err != nil {
		t.Fatal(err)
	}
	_, body := got.last()
	want := `{"token":"` + token + `","ref":"main","variables":{"EMPTY":"","LITERAL":"x","RELEASE_VERSION":"2.33.0"},` +
		`"inputs":{"bump-level":"minor","package":"requests"}}`
	if string(body) != want {
		t.Errorf("body\n got %s\nwant %s", body, want)
	}
}

func TestInputsOnlyAndVariablesOnly(t *testing.T) {
	srv, got := gitlab(t, nil)
	for name, tc := range map[string]struct {
		extra     string
		wantKey   string
		absentKey string
	}{
		"inputs only":    {`,"inputs":{"v":"{{.data.version}}"}`, "inputs", "variables"},
		"variables only": {`,"variables":{"V":"{{.data.version}}"}`, "variables", "inputs"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := newSink(t, srv).Deliver(t.Context(), request(t, cfgOf(srv.URL, tc.extra), testEnvelope())); err != nil {
				t.Fatal(err)
			}
			_, body := got.last()
			_, raw := decode(t, body)
			if _, ok := raw[tc.wantKey]; !ok {
				t.Errorf("no %s in %s", tc.wantKey, body)
			}
			if _, ok := raw[tc.absentKey]; ok {
				t.Errorf("%s in %s", tc.absentKey, body)
			}
		})
	}
}

// A configured value is cut to 2048 bytes at a UTF-8 boundary, with no marker.
func TestUserValuesCut(t *testing.T) {
	srv, got := gitlab(t, nil)
	env := testEnvelope()
	env.Data.Version = strings.Repeat("é", 1500)
	extra := `,"inputs":{"v":"{{.data.version}}"}`
	if _, err := newSink(t, srv).Deliver(t.Context(), request(t, cfgOf(srv.URL, extra), env)); err != nil {
		t.Fatal(err)
	}
	_, body := got.last()
	w, raw := decode(t, body)
	if v := w.Inputs["v"]; v != strings.Repeat("é", 1024) {
		t.Errorf("input: %d bytes", len(v))
	}
	if strings.Contains(string(body), "TRUNCATED") || raw["variables"] != nil {
		t.Errorf("marker or defaults in %s", body)
	}
}

// 90-B responses (D74).
func TestResponses(t *testing.T) {
	reset := fmt.Sprint(fixedNow.Add(90 * time.Second).Unix())
	farReset := fmt.Sprint(fixedNow.Add(10 * time.Hour).Unix())
	for _, tc := range []struct {
		name      string
		code      int
		header    map[string]string
		body      string
		permanent bool
		retry     time.Duration
		hint      string
	}{
		{name: "201", code: 201},
		{name: "200", code: 200},
		{name: "202", code: 202},
		{name: "400", code: 400, permanent: true, hint: "GitLab refused the ref, variables or inputs"},
		{name: "401", code: 401, permanent: true, hint: "the trigger token is not accepted or lacks access"},
		{name: "403", code: 403, permanent: true, hint: "the trigger token is not accepted or lacks access"},
		{name: "404", code: 404, permanent: true, hint: "project not found or token without access"},
		{name: "422", code: 422, permanent: true},
		{name: "409", code: 409, permanent: true},
		{name: "408", code: 408},
		{name: "429", code: 429},
		{name: "429 Retry-After", code: 429, header: map[string]string{"Retry-After": "30"}, retry: 30 * time.Second},
		{name: "429 RateLimit-Reset", code: 429, header: map[string]string{"RateLimit-Reset": reset}, retry: 90 * time.Second},
		{name: "429 the larger wins", code: 429, header: map[string]string{"Retry-After": "120", "RateLimit-Reset": reset}, retry: 120 * time.Second},
		{name: "429 reset is capped", code: 429, header: map[string]string{"RateLimit-Reset": farReset}, retry: time.Hour},
		{name: "429 reset in the past", code: 429, header: map[string]string{"RateLimit-Reset": "1"}},
		{name: "429 reset junk", code: 429, header: map[string]string{"RateLimit-Reset": "soon"}},
		{name: "500", code: 500},
		{name: "502 Retry-After", code: 502, header: map[string]string{"Retry-After": "5"}, retry: 5 * time.Second},
		{name: "503 reset", code: 503, header: map[string]string{"RateLimit-Reset": reset}, retry: 90 * time.Second},
		{name: "301", code: 301, header: map[string]string{"Location": "https://elsewhere.example/"}, permanent: true},
		{name: "307", code: 307, header: map[string]string{"Location": "/x"}, permanent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := gitlab(t, func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.code)
			})
			res, err := newSink(t, srv).Deliver(t.Context(), request(t, cfgOf(srv.URL, ""), testEnvelope()))
			if res.StatusCode != tc.code {
				t.Errorf("StatusCode = %d", res.StatusCode)
			}
			if tc.code < 300 {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("no error")
			}
			if sink.IsPermanent(err) != tc.permanent {
				t.Errorf("permanent = %v, err %v", sink.IsPermanent(err), err)
			}
			if got := sink.RetryAfter(err); got != tc.retry {
				t.Errorf("RetryAfter = %v, want %v", got, tc.retry)
			}
			var se *sink.StatusError
			if !errors.As(err, &se) || se.Code != tc.code {
				t.Errorf("not a StatusError with code %d: %v", tc.code, err)
			}
			if tc.hint != "" && !strings.Contains(err.Error(), "("+tc.hint+")") {
				t.Errorf("error %q lacks hint %q", err, tc.hint)
			}
			if !strings.HasPrefix(err.Error(), "gitlab-pipeline-trigger: "+srv.URL+": HTTP ") {
				t.Errorf("error %q", err)
			}
		})
	}
}

// The 400 for restricted pipeline variables gets its own hint; the body is
// searched, never returned.
func TestPipelineVariablesRefusal(t *testing.T) {
	const secretish = "details-that-must-not-leak-12345"
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"refusal":       {`{"message":{"base":["Insufficient permissions to set pipeline variables"]},"x":"` + secretish + `"}`, "use inputs"},
		"other 400":     {`{"message":"ref is invalid ` + secretish + `"}`, "GitLab refused the ref, variables or inputs"},
		"past the peek": {strings.Repeat(" ", 5000) + "Insufficient permissions to set pipeline variables", "GitLab refused the ref, variables or inputs"},
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := gitlab(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := newSink(t, srv).Deliver(t.Context(), request(t, cfgOf(srv.URL, ""), testEnvelope()))
			if err == nil || !sink.IsPermanent(err) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), secretish) || strings.Contains(err.Error(), "message") {
				t.Errorf("the body leaks into the error: %v", err)
			}
		})
	}
}

// A 3xx is permanent and not followed.
func TestRedirectNotFollowed(t *testing.T) {
	other, otherGot := gitlab(t, nil)
	srv, _ := gitlab(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusTemporaryRedirect)
	})
	_, err := newSink(t, srv).Deliver(t.Context(), request(t, cfgOf(srv.URL, ""), testEnvelope()))
	if err == nil || !sink.IsPermanent(err) || !strings.Contains(err.Error(), "redirect not followed") {
		t.Fatalf("err = %v", err)
	}
	if otherGot.count() != 0 {
		t.Error("the redirect target was contacted")
	}
}

// A template that fails to render is permanent, and nothing is sent.
func TestTemplateFailuresArePermanent(t *testing.T) {
	srv, got := gitlab(t, nil)
	for name, extra := range map[string]string{
		"variable missing key": `,"variables":{"A":"{{.data.pakage}}"}`,
		"input missing key":    `,"inputs":{"a":"{{.data.pakage}}"}`,
		"null guard":           `,"variables":{"A":"{{.data.notes.body}}"}`,
		"syntax":               `,"inputs":{"a":"{{"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newSink(t, srv).Deliver(t.Context(), request(t, cfgOf(srv.URL, extra), testEnvelope()))
			if err == nil || !sink.IsPermanent(err) {
				t.Fatalf("err = %v", err)
			}
			if strings.Contains(err.Error(), token) {
				t.Errorf("error holds the token: %v", err)
			}
		})
	}
	if got.count() != 0 {
		t.Errorf("%d requests were sent", got.count())
	}
	t.Run("context done", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := newSink(t, srv).Deliver(ctx, request(t, cfgOf(srv.URL, `,"inputs":{"a":"{{.data.version}}"}`), testEnvelope()))
		if err == nil || sink.IsPermanent(err) {
			t.Fatalf("err = %v, want a retried error", err)
		}
	})
}

// D73: the token is read only from Request.Secrets, trimmed, and refused
// (retried) when missing, empty, or holding white space or control characters.
func TestTokenErrors(t *testing.T) {
	srv, got := gitlab(t, nil)
	cfg := cfgOf(srv.URL, "")
	for name, mod := range map[string]func(*sink.Request){
		"unresolved": func(r *sink.Request) { r.Secrets = nil },
		"empty":      func(r *sink.Request) { r.Secrets[TokenPointer] = secrets.NewValueString("  \n") },
		"space":      func(r *sink.Request) { r.Secrets[TokenPointer] = secrets.NewValueString("glptt-a b") },
		"newline":    func(r *sink.Request) { r.Secrets[TokenPointer] = secrets.NewValueString("glptt-secretpart\nX-Evil: 1") },
		"non-ascii":  func(r *sink.Request) { r.Secrets[TokenPointer] = secrets.NewValueString("glptt-é") },
		"control":    func(r *sink.Request) { r.Secrets[TokenPointer] = secrets.NewValueString("glptt-\x07") },
	} {
		t.Run(name, func(t *testing.T) {
			req := request(t, cfg, testEnvelope())
			mod(&req)
			_, err := newSink(t, srv).Deliver(t.Context(), req)
			if err == nil || sink.IsPermanent(err) {
				t.Fatalf("err = %v, want a retried error", err)
			}
			if !strings.Contains(err.Error(), "token_env GL_TOKEN") || strings.Contains(err.Error(), "secretpart") || strings.Contains(err.Error(), "glptt") {
				t.Errorf("error %q", err)
			}
		})
	}
	if got.count() != 0 {
		t.Error("a request was sent without a valid token")
	}
}

// D72: http:// is delivered only for a host in http.allow_http_hosts, checked
// at every delivery; https is always allowed.
func TestHTTPBaseURL(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) }))
	t.Cleanup(plain.Close)
	cfg := cfgOf(plain.URL, "")
	hc := guarded(t, plain, true)

	_, err := New(hc, Options{}).Deliver(t.Context(), request(t, cfg, testEnvelope()))
	if err == nil || !sink.IsPermanent(err) || !strings.Contains(err.Error(), "http:// is not allowed for this host") || strings.Contains(err.Error(), token) {
		t.Fatalf("not allowed: err = %v", err)
	}
	_, err = New(hc, Options{AllowHTTPHosts: []string{"other.example"}}).Deliver(t.Context(), request(t, cfg, testEnvelope()))
	if err == nil || !sink.IsPermanent(err) {
		t.Fatalf("other host: err = %v", err)
	}
	res, err := New(hc, Options{AllowHTTPHosts: []string{"127.0.0.1"}}).Deliver(t.Context(), request(t, cfg, testEnvelope()))
	if err != nil || res.StatusCode != http.StatusCreated {
		t.Fatalf("allowed: %+v, %v", res, err)
	}
}

// Transport errors are retried with a fixed description; an SSRF refusal is
// permanent and names the host only.
func TestTransportErrors(t *testing.T) {
	srv, _ := gitlab(t, nil)
	t.Run("blocked", func(t *testing.T) {
		s := New(guarded(t, srv, false), Options{})
		_, err := s.Deliver(t.Context(), request(t, cfgOf(srv.URL, ""), testEnvelope()))
		if err == nil || !sink.IsPermanent(err) || !errors.Is(err, netguard.ErrBlocked) || err.Error() != "gitlab-pipeline-trigger: destination not allowed (127.0.0.1)" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("refused", func(t *testing.T) {
		dead := httptest.NewTLSServer(nil)
		cfg := cfgOf(dead.URL, "")
		hc := guarded(t, dead, true)
		dead.Close()
		_, err := New(hc, Options{}).Deliver(t.Context(), request(t, cfg, testEnvelope()))
		if err == nil || sink.IsPermanent(err) || err.Error() != "gitlab-pipeline-trigger: "+dead.URL+": connection refused" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		// The handler only returns at cleanup, so the client's timeout is the
		// sole way out.
		stop := make(chan struct{})
		slow, _ := gitlab(t, func(http.ResponseWriter, *http.Request) { <-stop })
		t.Cleanup(func() { close(stop) })
		_, err := newSink(t, slow).Deliver(t.Context(), request(t, cfgOf(slow.URL, `,"timeout":"1s"`), testEnvelope()))
		if err == nil || sink.IsPermanent(err) || !strings.HasSuffix(err.Error(), ": timeout") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("reset", func(t *testing.T) {
		rst, _ := gitlab(t, func(w http.ResponseWriter, _ *http.Request) {
			if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = c.Close()
			}
		})
		_, err := newSink(t, rst).Deliver(t.Context(), request(t, cfgOf(rst.URL, ""), testEnvelope()))
		if err == nil || sink.IsPermanent(err) || !strings.HasSuffix(err.Error(), ": connection reset") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Nanosecond)
		defer cancel()
		<-ctx.Done()
		_, err := newSink(t, srv).Deliver(ctx, request(t, cfgOf(srv.URL, ""), testEnvelope()))
		if err == nil || sink.IsPermanent(err) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestFailureClass(t *testing.T) {
	if got := sink.TransportClass(fmt.Errorf("dial tcp 10.0.0.1:443: %w", errConnRefused)); got != "connection refused" {
		t.Errorf("TransportClass = %q", got)
	}
}

// D73: no error, log line or last_error string of any failure path holds the
// token, the project path or the response body; the server echoes both.
func TestNothingHoldsTheTokenProjectOrBody(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const echoed = "echoed-response-body-0123456789"
	var all []string
	for _, code := range []int{200, 201, 400, 401, 403, 404, 408, 422, 429, 500, 503, 301, 307} {
		srv, _ := gitlab(t, func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			w.Header().Set("X-Echo", string(b))
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"message":"` + echoed + `","token":` + string(mustJSON(token)) + `,"path":"` + r.URL.EscapedPath() + `"}`))
		})
		_, err := newSink(t, srv).Deliver(t.Context(), request(t, cfgOf(srv.URL, ""), testEnvelope()))
		if err != nil {
			all = append(all, err.Error())
		}
	}
	// Failure paths before the request.
	srv, _ := gitlab(t, nil)
	for _, mod := range []func(*sink.Request){
		func(r *sink.Request) { r.Secrets = nil },
		func(r *sink.Request) { r.Secrets[TokenPointer] = secrets.NewValueString(token + " x") },
	} {
		req := request(t, cfgOf(srv.URL, ""), testEnvelope())
		mod(&req)
		if _, err := newSink(t, srv).Deliver(t.Context(), req); err != nil {
			all = append(all, err.Error())
		}
	}
	req := request(t, cfgOf(srv.URL, `,"variables":{"A":"{{.data.pakage}}"}`), testEnvelope())
	if _, err := newSink(t, srv).Deliver(t.Context(), req); err != nil {
		all = append(all, err.Error())
	}
	_, err := New(guarded(t, srv, false), Options{}).Deliver(t.Context(), request(t, cfgOf(srv.URL, ""), testEnvelope()))
	all = append(all, err.Error())
	if err := newSink(t, srv).Validate(sink.Config(`{"project":"platform/deploys/app","ref":"main","token_env":"X","token":"` + token + `"}`)); err != nil {
		all = append(all, err.Error())
	}
	if len(all) < 12 {
		t.Fatalf("%d errors captured", len(all))
	}
	all = append(all, logs.String())
	for _, e := range all {
		for _, leak := range []string{token, "glptt", "platform/deploys", "platform%2Fdeploys", echoed, "Bearer", "GL_TOKEN="} {
			if strings.Contains(e, leak) {
				t.Errorf("%q holds %q", e, leak)
			}
		}
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestRegistryAndNew(t *testing.T) {
	srv, _ := gitlab(t, nil)
	r := sink.NewRegistry()
	if err := r.Register(newSink(t, srv)); err != nil {
		t.Fatal(err)
	}
	if s, err := r.Get("gitlab-pipeline-trigger"); err != nil || s.Kind() != Kind {
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
	srv, got := gitlab(t, nil)
	_, err := newSink(t, srv).Deliver(t.Context(), request(t, `{"project":"nope"}`, testEnvelope()))
	if err == nil || !sink.IsPermanent(err) {
		t.Fatalf("err = %v", err)
	}
	if got.count() != 0 {
		t.Error("sent")
	}
}

func TestEventFallsBackToEnvelope(t *testing.T) {
	srv, got := gitlab(t, nil)
	req := request(t, cfgOf(srv.URL, `,"inputs":{"s":"x-{{.data.source}}"}`), testEnvelope())
	req.Event.JSON = nil
	if _, err := newSink(t, srv).Deliver(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if _, body := got.last(); !strings.Contains(string(body), `"s":"x-pypi"`) {
		t.Errorf("%s", body)
	}
}
