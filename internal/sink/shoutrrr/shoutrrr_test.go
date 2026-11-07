package shoutrrr

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/render"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/sink"
)

// Synthetic credentials: tests assert none of them reaches an error.
const (
	slackHook  = "slack://hook:AAAAAAAAA-BBBBBBBBB-123456789123456789123456@webhook"            //nolint:gosec // G101: synthetic
	slackBot   = "slack://xoxb:123456789012-1234567890123-4mt0t4l1YL3g1T5L4cK70k3N@C0123456789" //nolint:gosec // G101: synthetic
	discordURL = "discord://DISCORDSECRETTOKEN@123456789"
	telegramTk = "110201543:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"
	ntfyURL    = "ntfy://ntfyuser:NTFYSECRETPASS@ntfy.example/releases" //nolint:gosec // G101: synthetic
	gotifyURL  = "gotify://gotify.example/sub/AGOTIFYSECRET01"
	matrixURL  = "matrix://:syt_MATRIXSECRETTOKEN@matrix.example?rooms=!room:matrix.example"
)

var credentials = []string{
	"tokenlikeprefix0123", "AAAAAAAAA-BBBBBBBBB-123456789123456789123456", "123456789123456789123456", "4mt0t4l1YL3g1T5L4cK70k3N", "DISCORDSECRETTOKEN",
	telegramTk, "AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw", "NTFYSECRETPASS", "AGOTIFYSECRET01", "MATRIXSECRETTOKEN", "releases",
}

func noCredential(t *testing.T, where, s string) {
	t.Helper()
	for _, c := range credentials {
		if strings.Contains(s, c) {
			t.Errorf("%s holds a credential: %q", where, s)
		}
	}
}

func value(s string) secrets.Value { return secrets.NewValueString(s) }

func origins(t *testing.T, raw string) ([]string, error) {
	t.Helper()
	us, err := (&Sink{}).SecretDestinations(URLRefPointer, value(raw))
	if err != nil {
		noCredential(t, "error", err.Error())
		return nil, err
	}
	out := make([]string, len(us))
	for i, u := range us {
		out[i] = u.String()
	}
	return out, nil
}

// ADR 0026 §3: each allowed service's origins, derived without I/O from
// the URLs the library documents; refused schemes and options.
//
// Deriving initializes the library's service before any check: the failing
// http.DefaultTransport proves that none of them sends a request then
// (upstream matrix once logged in during Initialize), so a library upgrade
// that does fails here. The user-form matrix URL is the one that would.
func TestSecretDestinations(t *testing.T) {
	noEscape(t)
	for raw, want := range map[string]string{ //nolint:gosec // G101: synthetic URLs
		slackHook:  "https://hooks.slack.com",
		slackBot:   "https://slack.com",
		discordURL: "https://discord.com",
		"telegram://" + telegramTk + "@telegram?chats=@releases":                             "https://api.telegram.org",
		"telegram://" + telegramTk + "@telegram?chats=1&apiurl=https://tg.example:8443/base": "https://tg.example:8443",
		"  " + ntfyURL + "\n": "https://ntfy.example",
		"ntfy:///releases":    "https://ntfy.sh",
		"ntfy://ntfy.example:8080/releases?disabletls=yes": "http://ntfy.example:8080",
		"ntfy://ntfy.example/releases?scheme=http":         "http://ntfy.example",
		gotifyURL: "https://gotify.example",
		"gotify://GOTIFY.example:8443/AGOTIFYSECRET01?disabletls=yes": "http://gotify.example:8443",
		matrixURL:                      "https://matrix.example",
		"NTFY://ntfy.example/releases": "https://ntfy.example",
	} {
		got, err := origins(t, raw)
		if err != nil || strings.Join(got, " ") != want {
			t.Errorf("%s: %v, %v; want %s", strings.SplitN(raw, ":", 2)[0], got, err, want)
		}
	}
	for raw, want := range map[string]string{ //nolint:gosec // G101: synthetic URLs
		"generic+https://hooks.example/x":                  `service "generic" is not allowed`,
		"smtp://user:pass@mail.example:25/?to=a@b.example": `service "smtp" is not allowed`,
		"not a url":                    "not a shoutrrr service URL",
		"%zz":                          "not a shoutrrr service URL",
		"tokenlikeprefix0123:rest":     "service with this scheme is not allowed",
		"pushover://shoutrrr:tok@user": `service "pushover" is not allowed`,
		"slack://hook:short@webhook":   "slack: the service URL is invalid",
		"ntfy://ntfy.example/releases?skiptlsverify=yes":                   "ntfy: skiptlsverify is not supported",
		"ntfy://ntfy.example/releases?disabletlsverification=yes":          "ntfy: skiptlsverify is not supported",
		"gotify://gotify.example/AGOTIFYSECRET01?insecureskipverify=yes":   "gotify: skiptlsverify is not supported",
		"matrix://:t@matrix.example?skiptlsverify=yes":                     "matrix: skiptlsverify",
		"telegram://" + telegramTk + "@telegram?chats=1&skiptlsverify=yes": "telegram: skiptlsverify",
		"matrix://bot:MATRIXSECRETTOKEN@matrix.example":                    "only the access-token form",
	} {
		_, err := origins(t, raw)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%.20s: %v, want %q", raw, err, want)
		}
	}
	if _, err := (&Sink{}).SecretDestinations("/other_ref", value(ntfyURL)); err == nil {
		t.Error("other pointer: no error")
	}
}

// failingTransport replaces http.DefaultTransport during the injection
// tests: a request that escapes the injected client fails the test.
type failingTransport struct{ t *testing.T }

func (f failingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.t.Errorf("request escaped the injected client: %s %s", r.Method, r.URL.Host)
	return nil, errors.New("escaped")
}

// rewriter sends every request to srv, as a resolver pointing every host at
// it would, and records what the library asked for.
type rewriter struct {
	srv  *httptest.Server
	next http.RoundTripper
	mu   sync.Mutex
	seen []string // "METHOD scheme://host/path"
}

func (r *rewriter) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.seen = append(r.seen, req.Method+" "+req.URL.Scheme+"://"+req.URL.Host+req.URL.Path)
	r.mu.Unlock()
	u, _ := url.Parse(r.srv.URL)
	out := req.Clone(req.Context())
	out.URL.Scheme, out.URL.Host = u.Scheme, u.Host
	return r.next.RoundTrip(out)
}

func noEscape(t *testing.T) {
	prev := http.DefaultTransport
	http.DefaultTransport = failingTransport{t}
	t.Cleanup(func() { http.DefaultTransport = prev })
}

// received is one request the fake service got.
type received struct{ method, path, body string }

// fakeService answers like the real services do for a successful send.
func fakeService(t *testing.T, got *[]received, mu *sync.Mutex) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		*got = append(*got, received{r.Method, r.URL.Path, string(b)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/services/"): // slack webhook
			_, _ = io.WriteString(w, "ok")
		case r.URL.Path == "/api/chat.postMessage":
			_, _ = io.WriteString(w, `{"ok":true}`)
		case strings.HasPrefix(r.URL.Path, "/api/v10/webhooks/"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1,"date":0,"chat":{"id":1,"type":"channel"}}}`)
		case strings.Contains(r.URL.Path, "/join/"):
			_, _ = io.WriteString(w, `{"room_id":"!room:matrix.example"}`)
		case strings.Contains(r.URL.Path, "/send/m.room.message/"):
			_, _ = io.WriteString(w, `{"event_id":"$e"}`)
		case strings.HasSuffix(r.URL.Path, "/message"): // gotify
			_, _ = io.WriteString(w, `{"id":1,"appid":1,"message":"m","title":"t","priority":0,"date":"2026-11-07T00:00:00Z"}`)
		default: // ntfy
			_, _ = io.WriteString(w, `{"id":"x","time":1,"event":"message","topic":"releases"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func request(t *testing.T, cfg, raw string, dests []string, templated bool) sink.Request {
	t.Helper()
	ev := sampleEvent(t, event.TypePublished)
	req := sink.Request{
		Config: sink.Config(cfg), Secrets: map[string]secrets.Value{URLRefPointer: value(raw)}, Destinations: dests,
		Event: ev, Rendered: sink.Rendered{Body: []byte("custom message"), ContentType: render.ContentTypeText, Templated: templated},
	}
	return req
}

func sampleEvent(t *testing.T, typ string) sink.ReleaseEvent {
	t.Helper()
	env := render.Samples()[0].Envelope
	env.Type = typ
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return sink.ReleaseEvent{JSON: b, Envelope: env}
}

// ADR 0026 §4: for every allowed service the library sends through the
// injected, pinned client (http.DefaultTransport fails the test), to the
// derived origin, with the rendered message in the body; a URL whose
// derived origin is not where the library sends is refused by the pin,
// permanently, and nothing reaches the server.
func TestInjectedClientIsUsed(t *testing.T) {
	noEscape(t)
	for _, tc := range []struct {
		name, url, origin string
		requests          []string // METHOD and path prefix the library sends
	}{
		{"slack webhook", slackHook, "https://hooks.slack.com", []string{"POST /services/AAAAAAAAA/BBBBBBBBB/"}},
		{"slack api", slackBot, "https://slack.com", []string{"POST /api/chat.postMessage"}},
		{"discord", discordURL, "https://discord.com", []string{"POST /api/v10/webhooks/123456789/"}},
		{"telegram", "telegram://" + telegramTk + "@telegram?chats=@releases", "https://api.telegram.org", []string{"POST /bot"}},
		{"ntfy", ntfyURL, "https://ntfy.example", []string{"POST /releases"}},
		{"gotify", gotifyURL, "https://gotify.example", []string{"POST /sub/message"}},
		{"matrix", matrixURL, "https://matrix.example", []string{"PUT /_matrix/client/v3/rooms/"}},
		{"matrix alias", "matrix://:syt_MATRIXSECRETTOKEN@matrix.example?rooms=%23releases:matrix.example", "https://matrix.example", []string{"POST /_matrix/client/v3/join/", "PUT /_matrix/client/v3/rooms/"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu  sync.Mutex
				got []received
			)
			rw := &rewriter{srv: fakeService(t, &got, &mu), next: &http.Transport{}}
			s := New(&http.Client{Transport: rw}, Options{})
			u, _ := url.Parse(tc.origin)
			hp, _ := secrets.Destination(u)
			res, err := s.Deliver(t.Context(), request(t, `{"url_ref":"chat"}`, tc.url, []string{hp}, true))
			if err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			if res.StatusCode < 200 || res.StatusCode > 299 {
				t.Errorf("status %d", res.StatusCode)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(got) != len(tc.requests) {
				t.Fatalf("server got %d requests, want %d: %v", len(got), len(tc.requests), rw.seen)
			}
			for i, want := range tc.requests {
				method, path, _ := strings.Cut(want, " ")
				if got[i].method != method || !strings.HasPrefix(got[i].path, path) {
					t.Errorf("request %d: %s %s, want %s", i, got[i].method, got[i].path, want)
				}
				if !strings.HasPrefix(rw.seen[i], method+" "+tc.origin+"/") {
					t.Errorf("request %d went to %s, want origin %s", i, rw.seen[i], tc.origin)
				}
			}
			if last := got[len(got)-1]; !strings.Contains(last.body, "custom message") {
				t.Errorf("message not in the body: %q", last.body)
			}

			// The pin: a destination list without this origin refuses the
			// request unsent (as if the derivation drifted from the library).
			got = nil
			mu.Unlock()
			_, err = s.Deliver(t.Context(), request(t, `{"url_ref":"chat"}`, tc.url, []string{hp, "elsewhere.example:443"}, true))
			mu.Lock()
			if err != nil || len(got) == 0 {
				t.Fatalf("with an extra destination: %v, %d requests", err, len(got))
			}
			got = nil
			mu.Unlock()
			res, err = s.Deliver(t.Context(), request(t, `{"url_ref":"chat"}`, tc.url, []string{"elsewhere.example:443"}, true))
			mu.Lock()
			if err == nil || len(got) != 0 {
				t.Fatalf("unchecked destination: %v, %d requests", err, len(got))
			}
			if sink.IsPermanent(err) || !strings.Contains(err.Error(), "destination not checked") {
				t.Errorf("unchecked destination: %v", err)
			}
			noCredential(t, "error", err.Error())
			_ = res
		})
	}
}

// The pin itself: a request outside the allowed origins is refused unsent,
// and Deliver reports it permanently.
func TestPinRefusesOtherOrigins(t *testing.T) {
	noEscape(t)
	var sent int
	next := roundTripFunc(func(*http.Request) (*http.Response, error) {
		sent++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
	})
	c, p := pinnedClient(&http.Client{Transport: next}, []string{"https://hooks.slack.com:443"}, time.Now)
	for _, raw := range []string{"https://evil.example/x", "http://hooks.slack.com:443/x", "https://hooks.slack.com:8443/x"} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, raw, strings.NewReader("x"))
		resp, err := c.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, errUnpinned) {
			t.Errorf("%s: %v", raw, err)
		}
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://HOOKS.slack.com/services/x", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if n, refused, _ := p.seen(); sent != 1 || n != 1 || refused != 3 {
		t.Errorf("sent %d, requests %d, refused %d", sent, n, refused)
	}
	res, err := outcome("slack", "https://hooks.slack.com", p, errors.New("library error with https://hooks.slack.com/services/SECRET"), nil)
	if !sink.IsPermanent(err) || strings.Contains(err.Error(), "SECRET") || res.StatusCode != 200 {
		t.Errorf("outcome: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// ADR 0026 §5: outcomes come from what the pinned client saw. 3xx and 4xx
// are permanent, 408, 429 (with Retry-After) and 5xx retried, a transport
// error retried with a fixed class, telegram's ok:false after a 200 is a
// retried service error. Errors name the service and the origin, never the
// URL, a token or the response body.
func TestOutcomes(t *testing.T) {
	noEscape(t)
	const body = `{"ok":false,"description":"echo of NTFYSECRETPASS and AGOTIFYSECRET01"}`
	for _, tc := range []struct {
		name      string
		url       string
		origin    string
		handler   http.HandlerFunc
		permanent bool
		want      string
		after     time.Duration
	}{
		{"500", ntfyURL, "https://ntfy.example", status(500, ""), false, "shoutrrr: ntfy https://ntfy.example: HTTP 500 Internal Server Error", 0},
		{"404", gotifyURL, "https://gotify.example", status(404, body), true, "shoutrrr: gotify https://gotify.example: HTTP 404 Not Found", 0},
		{"401", slackBot, "https://slack.com", status(401, body), true, "shoutrrr: slack https://slack.com: HTTP 401 Unauthorized", 0},
		{"429", discordURL, "https://discord.com", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
		}, false, "shoutrrr: discord https://discord.com: HTTP 429 Too Many Requests", 120 * time.Second},
		{"redirect", slackHook, "https://hooks.slack.com", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://elsewhere.example/", http.StatusFound)
		}, true, "redirect not followed", 0},
		// The library returns nil for these: slack's webhook on a body of
		// "ok" whatever the status, gotify on any status below 400. The
		// status the pin saw decides.
		{"slack 302 ok", slackHook, "https://hooks.slack.com", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://elsewhere.example/")
			w.WriteHeader(http.StatusFound)
			_, _ = io.WriteString(w, "ok")
		}, true, "shoutrrr: slack https://hooks.slack.com: HTTP 302 Found: redirect not followed", 0},
		{"slack 503 ok", slackHook, "https://hooks.slack.com", status(503, "ok"), false, "shoutrrr: slack https://hooks.slack.com: HTTP 503 Service Unavailable", 0},
		{"gotify 302", gotifyURL, "https://gotify.example", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://elsewhere.example/")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusFound)
			_, _ = io.WriteString(w, `{"id":1,"appid":1,"message":"m","title":"t","priority":0,"date":"2026-11-07T00:00:00Z"}`)
		}, true, "shoutrrr: gotify https://gotify.example: HTTP 302 Found: redirect not followed", 0},
		{
			"gotify 500", gotifyURL, "https://gotify.example", status(500, `{"error":"Internal Server Error","errorCode":500,"errorDescription":"x"}`), false,
			"shoutrrr: gotify https://gotify.example: HTTP 500 Internal Server Error", 0,
		},
		{
			"telegram ok false", "telegram://" + telegramTk + "@telegram?chats=@releases", "https://api.telegram.org", status(200, body), false,
			"shoutrrr: telegram https://api.telegram.org: service error", 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			s := New(&http.Client{Transport: &rewriter{srv: srv, next: &http.Transport{}}}, Options{})
			u, _ := url.Parse(tc.origin)
			hp, _ := secrets.Destination(u)
			_, err := s.Deliver(t.Context(), request(t, `{"url_ref":"chat","timeout":"5s"}`, tc.url, []string{hp}, true))
			if err == nil || sink.IsPermanent(err) != tc.permanent || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Deliver: %v (permanent %v), want %q permanent=%v", err, sink.IsPermanent(err), tc.want, tc.permanent)
			}
			if got := sink.RetryAfter(err); got != tc.after {
				t.Errorf("Retry-After %v, want %v", got, tc.after)
			}
			noCredential(t, "error", err.Error())
			if strings.Contains(err.Error(), "echo of") {
				t.Errorf("error holds the response body: %v", err)
			}
		})
	}
	t.Run("transport error", func(t *testing.T) {
		srv := httptest.NewServer(status(200, ""))
		srv.Close() // connection refused
		s := New(&http.Client{Transport: &rewriter{srv: srv, next: &http.Transport{}}}, Options{})
		_, err := s.Deliver(t.Context(), request(t, `{"url_ref":"chat"}`, ntfyURL, []string{"ntfy.example:443"}, true))
		if err == nil || sink.IsPermanent(err) || err.Error() != "shoutrrr: ntfy https://ntfy.example: connection refused" {
			t.Fatalf("Deliver: %v", err)
		}
	})
}

func status(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

// Failures before the send: an invalid configuration is permanent; an
// unresolved or invalid URL and a service mismatch are retried (the fix is
// the secret); nothing is sent.
func TestDeliverRefusals(t *testing.T) {
	noEscape(t)
	s := New(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("sent %s", r.URL.Host)
		return nil, errors.New("no")
	})}, Options{})
	for _, tc := range []struct {
		name, cfg, url string
		permanent      bool
		want           string
	}{
		{"bad config", `{"url":"x"}`, ntfyURL, true, "invalid configuration"},
		{"both", `{"url_ref":"a","url_env":"B"}`, ntfyURL, true, "exactly one of url_ref and url_env"},
		{"not resolved", `{"url_env":"CHAT"}`, ntfyURL, false, "url_env CHAT: not resolved"},
		{"invalid url", `{"url_ref":"chat"}`, "smtp://x", false, `shoutrrr: url_ref chat: shoutrrr: service "smtp" is not allowed`},
		{"service mismatch", `{"url_ref":"chat","service":"slack"}`, ntfyURL, false, "the URL is for ntfy, but the sink's service is slack"},
		{"not checked", `{"url_ref":"chat"}`, ntfyURL, false, "destination not checked by the dispatcher"},
	} {
		dests := []string{}
		_, err := s.Deliver(t.Context(), request(t, tc.cfg, tc.url, dests, true))
		if err == nil || sink.IsPermanent(err) != tc.permanent || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q permanent=%v", tc.name, err, tc.want, tc.permanent)
			continue
		}
		noCredential(t, tc.name, err.Error())
	}
}

func TestParseConfig(t *testing.T) {
	for raw, ok := range map[string]bool{
		`{"url_ref":"chat"}`: true,
		`{"url_env":"CHAT_URL","service":"ntfy","timeout":"1s"}`: true,
		`{"url_ref":"chat","timeout":"2m"}`:                      true,
		`{}`:                                                     false,
		`{"url_ref":"Chat"}`:                                     false,
		`{"url_env":"1X"}`:                                       false,
		`{"url_ref":"chat","service":"smtp"}`:                    false,
		`{"url_ref":"chat","timeout":"3m"}`:                      false,
		`{"url_ref":"chat","timeout":"500ms"}`:                   false,
		`{"url_ref":"chat","url":"slack://x"}`:                   false,
		`[]`:                                                     false,
	} {
		if err := (&Sink{}).Validate(sink.Config(raw)); (err == nil) != ok {
			t.Errorf("%s: %v", raw, err)
		}
	}
	p, _ := ParseConfig(sink.Config(`{"url_env":"CHAT_URL"}`))
	if ref, ptr := p.Ref(); ref != "url_env CHAT_URL" || ptr != URLEnvPointer || p.Timeout != DefaultTimeout {
		t.Errorf("Ref %s %s %v", ref, ptr, p.Timeout)
	}
}

// #120 criterion 2: without a subscription template, a plain-text message
// per event type, rendered by the template engine, on the full and the
// sparse sample events; with a template, its rendered body.
func TestDefaultMessages(t *testing.T) {
	for _, sample := range render.Samples() {
		for typ, want := range map[string]string{
			event.TypePublished:            sample.Envelope.Data.Package + " " + sample.Envelope.Data.Version + " released",
			event.TypePrerelease:           " pre-release published",
			event.TypeYanked:               " was yanked upstream",
			"dev.angelia.release.later.v1": ": dev.angelia.release.later.v1",
		} {
			env := sample.Envelope
			env.Type = typ
			b, _ := json.Marshal(env)
			msg, err := message(context.Background(), sink.Request{Event: sink.ReleaseEvent{JSON: b, Envelope: env}})
			if err != nil || !strings.Contains(msg, want) || strings.Contains(msg, "<no value>") {
				t.Errorf("%s %s: %q, %v", sample.Name, typ, msg, err)
			}
			if typ == event.TypePublished && sample.Name == "full" && (!strings.Contains(msg, "("+*env.Data.Bump+")") || !strings.Contains(msg, *env.Data.URLs.Release)) {
				t.Errorf("full published: %q", msg)
			}
		}
	}
	msg, err := message(t.Context(), sink.Request{Rendered: sink.Rendered{Body: []byte("mine"), Templated: true}})
	if err != nil || msg != "mine" {
		t.Errorf("templated: %q %v", msg, err)
	}
	if _, err := message(t.Context(), sink.Request{Rendered: sink.Rendered{Templated: true}}); err == nil {
		t.Error("empty template output: no error")
	}
}

// ADR 0026 §8: only the six service packages (and what they import) are
// linked, never the router or the root package, which bring every service.
func TestOnlyAllowedShoutrrrPackages(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), "go", "list", "-deps", ".").Output()
	if err != nil {
		t.Skipf("go list: %v", err)
	}
	const mod = "github.com/nicholas-fedor/shoutrrr"
	allowed := []string{"chat/slack", "chat/discord", "chat/telegram", "push/ntfy", "push/gotify", "chat/matrix", "standard"}
	n := 0
	for pkg := range strings.Lines(string(out)) {
		pkg = strings.TrimSpace(pkg)
		if !strings.HasPrefix(pkg, mod) {
			continue
		}
		n++
		rest := strings.TrimPrefix(pkg, mod)
		switch {
		case rest == "", strings.HasPrefix(rest, "/pkg/router"), rest == "/shoutrrr":
			t.Errorf("links %s", pkg)
		case strings.HasPrefix(rest, "/pkg/services/") && !slices.Contains(allowed, strings.TrimPrefix(rest, "/pkg/services/")):
			t.Errorf("links service %s", pkg)
		}
	}
	if n == 0 {
		t.Error("no shoutrrr package listed")
	}
}

// New fails closed without a transport: http.Client would otherwise send
// through http.DefaultTransport, around netguard.
func TestNewNeedsATransport(t *testing.T) {
	for name, c := range map[string]*http.Client{"nil client": nil, "nil transport": {}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			New(c, Options{})
		}()
	}
}

// ADR 0026 §4: when the derivation drifts from where the library really
// sends, the pin refuses the library's request unsent and the attempt is
// permanent. The ntfy derivation is replaced by one that predicts another
// host; the core checked only that host.
func TestPinRefusesDriftedDerivation(t *testing.T) {
	noEscape(t)
	var (
		mu  sync.Mutex
		got []received
	)
	rw := &rewriter{srv: fakeService(t, &got, &mu), next: &http.Transport{}}
	s := New(&http.Client{Transport: rw}, Options{})
	ntfySvc := s.services["ntfy"]
	ntfySvc.origins = func(sender) ([]*url.URL, error) { return fixed("https://drifted.example"), nil }
	s.services["ntfy"] = ntfySvc

	res, err := s.Deliver(t.Context(), request(t, `{"url_ref":"chat"}`, ntfyURL, []string{"drifted.example:443"}, true))
	if err == nil || !sink.IsPermanent(err) || !errors.Is(err, errUnpinned) {
		t.Fatalf("Deliver: %v (permanent %v)", err, sink.IsPermanent(err))
	}
	if !strings.Contains(err.Error(), "shoutrrr: ntfy https://drifted.example: shoutrrr: request to an unchecked destination refused") {
		t.Errorf("error %q", err)
	}
	noCredential(t, "error", err.Error())
	mu.Lock()
	defer mu.Unlock()
	if len(rw.seen) != 0 || len(got) != 0 || res.StatusCode != 0 {
		t.Errorf("the library's request was sent: %v", rw.seen)
	}
	// The default table is untouched.
	if o, err := (&Sink{}).SecretDestinations(URLRefPointer, value(ntfyURL)); err != nil || o[0].Host != "ntfy.example" {
		t.Errorf("default table changed: %v %v", o, err)
	}
}
