package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// Retention test times: old events are 10 days old, new ones are current, and
// the cutoff is one day before t0.
var (
	pruneOld    = t0.Add(-10 * 24 * time.Hour)
	pruneBefore = t0.Add(-24 * time.Hour)
)

func eventAt(id string, at time.Time) store.Event {
	return store.Event{ID: id, WatchID: "w1", Type: "release.published", Payload: json.RawMessage(`{"v":1}`), CreatedAt: at}
}

func deliveryWith(id, eventID, subID, status string) store.Delivery {
	return store.Delivery{ID: id, EventID: eventID, SubscriptionID: subID, Status: status}
}

// listEventIDs returns the IDs of every retained event, in commit order.
func listEventIDs(t *testing.T, s *Store) []string {
	t.Helper()
	evs, err := s.ListEventsAfter(t.Context(), 0, 1000)
	must(t, err)
	ids := make([]string, len(evs))
	for i, e := range evs {
		ids[i] = e.ID
	}
	return ids
}

func countDeliveries(t *testing.T, s *Store, eventID string) int {
	t.Helper()
	ds, err := s.ListDeliveries(t.Context(), eventID)
	must(t, err)
	return len(ds)
}

// TestPruneHistoryRetention: events older than the cutoff are pruned together
// with their terminal deliveries, in batches. Events with a pending or
// in-flight delivery are never pruned, and neither are newer events. Config
// rows are untouched.
func TestPruneHistoryRetention(t *testing.T) {
	s, _ := openTest(t)
	seed(t, s)
	ctx := t.Context()

	must(t, s.AppendEvent(ctx, eventAt("e1", pruneOld), []store.Delivery{
		deliveryWith("d1a", "e1", "s1", store.DeliveryDelivered),
		deliveryWith("d1b", "e1", "s2", store.DeliveryDead),
	}))
	must(t, s.AppendEvent(ctx, eventAt("e2", pruneOld), []store.Delivery{
		deliveryWith("d2a", "e2", "s1", store.DeliveryDelivered),
		deliveryWith("d2b", "e2", "s2", store.DeliveryPending),
	}))
	must(t, s.AppendEvent(ctx, eventAt("e3", pruneOld), []store.Delivery{
		deliveryWith("d3a", "e3", "s1", store.DeliveryInFlight),
	}))
	must(t, s.AppendEvent(ctx, eventAt("e4", pruneOld), nil))
	must(t, s.AppendEvent(ctx, eventAt("e5", pruneOld), []store.Delivery{
		deliveryWith("d5a", "e5", "s1", store.DeliveryDelivered),
	}))
	must(t, s.AppendEvent(ctx, eventAt("e6", t0), []store.Delivery{
		deliveryWith("d6a", "e6", "s1", store.DeliveryDelivered),
	}))

	// Batches of 2 events: the old events span three batches.
	res, err := s.PruneHistory(ctx, pruneBefore, 2)
	must(t, err)
	// e1, e4 and e5 are pruned. Their deliveries are d1a, d1b and d5a.
	if res.Events != 3 || res.Deliveries != 3 {
		t.Fatalf("first prune = %+v, want 3 events and 3 deliveries", res)
	}

	if got, want := listEventIDs(t, s), []string{"e2", "e3", "e6"}; !equalStrings(got, want) {
		t.Fatalf("retained events = %v, want %v", got, want)
	}
	// e2 keeps its delivered delivery because a sibling is still pending.
	if n := countDeliveries(t, s, "e2"); n != 2 {
		t.Errorf("e2 has %d deliveries, want 2 (pending sibling keeps the event whole)", n)
	}
	if n := countDeliveries(t, s, "e3"); n != 1 {
		t.Errorf("e3 has %d deliveries, want 1 (in_flight is never pruned)", n)
	}
	if n := countDeliveries(t, s, "e6"); n != 1 {
		t.Errorf("e6 has %d deliveries, want 1 (newer than the cutoff)", n)
	}

	// Config rows are not history: they survive the prune.
	if _, err := s.GetTarget(ctx, "t1"); err != nil {
		t.Errorf("target removed by prune: %v", err)
	}
	if _, err := s.GetWatch(ctx, "w1"); err != nil {
		t.Errorf("watch removed by prune: %v", err)
	}

	// A second run finds nothing left to prune.
	again, err := s.PruneHistory(ctx, pruneBefore, 2)
	must(t, err)
	if again != (store.PruneResult{}) {
		t.Errorf("second prune = %+v, want nothing", again)
	}
}

// TestPruneHistoryPendingReleasedLater: an old event whose delivery was pending
// survives every prune until the delivery reaches a terminal state.
func TestPruneHistoryPendingReleasedLater(t *testing.T) {
	s, _ := openTest(t)
	seed(t, s)
	ctx := t.Context()
	must(t, s.AppendEvent(ctx, eventAt("e1", pruneOld), []store.Delivery{
		deliveryWith("d1", "e1", "s1", store.DeliveryPending),
	}))

	for i := 0; i < 2; i++ {
		res, err := s.PruneHistory(ctx, pruneBefore, 1)
		must(t, err)
		if res != (store.PruneResult{}) {
			t.Fatalf("prune %d of a pending delivery = %+v, want nothing", i, res)
		}
	}
	if n := countDeliveries(t, s, "e1"); n != 1 {
		t.Fatalf("pending delivery count = %d, want 1", n)
	}

	// The dispatcher finishes the delivery (done here with SQL: the dispatcher
	// is a later story). The next prune removes it.
	_, err := s.db.ExecContext(ctx, `UPDATE delivery SET status = 'delivered' WHERE id = 'd1'`)
	must(t, err)
	res, err := s.PruneHistory(ctx, pruneBefore, 1)
	must(t, err)
	if res.Events != 1 || res.Deliveries != 1 {
		t.Fatalf("prune after delivery = %+v, want 1 event and 1 delivery", res)
	}
}

// TestPruneHistoryCursorNotReused: after the newest events are pruned, a new
// event still gets a larger seq, so a consumer's cursor never points at a
// reused value (ADR 0008).
func TestPruneHistoryCursorNotReused(t *testing.T) {
	s, _ := openTest(t)
	seed(t, s)
	ctx := t.Context()
	must(t, s.AppendEvent(ctx, eventAt("e1", pruneOld), nil))
	before, err := s.ListEventsAfter(ctx, 0, 10)
	must(t, err)
	if len(before) != 1 {
		t.Fatalf("events = %+v", before)
	}
	oldSeq := before[0].Seq

	res, err := s.PruneHistory(ctx, pruneBefore, 0) // 0: default batch size
	must(t, err)
	if res.Events != 1 {
		t.Fatalf("prune = %+v, want 1 event", res)
	}
	if got := listEventIDs(t, s); len(got) != 0 {
		t.Fatalf("events after prune = %v", got)
	}

	must(t, s.AppendEvent(ctx, eventAt("e2", t0), nil))
	after, err := s.ListEventsAfter(ctx, oldSeq, 10)
	must(t, err)
	if len(after) != 1 || after[0].ID != "e2" || after[0].Seq <= oldSeq {
		t.Fatalf("event after prune = %+v, want e2 with seq > %d", after, oldSeq)
	}
}

// TestPruneHistoryNothingOld: a cutoff before every event prunes nothing, and a
// cancelled context fails without deleting an old event.
func TestPruneHistoryNothingOld(t *testing.T) {
	s, _ := openTest(t)
	seed(t, s)
	ctx := t.Context()
	must(t, s.AppendEvent(ctx, eventAt("e1", pruneOld), []store.Delivery{
		deliveryWith("d1", "e1", "s1", store.DeliveryDelivered),
	}))

	res, err := s.PruneHistory(ctx, pruneOld.Add(-time.Hour), 10)
	must(t, err)
	if res != (store.PruneResult{}) {
		t.Fatalf("prune before all events = %+v", res)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.PruneHistory(cancelled, pruneBefore, 10); err == nil {
		t.Fatal("prune with a cancelled context succeeded")
	}
	if got := listEventIDs(t, s); len(got) != 1 {
		t.Fatalf("events after cancelled prune = %v, want e1 kept", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPruneHistoryManyBatches: a prune spanning many small batches removes
// every old event and keeps the newer ones.
func TestPruneHistoryManyBatches(t *testing.T) {
	s, _ := openTest(t)
	seed(t, s)
	ctx := t.Context()
	const old, recent = 250, 30
	for i := 0; i < old+recent; i++ {
		at := pruneOld
		if i >= old {
			at = t0
		}
		id := fmt.Sprintf("e%03d", i)
		must(t, s.AppendEvent(ctx, eventAt(id, at), []store.Delivery{
			deliveryWith("d-"+id, id, "s1", store.DeliveryDelivered),
		}))
	}

	res, err := s.PruneHistory(ctx, pruneBefore, 7)
	must(t, err)
	if res.Events != old || res.Deliveries != old {
		t.Fatalf("prune = %+v, want %d events and %d deliveries", res, old, old)
	}
	got := listEventIDs(t, s)
	if len(got) != recent || got[0] != fmt.Sprintf("e%03d", old) {
		t.Fatalf("retained %d events, first %v; want %d recent events", len(got), got[:1], recent)
	}
}
