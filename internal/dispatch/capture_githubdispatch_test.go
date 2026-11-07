package dispatch

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/sink"
	"github.com/angelia-dev/angelia/internal/sink/githubdispatch"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/telemetry"
	"github.com/angelia-dev/angelia/internal/telemetry/telemetrytest"
)

// recordingSink keeps every error the wrapped sink returns.
type recordingGHSink struct {
	sink.Sink
	mu   sync.Mutex
	errs []string
}

func (r *recordingGHSink) Deliver(ctx context.Context, req sink.Request) (sink.Result, error) {
	res, err := r.Sink.Deliver(ctx, req)
	if err != nil {
		r.mu.Lock()
		r.errs = append(r.errs, err.Error())
		r.mu.Unlock()
	}
	return res, err
}

// #113 acceptance criteria 2 and the secret rule of ADR 0023 section 4, end
// to end: a dispatcher with a debug JSON log and recorded spans delivers
// through the real github-dispatch sink and netguard client. 204 is
// delivered; 404 and 422 are dead at once; 500 and a rate-limited 403 stay
// pending for a retry; a redirect is dead; an unset variable, a missing
// stored token and a host the stored token is not bound to are retried
// before anything is sent. The token (from the environment or stored) never
// appears in the logs, the spans, delivery.last_error or the sink's errors.
func TestGithubDispatchDeliveriesNeverLogTheToken(t *testing.T) {
	const tok = "ghp_CAPTURETOKENCAPTURETOKENCAPTURE0123"
	handlers := map[string]http.HandlerFunc{
		"204": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) },
		"404": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
		"422": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnprocessableEntity) },
		"500": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		"403 rate limit": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "4102444800")
			w.WriteHeader(http.StatusForbidden)
		},
		"redirect": func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) },
		"echo": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Echo", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	type scenario struct {
		name, handler, env string
		ref                string
		hosts              []string
		want               string
	}
	scenarios := []scenario{
		{"204", "204", tok, "", nil, store.DeliveryDelivered},
		{"404", "404", tok, "", nil, store.DeliveryDead},
		{"422", "422", tok, "", nil, store.DeliveryDead},
		{"500", "500", tok, "", nil, store.DeliveryPending},
		{"403 rate limit", "403 rate limit", tok, "", nil, store.DeliveryPending},
		{"redirect", "redirect", tok, "", nil, store.DeliveryDead},
		{"echoing server", "echo", tok, "", nil, store.DeliveryPending},
		{"unset variable", "204", "", "", nil, store.DeliveryPending},
		{"ref 204", "204", tok, "gh-token", []string{"127.0.0.1:0"}, store.DeliveryDelivered},
		{"ref missing secret", "204", "", "gh-missing", nil, store.DeliveryPending},
		{"ref host mismatch", "204", tok, "gh-token", []string{"api.github.com"}, store.DeliveryPending},
	}
	stored := newSecretStore(t)
	logs := &syncBuffer{}
	log := slog.New(telemetry.NewLogHandler(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	rec, tp := telemetrytest.Install(t)
	var lastErrors []string
	var sinks []*recordingGHSink
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(handlers[sc.handler])
			defer srv.Close()
			nc, err := netguard.New(netguard.Config{
				Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, Logger: log, MaxAttempts: 1,
				Transport: srv.Client().Transport, Proxy: func(*http.Request) (*url.URL, error) { return nil, nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			rs := &recordingGHSink{Sink: githubdispatch.New(nc.HTTPClient(), githubdispatch.Options{})}
			sinks = append(sinks, rs)
			ref := `"token_env":"GH_CAPTURE"`
			env := map[string]string{}
			if sc.ref != "" {
				ref = `"token_ref":"` + sc.ref + `"`
				hosts := make([]string, len(sc.hosts))
				for i, h := range sc.hosts { // the receiver's port is known only now
					hosts[i] = strings.Replace(h, ":0", srv.URL[strings.LastIndex(srv.URL, ":"):], 1)
				}
				if sc.env != "" {
					stored.put(t, sc.ref, sc.env, hosts...)
				}
			} else if sc.env != "" {
				env["GH_CAPTURE"] = sc.env
			}
			st := storeWithSink(t, githubdispatch.Kind, `{"repo":"octo-org/octo-repo","api_base_url":"`+srv.URL+`",`+ref+`}`)
			d := newDispatcher(t, st, registry(t, rs), Options{
				Clock: &fakeClock{now: t0}, Logger: log, TracerProvider: tp, LookupEnv: envMap(env),
				Secrets: stored.resolver(), Destinations: map[string]string{githubdispatch.Kind: "/api_base_url"},
			})
			pollWait(t, d)
			del := delivery(t, st, "E0000")
			if del.Status != sc.want {
				t.Errorf("status %s, want %s (last_error %q)", del.Status, sc.want, del.LastError)
			}
			lastErrors = append(lastErrors, del.LastError)
		})
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
		for _, f := range []string{tok, strings.TrimPrefix(tok, "ghp_"), "Bearer", string(stored.master)} {
			if strings.Contains(text, f) {
				t.Errorf("%s contains the token or a credential (%d bytes)", where, len(f))
			}
		}
	}
	for _, want := range []string{
		"HTTP 404 Not Found (repository, workflow or token without access)", "HTTP 422 Unprocessable Entity",
		"HTTP 500 Internal Server Error", "HTTP 403 Forbidden", "redirect not followed",
		"environment variable GH_CAPTURE is not set or empty", "secret gh-token is bound to api.github.com; refused for 127.0.0.1:",
	} {
		if !strings.Contains(haystacks["last_error"], want) {
			t.Errorf("last_error lacks %q:\n%s", want, haystacks["last_error"])
		}
	}
}
