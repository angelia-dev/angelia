package sqlite

import (
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

// TestPruneHistoryWritersBetweenBatches: between batches the prune holds no
// transaction, so a writer runs at once and its event is kept. The hook makes
// this deterministic: it runs after each committed batch.
func TestPruneHistoryWritersBetweenBatches(t *testing.T) {
	s, _ := openTest(t)
	seed(t, s)
	ctx := t.Context()
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("e%d", i)
		must(t, s.AppendEvent(ctx, eventAt(id, pruneOld), []store.Delivery{
			deliveryWith("d-"+id, id, "s1", store.DeliveryDelivered),
		}))
	}

	batches := 0
	res, err := s.pruneHistory(ctx, pruneBefore, 2, func() {
		batches++
		// A writer runs now. If the prune still held the write lock, this
		// insert would fail with a busy error after the busy timeout.
		id := fmt.Sprintf("late%d", batches)
		must(t, s.AppendEvent(ctx, eventAt(id, t0), []store.Delivery{
			deliveryWith("d-"+id, id, "s1", store.DeliveryPending),
		}))
	})
	must(t, err)
	// Six old events in batches of 2: three batches, and the hook runs after each.
	if batches != 3 {
		t.Fatalf("hook ran %d times, want 3", batches)
	}
	if res.Events != 6 || res.Deliveries != 6 {
		t.Fatalf("prune = %+v, want 6 events and 6 deliveries", res)
	}
	want := []string{"late1", "late2", "late3"}
	if got := listEventIDs(t, s); !equalStrings(got, want) {
		t.Fatalf("retained events = %v, want the writers' events %v", got, want)
	}
}

// TestPruneBatchSize: batch <= 0 means the default, and anything above the
// cap is cut to it.
func TestPruneBatchSize(t *testing.T) {
	for in, want := range map[int]int{
		-3:                      store.DefaultPruneBatch,
		0:                       store.DefaultPruneBatch,
		1:                       1,
		7:                       7,
		store.MaxPruneBatch:     store.MaxPruneBatch,
		store.MaxPruneBatch + 1: store.MaxPruneBatch,
		1 << 30:                 store.MaxPruneBatch,
	} {
		if got := pruneBatchSize(in); got != want {
			t.Errorf("pruneBatchSize(%d) = %d, want %d", in, got, want)
		}
	}
}
