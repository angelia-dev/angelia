package dispatch

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
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

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/sink"
	"github.com/angelia-dev/angelia/internal/sink/webhook"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/telemetry"
	"github.com/angelia-dev/angelia/internal/telemetry/telemetrytest"
)

// syncBuffer is a log destination shared by the dispatcher's workers.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// recordingSink wraps the webhook sink and keeps every error it returns.
type recordingSink struct {
	*webhook.Sink
	mu   sync.Mutex
	errs []string
}

func (r *recordingSink) Deliver(ctx context.Context, req sink.Request) (sink.Result, error) {
	res, err := r.Sink.Deliver(ctx, req)
	if err != nil {
		r.mu.Lock()
		r.errs = append(r.errs, err.Error())
		r.mu.Unlock()
	}
	return res, err
}

// AC 3 (#108, ADR 0023 section 4): secrets are never logged. A dispatcher
// logging JSON at debug level and recording spans sends through the real
// webhook sink and netguard client to receivers that answer 200, 500, 404,
// 429, a redirect, a timeout and a reset, plus an invalid secret, an unset
// variable and an SSRF refusal. Neither the secrets, their base64 part, the
// hex of the keys nor any emitted webhook-signature appears in the logs,
// the spans, delivery.last_error or the sink's errors.
func TestSecretsNeverLogged(t *testing.T) {
	keys := [][]byte{make([]byte, 32), make([]byte, 32)}
	for _, k := range keys {
		_, _ = rand.Read(k)
	}
	secrets := []string{"whsec_" + base64.StdEncoding.EncodeToString(keys[0]), "whsec_" + base64.StdEncoding.EncodeToString(keys[1])}
	value := secrets[0] + " " + secrets[1] + "\n"                       // rotation, with a trailing newline
	invalid := "whsec_" + base64.RawURLEncoding.EncodeToString(keys[0]) // URL-safe: refused

	var sigMu sync.Mutex
	var sigs []string
	record := func(r *http.Request) {
		sigMu.Lock()
		defer sigMu.Unlock()
		if s := r.Header.Get(webhook.HeaderSignature); s != "" {
			sigs = append(sigs, s)
			sigs = append(sigs, strings.Fields(s)...)
		}
	}
	handlers := map[string]http.HandlerFunc{
		"200": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
		"500": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		"404": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
		"429": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
		},
		"redirect": func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) },
		"timeout": func(_ http.ResponseWriter, r *http.Request) {
			// Reading the body lets the server notice the client hang up.
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
		},
		"reset": func(w http.ResponseWriter, _ *http.Request) {
			if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = c.Close()
			}
		},
	}
	type scenario struct {
		name, handler, env string
		blocked            bool
		wantStatus         string
	}
	scenarios := []scenario{
		{"200", "200", value, false, store.DeliveryDelivered},
		{"500", "500", value, false, store.DeliveryPending},
		{"404", "404", value, false, store.DeliveryDead},
		{"429", "429", value, false, store.DeliveryPending},
		{"redirect", "redirect", value, false, store.DeliveryDead},
		{"timeout", "timeout", value, false, store.DeliveryPending},
		{"reset", "reset", value, false, store.DeliveryPending},
		{"invalid secret", "200", invalid, false, store.DeliveryPending},
		{"unset variable", "200", "", false, store.DeliveryPending},
		{"ssrf", "200", value, true, store.DeliveryDead},
	}

	logs := &syncBuffer{}
	log := slog.New(telemetry.NewLogHandler(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	rec, tp := telemetrytest.Install(t)
	var lastErrors []string
	var sinks []*recordingSink
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				record(r)
				handlers[sc.handler](w, r)
			}))
			defer srv.Close()
			var allow []netip.Prefix
			if !sc.blocked {
				allow = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
			}
			nc, err := netguard.New(netguard.Config{
				Allow: allow, Logger: log, MaxAttempts: 1,
				Proxy: func(*http.Request) (*url.URL, error) { return nil, nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			rs := &recordingSink{Sink: webhook.New(nc.HTTPClient(), webhook.Options{})}
			sinks = append(sinks, rs)
			st := storeWithSink(t, webhook.Kind, `{"url":"`+srv.URL+`/hook","secret_env":"WH_CAPTURE","timeout":"10s"}`)
			env := map[string]string{}
			if sc.env != "" {
				env["WH_CAPTURE"] = sc.env
			}
			opts := Options{Clock: &fakeClock{now: t0}, Logger: log, TracerProvider: tp, LookupEnv: envMap(env)}
			if sc.handler == "timeout" {
				opts.DeliverTimeout = 200 * time.Millisecond
			}
			d := newDispatcher(t, st, registry(t, rs), opts)
			pollWait(t, d)
			del := delivery(t, st, "E0000")
			if del.Status != sc.wantStatus {
				t.Errorf("status %s, want %s (last_error %q)", del.Status, sc.wantStatus, del.LastError)
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
	sigMu.Lock()
	forbidden := append([]string(nil), sigs...)
	sigMu.Unlock()
	if len(forbidden) < 2 {
		t.Fatalf("only %d signatures captured; the scenarios did not sign", len(forbidden))
	}
	for i, s := range secrets {
		forbidden = append(forbidden, s, strings.TrimPrefix(s, "whsec_"), hex.EncodeToString(keys[i]))
	}
	forbidden = append(forbidden, invalid, strings.TrimPrefix(invalid, "whsec_"))
	haystacks := map[string]string{
		"logs":       logs.String(),
		"spans":      telemetrytest.Dump(rec.Ended()),
		"last_error": strings.Join(lastErrors, "\n"),
		"errors":     strings.Join(errs, "\n"),
	}
	for where, text := range haystacks {
		if text == "" && where != "errors" {
			t.Errorf("%s: nothing recorded; the capture is not wired", where)
		}
		for _, f := range forbidden {
			if strings.Contains(text, f) {
				t.Errorf("%s contains a secret or signature (%d bytes)", where, len(f))
			}
		}
	}
	// The references are named, so that the operator can act.
	for _, want := range []string{"secret_env WH_CAPTURE: invalid secret", "environment variable WH_CAPTURE is not set or empty", "destination not allowed (127.0.0.1)"} {
		if !strings.Contains(haystacks["last_error"], want) {
			t.Errorf("last_error lacks %q:\n%s", want, haystacks["last_error"])
		}
	}
}
