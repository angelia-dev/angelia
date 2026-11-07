package dispatch

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/sink"
	"github.com/angelia-dev/angelia/internal/sink/shoutrrr"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/telemetry"
	"github.com/angelia-dev/angelia/internal/telemetry/telemetrytest"
)

type staticResolver map[string][]netip.Addr

func (r staticResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := r[host]; ok {
		return a, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// toServer sends every request to srv over its TLS client: a mocked Slack
// endpoint and a local ntfy server on one test server.
type toServer struct {
	srv *httptest.Server
}

func (s toServer) RoundTrip(r *http.Request) (*http.Response, error) {
	u, _ := url.Parse(s.srv.URL)
	out := r.Clone(r.Context())
	out.URL.Host = u.Host
	return s.srv.Client().Transport.RoundTrip(out)
}

type textRenderer struct{}

func (textRenderer) Render(context.Context, string, []byte) (sink.Rendered, error) {
	return sink.Rendered{Body: []byte("requests 2.33.0 released"), ContentType: "text/plain; charset=utf-8", Templated: true}, nil
}

type recordingShoutrrr struct {
	*shoutrrr.Sink
	mu   sync.Mutex
	errs []string
}

func (r *recordingShoutrrr) Deliver(ctx context.Context, req sink.Request) (sink.Result, error) {
	res, err := r.Sink.Deliver(ctx, req)
	if err != nil {
		r.mu.Lock()
		r.errs = append(r.errs, err.Error())
		r.mu.Unlock()
	}
	return res, err
}

// #120 criteria 1 and 3, end to end: a dispatcher with a debug JSON log and
// recorded spans delivers through the real netguard client and shoutrrr
// sink to a mocked Slack webhook and a local ntfy server. The URL comes from
// a stored secret (url_ref) or a variable (url_env). Delivered, a 500
// (retried), a 404 (dead), a secret bound to another host, an SSRF refusal,
// plain http without allow_http_hosts and a refused scheme (all retried
// before sending). The URL and its tokens never appear in the logs, the
// spans, delivery.last_error or the sink's errors.
func TestShoutrrrDeliveriesNeverLogTheURL(t *testing.T) {
	const (
		slackToken = "AAAAAAAAA-BBBBBBBBB-SLACKSECRET1234567890123"
		ntfyPass   = "NTFYSECRETPASS0123"
	)
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		paths = append(paths, r.Host+r.URL.Path)
		mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/services/"):
			_, _ = io.WriteString(w, "ok")
		case r.URL.Path == "/fail":
			w.WriteHeader(http.StatusInternalServerError)
		case r.URL.Path == "/gone":
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/echo":
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, r.Header.Get("Authorization")) //nolint:gosec // G705: a test server echoing a header on purpose
		default:
			_, _ = io.WriteString(w, `{"id":"x","time":1,"event":"message","topic":"releases"}`)
		}
	}))
	defer srv.Close()
	local := srv.Listener.Addr().String() // 127.0.0.1:port
	ntfy := func(path string) string { return "ntfy://releases:" + ntfyPass + "@" + local + path }
	slackURL := "slack://hook:" + slackToken + "@webhook"

	type scenario struct {
		name, url string
		ref       bool
		hosts     []string
		want      string
		lastError string
	}
	scenarios := []scenario{
		{"slack env", slackURL, false, nil, store.DeliveryDelivered, ""},
		{"slack ref bound", slackURL, true, []string{"hooks.slack.com"}, store.DeliveryDelivered, ""},
		{"ntfy ref bound", ntfy("/releases"), true, []string{local}, store.DeliveryDelivered, ""},
		{"ntfy 500", ntfy("/fail"), true, nil, store.DeliveryPending, "shoutrrr: ntfy https://" + local + ": HTTP 500 Internal Server Error"},
		{"ntfy 404", ntfy("/gone"), false, nil, store.DeliveryDead, "shoutrrr: ntfy https://" + local + ": HTTP 404 Not Found"},
		{"ntfy echo", ntfy("/echo"), true, nil, store.DeliveryPending, "HTTP 502 Bad Gateway"},
		{
			"host binding", ntfy("/releases"), true,
			[]string{"ntfy.sh"},
			store.DeliveryPending,
			"shoutrrr: secret /url_ref (secret chat-url): https://" + local + ": secrets: host not allowed: secret chat-url is bound to ntfy.sh; refused for " + local,
		},
		{
			"ssrf", "ntfy://releases:" + ntfyPass + "@10.0.0.7/releases", false, nil, store.DeliveryPending,
			"(environment variable CHAT_URL): https://10.0.0.7: destination not allowed by the SSRF guard",
		},
		{"plain http", ntfy("/releases?disabletls=yes"), true, nil, store.DeliveryPending, "http:// is not allowed for this host (http.allow_http_hosts)"},
		{
			"refused scheme", "generic+https://" + local + "/x?token=" + ntfyPass, true, nil, store.DeliveryPending,
			`shoutrrr: secret /url_ref (secret chat-url): service "generic" is not allowed`,
		},
	}
	stored := newSecretStore(t)
	logs := &syncBuffer{}
	log := slog.New(telemetry.NewLogHandler(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	rec, tp := telemetrytest.Install(t)
	var lastErrors []string
	var sinks []*recordingShoutrrr
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			nc, err := netguard.New(netguard.Config{
				Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, Logger: log, MaxAttempts: 1,
				Resolver:  staticResolver{"hooks.slack.com": {netip.MustParseAddr("192.0.2.10")}},
				Transport: toServer{srv}, Proxy: func(*http.Request) (*url.URL, error) { return nil, nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			rs := &recordingShoutrrr{Sink: shoutrrr.New(nc.HTTPClient(), shoutrrr.Options{})}
			sinks = append(sinks, rs)
			cfg := `{"url_env":"CHAT_URL"}`
			env := map[string]string{"CHAT_URL": sc.url}
			if sc.ref {
				cfg = `{"url_ref":"chat-url"}`
				stored.put(t, "chat-url", sc.url+"\n", sc.hosts...)
			}
			st := storeWithSink(t, shoutrrr.Kind, cfg)
			d := newDispatcher(t, st, registry(t, rs), Options{
				Clock: &fakeClock{now: t0}, Logger: log, TracerProvider: tp, LookupEnv: envMap(env), Renderer: textRenderer{},
				Secrets: stored.resolver(), SecretDestinations: map[string][]string{shoutrrr.Kind: {"/url_env", "/url_ref"}},
				URLCheck: nc,
			})
			pollWait(t, d)
			del := delivery(t, st, "E0000")
			if del.Status != sc.want || !strings.Contains(del.LastError, sc.lastError) {
				t.Errorf("status %s %q, want %s with %q", del.Status, del.LastError, sc.want, sc.lastError)
			}
			lastErrors = append(lastErrors, del.LastError)
		})
	}
	mu.Lock()
	sent := strings.Join(paths, "\n")
	mu.Unlock()
	if !strings.Contains(sent, "hooks.slack.com/services/AAAAAAAAA/BBBBBBBBB/") || !strings.Contains(sent, local+"/releases") {
		t.Errorf("the mocked Slack endpoint or ntfy server got no message:\n%s", sent)
	}

	var errs []string
	for _, rs := range sinks {
		rs.mu.Lock()
		errs = append(errs, rs.errs...)
		rs.mu.Unlock()
	}
	haystacks := map[string]string{
		"logs":       logs.String(),
		"spans":      telemetrytest.Dump(rec.Ended()),
		"last_error": strings.Join(lastErrors, "\n"),
		"errors":     strings.Join(errs, "\n"),
	}
	for where, text := range haystacks {
		if text == "" {
			t.Errorf("%s: nothing recorded; the capture is not wired", where)
		}
		for _, f := range []string{slackToken, "SLACKSECRET", ntfyPass, "releases:", "slack://", "ntfy://", "Basic ", string(stored.master)} {
			if strings.Contains(text, f) {
				t.Errorf("%s contains the URL or a credential (%q)", where, f)
			}
		}
	}
}
