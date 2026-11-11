package sqlite

import (
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// seedDeliveries writes event e1 with a due delivery for s1 and one for s2
// due a minute later.
func seedDeliveries(t *testing.T, s *Store) {
	t.Helper()
	seed(t, s)
	d1 := pending("s1")
	d1.NextAttemptAt = t0
	d2 := pending("s2")
	d2.NextAttemptAt = t0.Add(time.Minute)
	must(t, s.AppendEvent(t.Context(), event("e1"), []store.Delivery{d1, d2}))
}

func leaseIDs(t *testing.T, s *Store, holder string, now time.Time, limit int) []string {
	t.Helper()
	got, err := s.LeaseDueDeliveries(t.Context(), holder, now, now.Add(time.Minute), limit)
	if err != nil {
		t.Fatalf("LeaseDueDeliveries(%s, %v): %v", holder, now, err)
	}
	ids := make([]string, len(got))
	for i, ld := range got {
		ids[i] = ld.Delivery.ID
	}
	return ids
}

// The due-delivery queries must use delivery_status_next_attempt_at_id for
// every term, not scan the delivery table.
func TestDueDeliveryQueriesUseTheIndex(t *testing.T) {
	s, _ := openTest(t)
	for name, q := range map[string]string{"list": listLeasableDueDeliveries, "has": hasLeasableDueDeliveries} {
		args := []any{millis(t0), millis(t0)}
		if name == "list" {
			args = append(args, 10)
		}
		rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+q, args...)
		must(t, err)
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			must(t, rows.Scan(&id, &parent, &unused, &detail))
			plan = append(plan, detail)
		}
		must(t, rows.Err())
		_ = rows.Close()
		joined := strings.Join(plan, "\n")
		t.Logf("%s plan:\n%s", name, joined)
		if strings.Contains(joined, "SCAN d\n") || strings.HasSuffix(joined, "SCAN d") ||
			!strings.Contains(joined, "delivery_status_next_attempt_at_id") ||
			strings.Contains(joined, "USING INDEX delivery_status_next_attempt_at ") ||
			strings.Contains(joined, "USING COVERING INDEX delivery_status_next_attempt_at ") {
			t.Errorf("%s does not use the index:\n%s", name, joined)
		}
	}
}

// HasDueDeliveries and the lease query treat a zero now alike: a time, not
// NULL. A NULL now made every in-flight row look unleased to the EXISTS
// query while the lease query leased nothing.
func TestHasDueDeliveriesZeroNowMatchesLease(t *testing.T) {
	s, _ := openTest(t)
	seedDeliveries(t, s)
	ctx := t.Context()
	if ids := leaseIDs(t, s, "a", t0, 1); len(ids) != 1 {
		t.Fatalf("lease at t0 = %v", ids)
	}
	var zero time.Time
	ok, err := s.HasDueDeliveries(ctx, zero)
	must(t, err)
	ids, err := s.q.ListLeasableDueDeliveries(ctx, ListLeasableDueDeliveriesParams{Now: millis(zero), MaxRows: 10})
	must(t, err)
	if ok || len(ids) != 0 {
		t.Errorf("zero now: HasDueDeliveries = %v, lease query = %v; want false and none", ok, ids)
	}
}
