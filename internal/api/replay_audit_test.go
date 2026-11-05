package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/store"
)

// auditRecords returns the audit records of the log, in order.
func (e *env) auditRecords() []map[string]any {
	e.t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(e.log.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil || m["audit"] != true {
			continue
		}
		out = append(out, m)
	}
	return out
}

// The single replay is audited with who (principal, request id), what
// (action, delivery) and how many.
func TestReplayDeliveryAuditRecord(t *testing.T) {
	e, evs := deliveryEnv(t)
	id := e.deliveryOf(evs[0])["id"].(string)
	e.must(http.StatusOK, e.do("POST", "/v1/deliveries/"+id+"/replay", tokWrite, nil))
	recs := e.auditRecords()
	if len(recs) != 1 {
		t.Fatalf("%d audit records, want 1: %s", len(recs), e.log.String())
	}
	r := recs[0]
	for k, want := range map[string]any{"msg": "api audit", "action": "replay", "principal": "tok-write", "delivery": id, "deliveries": float64(1)} {
		if r[k] != want {
			t.Errorf("audit %s = %v, want %v", k, r[k], want)
		}
	}
	if rid, _ := r["request_id"].(string); rid == "" {
		t.Errorf("audit record without request_id: %v", r)
	}
	if fmt.Sprint(r["ids"]) != "["+id+"]" || fmt.Sprint(r["from_status"]) != "map[dead:1]" {
		t.Errorf("audit ids %v from_status %v", r["ids"], r["from_status"])
	}
	// A refused replay changes nothing and is not audited.
	e.do("POST", "/v1/deliveries/"+id+"/replay", tokWrite, nil)
	if n := len(e.auditRecords()); n != 1 {
		t.Errorf("%d audit records after a refused replay, want 1", n)
	}
}

// The bulk replay is audited with the selector, the count and the ids; a
// dry run writes nothing and is not audited.
func TestCreateReplayAuditRecord(t *testing.T) {
	e, evs := deliveryEnv(t)
	body := map[string]any{"subscription": "platform-ci", "since": "2026-11-03T00:00:00Z", "until": "2026-11-04T00:00:00Z", "status": []any{"dead", "delivered"}}
	dry := map[string]any{"dry_run": true}
	for k, v := range body {
		dry[k] = v
	}
	e.must(http.StatusOK, e.do("POST", "/v1/replays", tokWrite, dry))
	if n := len(e.auditRecords()); n != 0 {
		t.Fatalf("a dry run was audited (%d records)", n)
	}
	want := []any{e.deliveryOf(evs[0])["id"], e.deliveryOf(evs[1])["id"]}
	e.must(http.StatusOK, e.do("POST", "/v1/replays", tokAdmin, body))
	recs := e.auditRecords()
	if len(recs) != 1 {
		t.Fatalf("%d audit records, want 1", len(recs))
	}
	r := recs[0]
	for k, w := range map[string]any{
		"action": "replay", "principal": "tok-admin", "deliveries": float64(2),
		"subscription": "platform-ci", "since": "2026-11-03T00:00:00Z", "until": "2026-11-04T00:00:00Z",
	} {
		if r[k] != w {
			t.Errorf("audit %s = %v, want %v", k, r[k], w)
		}
	}
	if st, _ := r["status"].([]any); len(st) != 2 || st[0] != "dead" || st[1] != "delivered" {
		t.Errorf("audit status = %v", r["status"])
	}
	if r["subscription_id"] != e.subscriptionID("platform-ci") {
		t.Errorf("audit subscription_id = %v", r["subscription_id"])
	}
	if fmt.Sprint(r["from_status"]) != "map[dead:1 delivered:1]" {
		t.Errorf("audit from_status = %v", r["from_status"])
	}
	if ids, _ := r["ids"].([]any); len(ids) != 2 || ids[0] != want[0] || ids[1] != want[1] {
		t.Errorf("audit ids = %v, want %v", ids, want)
	}
	if _, ok := r["event"]; ok {
		t.Errorf("audit names an event it was not given: %v", r)
	}
	// By event: the record names the event and the default status, and a
	// replay that matched nothing is recorded too.
	m := e.must(http.StatusOK, e.do("POST", "/v1/replays", tokWrite, map[string]any{"event": evs[2].ID})).json(t)
	if m["replayed"] != float64(0) {
		t.Fatalf("a pending delivery was replayed: %v", m)
	}
	recs = e.auditRecords()
	r = recs[len(recs)-1]
	if r["event"] != evs[2].ID || r["deliveries"] != float64(0) || r["principal"] != "tok-write" {
		t.Errorf("audit by event: %v", r)
	}
	if st, _ := r["status"].([]any); len(st) != 1 || st[0] != "dead" {
		t.Errorf("audit default status = %v", r["status"])
	}
	for _, k := range []string{"subscription", "subscription_id"} {
		if _, ok := r[k]; ok {
			t.Errorf("audit names a %s it was not given: %v", k, r)
		}
	}
	// Nothing matched: ids and from_status are empty, not null.
	if ids, ok := r["ids"].([]any); !ok || len(ids) != 0 {
		t.Errorf("audit ids = %#v, want []", r["ids"])
	}
	if fs, ok := r["from_status"].(map[string]any); !ok || len(fs) != 0 {
		t.Errorf("audit from_status = %#v, want {}", r["from_status"])
	}
}

// A replay is refused with not-replayable while the delivery is pending or
// in flight, or once its subscription is gone; the bulk replay skips those.
func TestNotReplayable(t *testing.T) {
	e, evs := deliveryEnv(t)
	sid, wid := e.subscriptionID("platform-ci"), e.watchID("requests")
	inFlight := e.appendEvent(wid, event.TypePublished, "pypi", e.now.Add(3*time.Hour),
		store.Delivery{SubscriptionID: sid, Status: store.DeliveryInFlight, Attempts: 1, NextAttemptAt: e.now})
	notReplayable := func(name string, ev store.Event) {
		t.Helper()
		id := e.deliveryOf(ev)["id"].(string)
		r := e.do("POST", "/v1/deliveries/"+id+"/replay", tokWrite, nil)
		if r.Status != http.StatusConflict || r.problemType(t) != SlugNotReplayable {
			t.Errorf("%s: %d %s", name, r.Status, r.Body)
		}
	}
	notReplayable("pending", evs[2])
	notReplayable("in_flight", inFlight)
	// The bulk replay never touches pending or in_flight deliveries.
	for _, ev := range []store.Event{evs[2], inFlight} {
		m := e.must(http.StatusOK, e.do("POST", "/v1/replays", tokWrite, map[string]any{"event": ev.ID, "status": []any{"dead", "delivered"}})).json(t)
		if m["replayed"] != float64(0) {
			t.Errorf("bulk replayed a %s delivery: %v", e.deliveryOf(ev)["status"], m)
		}
	}
	// The subscription goes: its deliveries can no longer be replayed.
	e.must(http.StatusNoContent, e.do("DELETE", "/v1/subscriptions/platform-ci", tokWrite, nil))
	notReplayable("subscription gone", evs[0])
	m := e.must(http.StatusOK, e.do("POST", "/v1/replays", tokWrite, map[string]any{"event": evs[0].ID})).json(t)
	if m["replayed"] != float64(0) {
		t.Errorf("bulk replayed a delivery of a deleted subscription: %v", m)
	}
}

// sinkGoneTx is a transaction in which every sink is gone.
type sinkGoneTx struct{ store.APITx }

func (sinkGoneTx) GetSink(context.Context, string) (store.Sink, error) {
	return store.Sink{}, fmt.Errorf("sink: %w", store.ErrNotFound)
}

// A delivery whose sink is gone is not replayable. The API refuses to
// delete a sink in use, so the gone sink is simulated.
func TestNotReplayableSinkGone(t *testing.T) {
	e, evs := deliveryEnv(t)
	id := e.deliveryOf(evs[0])["id"].(string)
	err := e.st.InAPITx(t.Context(), func(tx store.APITx) error {
		d, err := tx.GetDelivery(t.Context(), id)
		if err != nil {
			return err
		}
		if why, err := replayable(t.Context(), tx, d); err != nil || why != "" {
			t.Errorf("with its sink: %q, %v", why, err)
		}
		why, err := replayable(t.Context(), sinkGoneTx{tx}, d)
		if err != nil || !strings.Contains(why, "sink no longer exists") {
			t.Errorf("sink gone: %q, %v", why, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A bulk replay queues every match but lists at most 100 of them, oldest
// event first, and says so with truncated; the audit record counts them all.
func TestCreateReplayLimit(t *testing.T) {
	e := newEnv(t)
	e.seedConfig()
	sid, wid := e.subscriptionID("platform-ci"), e.watchID("requests")
	const n = replayListMax + 5
	var first string
	for i := range n {
		ev := e.appendEvent(wid, event.TypePublished, "pypi", e.now.Add(time.Duration(i)*time.Second),
			store.Delivery{SubscriptionID: sid, Status: store.DeliveryDead, Attempts: 5, LastError: "500"})
		if i == 0 {
			first = ev.ID
		}
	}
	body := map[string]any{"subscription": "platform-ci", "since": "2026-11-03T00:00:00Z", "until": "2026-11-04T00:00:00Z"}
	m := e.must(http.StatusOK, e.do("POST", "/v1/replays", tokWrite, body)).json(t)
	ds := m["deliveries"].([]any)
	if m["replayed"] != float64(n) || m["truncated"] != true || len(ds) != replayListMax {
		t.Fatalf("replayed %v truncated %v listed %d", m["replayed"], m["truncated"], len(ds))
	}
	if d0 := ds[0].(map[string]any); d0["event_id"] != first || d0["status"] != "pending" || d0["attempts"] != float64(5) {
		t.Errorf("first listed: %v", d0)
	}
	recs := e.auditRecords()
	r := recs[len(recs)-1]
	if r["deliveries"] != float64(n) || len(r["ids"].([]any)) != replayListMax {
		t.Errorf("audit: deliveries %v, %d ids", r["deliveries"], len(r["ids"].([]any)))
	}
	left := e.must(http.StatusOK, e.do("GET", "/v1/deliveries?status=dead", tokRead, nil)).json(t)
	if k := len(left["items"].([]any)); k != 0 {
		t.Errorf("%d deliveries still dead", k)
	}
}

// The selector is checked against the schema before anything runs: no
// selector, a status that cannot be replayed, a time that is not RFC 3339
// UTC, an unknown member, or a read token.
func TestCreateReplayRejects(t *testing.T) {
	e, evs := deliveryEnv(t)
	for _, body := range []map[string]any{
		{},
		{"status": []any{"dead"}},
		{"event": evs[0].ID, "status": []any{"pending"}},
		{"event": evs[0].ID, "status": []any{}},
		{"subscription": "platform-ci", "since": "yesterday", "until": "2026-11-04T00:00:00Z"},
		{"subscription": "platform-ci", "since": "2026-11-03T00:00:00+02:00", "until": "2026-11-04T00:00:00Z"},
		{"event": "not-a-ulid"},
		{"event": evs[0].ID, "extra": true},
	} {
		r := e.do("POST", "/v1/replays", tokWrite, body)
		if r.Status != http.StatusUnprocessableEntity && r.Status != http.StatusBadRequest {
			t.Errorf("%v: %d %s", body, r.Status, r.Body)
		}
	}
	if r := e.do("POST", "/v1/replays", tokRead, map[string]any{"event": evs[0].ID}); r.Status != http.StatusForbidden {
		t.Errorf("a read token replayed: %d", r.Status)
	}
	if e.deliveryOf(evs[0])["status"] != "dead" {
		t.Error("a refused request replayed")
	}
	if n := len(e.auditRecords()); n != 0 {
		t.Errorf("%d audit records for refused requests", n)
	}
}
