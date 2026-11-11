package sqlite

import (
	"strings"
	"testing"
)

// The scrape-time count reads the status index only, not the table.
func TestCountDeliveriesByStatusUsesTheIndex(t *testing.T) {
	s, _ := openTest(t)
	rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+countDeliveriesByStatus)
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
	t.Logf("plan:\n%s", joined)
	if !strings.Contains(joined, "USING COVERING INDEX delivery_status_next_attempt_at_id") {
		t.Errorf("count does not use the status index:\n%s", joined)
	}
}
