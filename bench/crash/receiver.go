package main

import (
	"crypto/sha256"
	"encoding/json"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/angelia-dev/angelia/internal/sink/webhook"
)

// Receipt is one request the receiver got.
type Receipt struct {
	Sink      string    `json:"sink"`       // last element of the URL path
	WebhookID string    `json:"webhook_id"` // the idempotency key
	EventID   string    `json:"event_id"`   // the CloudEvents id in the body
	Package   string    `json:"package"`    // data.package in the body
	Version   string    `json:"version"`    // data.version in the body
	BodyHash  [32]byte  `json:"-"`
	SigError  string    `json:"sig_error,omitempty"` // empty: the Standard Webhooks signature verified
	At        time.Time `json:"at"`
}

// Receiver is the recording webhook receiver: it verifies each request's
// Standard Webhooks signature, records it and answers 200 after a short
// random delay, which keeps requests open long enough for a kill to land
// in the middle of some. It never fails a request: a 4xx would make the
// delivery dead and a 5xx would schedule a retry minutes later; a bad
// signature is recorded and fails the run instead.
type Receiver struct {
	secrets  []string
	maxDelay time.Duration
	now      func() time.Time

	open atomic.Int64 // requests being answered
	mu   sync.Mutex
	got  []Receipt
}

// NewReceiver returns a receiver for the signing secret (one to three
// whsec_ values separated by spaces).
func NewReceiver(secret string, maxDelay time.Duration) *Receiver {
	return &Receiver{secrets: strings.Fields(secret), maxDelay: maxDelay, now: time.Now}
}

// ServeHTTP implements http.Handler for POST /hook/<sink>.
func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.open.Add(1)
	defer r.open.Add(-1)
	sinkName, ok := strings.CutPrefix(req.URL.Path, "/hook/")
	if req.Method != http.MethodPost || !ok || sinkName == "" {
		// Not a delivery; no sink of this run sends here.
		http.NotFound(w, req)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	if err != nil {
		// The client went away (it was killed): nothing was received.
		return
	}
	rc := Receipt{Sink: sinkName, WebhookID: req.Header.Get(webhook.HeaderID), BodyHash: sha256.Sum256(body), At: r.now()}
	if err := webhook.Verify(r.secrets, req.Header, body, rc.At, webhook.DefaultTolerance); err != nil {
		rc.SigError = err.Error()
	}
	var ev struct {
		ID   string `json:"id"`
		Data struct {
			Package string `json:"package"`
			Version string `json:"version"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &ev) // an empty id is reported as unexpected
	rc.EventID, rc.Package, rc.Version = ev.ID, ev.Data.Package, ev.Data.Version
	if r.maxDelay > 0 {
		time.Sleep(rand.N(r.maxDelay)) //nolint:gosec // timing jitter, not security
	}
	// Recorded when the answer is about to be written: a request whose
	// sender was killed before this point may still count as received,
	// which only makes a duplicate more likely, never hides a loss.
	r.mu.Lock()
	r.got = append(r.got, rc)
	r.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// Open is the number of requests being answered now.
func (r *Receiver) Open() int { return int(r.open.Load()) }

// Count is the number of requests received so far.
func (r *Receiver) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

// Receipts returns a copy of everything received.
func (r *Receiver) Receipts() []Receipt {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Receipt(nil), r.got...)
}
