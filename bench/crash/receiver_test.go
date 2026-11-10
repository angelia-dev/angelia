package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/sink/webhook"
)

func signed(t *testing.T, secret, path, id string, body []byte) *http.Request {
	t.Helper()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig, err := webhook.Sign([]string{secret}, id, ts, body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	r.Header.Set(webhook.HeaderID, id)
	r.Header.Set(webhook.HeaderTimestamp, ts)
	r.Header.Set(webhook.HeaderSignature, sig)
	return r
}

func TestReceiverRecordsSignedDeliveries(t *testing.T) {
	secret, err := newSecret()
	if err != nil {
		t.Fatal(err)
	}
	other, _ := newSecret()
	rv := NewReceiver(secret, 0)
	body := []byte(`{"id":"01EV","type":"dev.angelia.release.published.v1"}`)

	w := httptest.NewRecorder()
	rv.ServeHTTP(w, signed(t, secret, "/hook/hook-1", "01EV:01SUB", body))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	// A wrong signature is answered 200 too (a 4xx would make the delivery
	// dead) and recorded as a failure.
	w = httptest.NewRecorder()
	rv.ServeHTTP(w, signed(t, other, "/hook/hook-1", "01EV:01SUB", body))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	w = httptest.NewRecorder()
	rv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/other", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d for a path that is no hook", w.Code)
	}

	got := rv.Receipts()
	if rv.Count() != 2 || len(got) != 2 {
		t.Fatalf("receipts %+v", got)
	}
	if r := got[0]; r.Sink != "hook-1" || r.WebhookID != "01EV:01SUB" || r.EventID != "01EV" || r.SigError != "" {
		t.Errorf("first receipt %+v", r)
	}
	if got[1].SigError == "" {
		t.Error("a signature with another secret was accepted")
	}
	if got[0].BodyHash != got[1].BodyHash {
		t.Error("same body, different hashes")
	}
}

func TestReceiverCountsOpenRequests(t *testing.T) {
	secret, _ := newSecret()
	rv := NewReceiver(secret, 300*time.Millisecond)
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			rv.ServeHTTP(httptest.NewRecorder(), signed(t, secret, "/hook/hook-0", "a:b", []byte(`{"id":"a"}`)))
		})
	}
	deadline := time.Now().Add(2 * time.Second)
	for rv.Open() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if rv.Open() == 0 {
		t.Error("no open request seen during the delay")
	}
	wg.Wait()
	if rv.Open() != 0 || rv.Count() != 3 {
		t.Errorf("after: open %d, count %d", rv.Open(), rv.Count())
	}
}
