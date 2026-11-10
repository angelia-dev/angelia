package sqlite

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// legacyListLeasableDueDeliveries is the lease query before story #232: one
// ORDER BY over the OR of the three kinds of due row. It is the reference
// for the order the rewritten query must keep.
const legacyListLeasableDueDeliveries = `SELECT d.id FROM delivery d
WHERE ((d.status = 'pending' AND d.next_attempt_at <= ?1)
       OR (d.status = 'pending' AND d.next_attempt_at IS NULL)
       OR d.status = 'in_flight')
  AND NOT EXISTS (
    SELECT 1 FROM lease
    WHERE lease.name = 'delivery:' || d.id AND lease.expires_at > ?1
  )
ORDER BY d.next_attempt_at, d.id
LIMIT ?2`

// The rewritten lease query returns the same rows in the same order as the
// legacy one, on a mix of statuses, NULL and tied next_attempt_at values and
// live, expired and missing leases, for every now and limit.
func TestListLeasableDueDeliveriesKeepsTheOrder(t *testing.T) {
	s, _ := openTest(t)
	seed(t, s)
	ctx := t.Context()
	r := rand.New(rand.NewPCG(232, 1)) //nolint:gosec // reproducible test data, not crypto
	statuses := []string{store.DeliveryPending, store.DeliveryPending, store.DeliveryInFlight, store.DeliveryDelivered, store.DeliveryDead}
	const n = 400
	ds := make([]store.Delivery, 0, n)
	for i := range n {
		ds = append(ds, store.Delivery{ID: fmt.Sprintf("d%03d", r.IntN(1000)*1000+i), SubscriptionID: fmt.Sprintf("s%03d", i), Status: store.DeliveryPending})
	}
	must(t, s.AppendEvent(ctx, event("e1"), ds))
	for _, d := range ds {
		var next any // NULL in one row out of four, else one of 11 shared minutes
		if r.IntN(4) > 0 {
			next = millis(t0.Add(time.Duration(r.IntN(11)-5) * time.Minute))
		}
		_, err := s.db.ExecContext(ctx, `UPDATE delivery SET status = ?, next_attempt_at = ? WHERE id = ?`, statuses[r.IntN(len(statuses))], next, d.ID)
		must(t, err)
		if k := r.IntN(3); k > 0 { // a lease ending 4 minutes before or after t0
			exp := t0.Add(time.Duration(8*k-12) * time.Minute)
			_, err := s.db.ExecContext(ctx, `INSERT INTO lease (name, holder, expires_at) VALUES (?, 'x', ?)`, store.DeliveryLeaseName(d.ID), millis(exp))
			must(t, err)
		}
	}
	nonEmpty := 0
	for _, now := range []time.Time{t0.Add(-10 * time.Minute), t0.Add(-4 * time.Minute), t0, t0.Add(3 * time.Minute), t0.Add(10 * time.Minute)} {
		for _, limit := range []int64{1, 2, 7, 16, 64, n} {
			want := queryIDs(t, s, legacyListLeasableDueDeliveries, millis(now), limit)
			got, err := s.q.ListLeasableDueDeliveries(ctx, ListLeasableDueDeliveriesParams{Now: millis(now), MaxRows: limit})
			must(t, err)
			if !slices.Equal(got, want) {
				t.Errorf("now %v, limit %d:\n got  %v\n want %v", now.Sub(t0), limit, got, want)
			}
			if len(want) > 0 {
				nonEmpty++
			}
		}
	}
	if nonEmpty < 20 {
		t.Fatalf("only %d of 30 cases had due rows; the data does not exercise the query", nonEmpty)
	}
}

func queryIDs(t *testing.T, s *Store, q string, args ...any) []string {
	t.Helper()
	rows, err := s.db.QueryContext(t.Context(), q, args...)
	must(t, err)
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		must(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	must(t, rows.Err())
	return ids
}

// Each branch of the lease query reads delivery_status_next_attempt_at_id in
// order and stops at its LIMIT; the only sort is the final merge of at most
// 3 * max_rows rows. Before story #232 the plan was a MULTI-INDEX OR with a
// sort of the whole due backlog.
func TestListLeasableDueDeliveriesReadsTheIndexInOrder(t *testing.T) {
	s, _ := openTest(t)
	plan := explain(t, s, listLeasableDueDeliveries, millis(t0), 16)
	t.Logf("plan:\n%s", strings.Join(plan, "\n"))
	var searches, sorts int
	for i, line := range plan {
		switch {
		case strings.HasPrefix(line, "SEARCH d "):
			searches++
			if !strings.Contains(line, "USING COVERING INDEX delivery_status_next_attempt_at_id ") {
				t.Errorf("branch does not read the order index: %s", line)
			}
		case strings.Contains(line, "MULTI-INDEX OR"), strings.HasPrefix(line, "SCAN d"):
			t.Errorf("plan reads the delivery table unordered: %s", line)
		case strings.Contains(line, "TEMP B-TREE"):
			sorts++
			if i != len(plan)-1 || line != "USE TEMP B-TREE FOR ORDER BY" {
				t.Errorf("sort inside a branch (line %d): %s", i, line)
			}
		}
	}
	if searches != 3 || sorts > 1 {
		t.Errorf("plan has %d index searches and %d sorts, want 3 and at most 1", searches, sorts)
	}
}

func explain(t *testing.T, s *Store, q string, args ...any) []string {
	t.Helper()
	rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+q, args...)
	must(t, err)
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		must(t, rows.Scan(&id, &parent, &unused, &detail))
		plan = append(plan, detail)
	}
	must(t, rows.Err())
	return plan
}
