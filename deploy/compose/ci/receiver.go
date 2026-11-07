package main

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/angelia-dev/angelia/internal/sink/webhook"
)

const maxBody = 1 << 20

// delivery is one accepted webhook request.
type delivery struct {
	WebhookID string          `json:"webhook_id"`
	Type      string          `json:"type"`
	Version   string          `json:"version"`
	Package   string          `json:"package"`
	Body      json.RawMessage `json:"body"`
}

// receiver accepts POST /hook when its Standard Webhooks signature is valid
// and lists what it accepted on GET /received.
type receiver struct {
	secrets []string
	now     func() time.Time

	mu  sync.Mutex
	got []delivery
}

func newReceiver(secrets []string, now func() time.Time) http.Handler {
	r := &receiver{secrets: secrets, now: now, got: []delivery{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hook", r.hook)
	mux.HandleFunc("GET /received", r.received)
	return mux
}

func (r *receiver) hook(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxBody))
	if err != nil {
		http.Error(w, "body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	if err := webhook.Verify(r.secrets, req.Header, body, r.now(), webhook.DefaultTolerance); err != nil {
		http.Error(w, "signature: "+err.Error(), http.StatusUnauthorized)
		return
	}
	var ev struct {
		Type string `json:"type"`
		Data struct {
			Package string `json:"package"`
			Version string `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "body is not JSON", http.StatusBadRequest)
		return
	}
	d := delivery{WebhookID: req.Header.Get(webhook.HeaderID), Type: ev.Type, Version: ev.Data.Version, Package: ev.Data.Package, Body: body}
	r.mu.Lock()
	r.got = append(r.got, d)
	r.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (r *receiver) received(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	out := append([]delivery(nil), r.got...)
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// splitSecrets splits a space-separated list of secrets, as the sink takes it.
func splitSecrets(s string) []string {
	var out []string
	start := -1
	for i := 0; i <= len(s); i++ {
		sep := i == len(s) || s[i] == ' ' || s[i] == '\n' || s[i] == '\t'
		switch {
		case !sep && start < 0:
			start = i
		case sep && start >= 0:
			out = append(out, s[start:i])
			start = -1
		}
	}
	return out
}
