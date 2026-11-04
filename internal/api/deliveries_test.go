package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/store"
)

// deliveryEnv seeds the example config and three events with one delivery
// each: dead (3 attempts), delivered, pending.
func deliveryEnv(t *testing.T) (*env, []store.Event) {
	t.Helper()
	e := newEnv(t)
	e.seedConfig()
	sid, wid := e.subscriptionID("platform-ci"), e.watchID("requests")
	var evs []store.Event
	for i, st := range []string{store.DeliveryDead, store.DeliveryDelivered, store.DeliveryPending} {
		d := store.Delivery{SubscriptionID: sid, Status: st, Attempts: 3, LastError: "POST https://ci.example.com/hooks/angelia?token=abc: 500"}
		if st == store.DeliveryPending {
			d.Attempts, d.LastError, d.NextAttemptAt = 0, "", e.now
		}
		evs = append(evs, e.appendEvent(wid, event.TypePublished, "pypi", e.now.Add(time.Duration(i)*time.Hour), d))
	}
	return e, evs
}

func (e *env) deliveryOf(ev store.Event) map[string]any {
	e.t.Helper()
	m := e.must(http.StatusOK, e.do("GET", "/v1/deliveries?event="+ev.ID, tokRead, nil)).json(e.t)
	items := m["items"].([]any)
	if len(items) != 1 {
		e.t.Fatalf("event %s: %d deliveries", ev.ID, len(items))
	}
	return items[0].(map[string]any)
}

func TestDeliveryList(t *testing.T) {
	e, evs := deliveryEnv(t)
	for _, tc := range []struct {
		query string
		n     int
	}{
		{"", 3},
		{"status=dead", 1},
		{"status=dead&status=pending", 2},
		{"subscription=platform-ci", 3},
		{"subscription=nothing", 0},
		{"event=" + evs[1].ID, 1},
	} {
		m := e.must(http.StatusOK, e.do("GET", "/v1/deliveries?"+tc.query, tokRead, nil)).json(t)
		if n := len(m["items"].([]any)); n != tc.n {
			t.Errorf("%q: %d items, want %d", tc.query, n, tc.n)
		}
	}
	// Newest event first, one per page.
	var seqs []any
	path := "/v1/deliveries?limit=1"
	for path != "" {
		m := e.must(http.StatusOK, e.do("GET", path, tokRead, nil)).json(t)
		for _, it := range m["items"].([]any) {
			seqs = append(seqs, it.(map[string]any)["event_id"])
		}
		path = ""
		if c, ok := m["next_cursor"].(string); ok {
			path = "/v1/deliveries?limit=1&cursor=" + c
		}
	}
	if len(seqs) != 3 || seqs[0] != evs[2].ID || seqs[2] != evs[0].ID {
		t.Errorf("order: %v", seqs)
	}
	// last_error never carries the full URL.
	d := e.deliveryOf(evs[0])
	if le, _ := d["last_error"].(string); strings.Contains(le, "token=abc") || strings.Contains(le, "/hooks/") {
		t.Errorf("last_error = %q", le)
	}
	if d["next_attempt_at"] != nil {
		t.Errorf("a dead delivery has next_attempt_at %v", d["next_attempt_at"])
	}
	e.must(http.StatusNotFound, e.do("GET", "/v1/deliveries/01JBX2A7C8D9E0F1G2H3J4K5M6", tokRead, nil))
}

func TestReplayDelivery(t *testing.T) {
	e, evs := deliveryEnv(t)
	id := e.deliveryOf(evs[0])["id"].(string)
	m := e.must(http.StatusOK, e.do("POST", "/v1/deliveries/"+id+"/replay", tokWrite, nil)).json(t)
	if m["status"] != "pending" || m["attempts"] != float64(3) || m["next_attempt_at"] == nil {
		t.Errorf("replayed: %v", m)
	}
	// Pending: not replayable.
	r := e.do("POST", "/v1/deliveries/"+id+"/replay", tokWrite, nil)
	if r.Status != http.StatusConflict || r.problemType(t) != SlugNotReplayable {
		t.Errorf("again: %d %s", r.Status, r.Body)
	}
	e.must(http.StatusNotFound, e.do("POST", "/v1/deliveries/01JBX2A7C8D9E0F1G2H3J4K5M6/replay", tokWrite, nil))
	if !strings.Contains(e.log.String(), `"action":"replay"`) {
		t.Error("no audit record")
	}
}

func TestCreateReplay(t *testing.T) {
	e, evs := deliveryEnv(t)
	post := func(body map[string]any) map[string]any {
		t.Helper()
		return e.must(http.StatusOK, e.do("POST", "/v1/replays", tokWrite, body)).json(t)
	}
	// Dry run: counts, writes nothing.
	m := post(map[string]any{"subscription": "platform-ci", "since": "2026-11-03T00:00:00Z", "until": "2026-11-04T00:00:00Z", "status": []any{"dead", "delivered"}, "dry_run": true})
	if m["replayed"] != float64(2) || m["dry_run"] != true || m["truncated"] != false {
		t.Fatalf("dry run: %v", m)
	}
	if e.deliveryOf(evs[0])["status"] != "dead" {
		t.Fatal("the dry run wrote")
	}
	// The window is on the event time: only the first event.
	m = post(map[string]any{"subscription": "platform-ci", "since": "2026-11-03T09:00:00Z", "until": "2026-11-03T10:30:00Z", "status": []any{"dead", "delivered"}})
	if m["replayed"] != float64(1) {
		t.Fatalf("window: %v", m)
	}
	// By event, default status dead: the delivered one is left alone.
	m = post(map[string]any{"event": evs[1].ID})
	if m["replayed"] != float64(0) {
		t.Fatalf("event: %v", m)
	}
	for _, body := range []map[string]any{
		{"since": "2026-11-03T00:00:00Z", "until": "2026-11-04T00:00:00Z"},
		{"subscription": "platform-ci"},
		{"subscription": "platform-ci", "since": "2026-11-04T00:00:00Z", "until": "2026-11-03T00:00:00Z"},
		{"subscription": "nothing", "event": evs[0].ID},
	} {
		r := e.do("POST", "/v1/replays", tokWrite, body)
		if r.Status != http.StatusUnprocessableEntity || r.problemType(t) != SlugValidationFailed {
			t.Errorf("%v: %d %s", body, r.Status, r.Body)
		}
	}
}

// The access log has one record per request, without the token, the query
// or the body (acceptance criterion 4).
func TestAccessLog(t *testing.T) {
	e := newEnv(t)
	e.seedConfig()
	e.do("GET", "/v1/sinks/ci-webhook", tokAdmin, nil)
	e.do("GET", "/v1/sinks?dry_run=1", tokAdmin, nil)
	e.do("GET", "/v1/watches/"+tokRead, tokRead, nil)
	e.do("GET", "/v1/watches/ghp_0123456789abcdef", tokRead, nil)
	e.do("GET", "/v1/watches", "not-a-known-token-at-all-0000000", nil)
	log := e.log.String()
	for _, secret := range []string{tokRead, tokWrite, tokAdmin, "not-a-known-token", "ghp_", "ci.example.com", "/hooks/angelia", "CI_WEBHOOK_SECRET", "dry_run"} {
		if strings.Contains(log, secret) {
			t.Errorf("the log contains %q", secret)
		}
	}
	for _, want := range []string{`"msg":"api request"`, `"route":"/v1/sinks/{name}"`, `"status":200`, `"principal":"tok-admin"`, `"scope":"admin"`, `"status":401`, `"request_id"`, `"duration_ms"`} {
		if !strings.Contains(log, want) {
			t.Errorf("the log lacks %s:\n%s", want, log)
		}
	}
	if got := strings.Count(log, `"msg":"api request"`); got != 8 {
		t.Errorf("%d access records, want 8", got)
	}
}
