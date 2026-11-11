package sqlite

import (
	"strings"
	"testing"
)

// The lookup runs once per yank inside the check's transaction: it must use
// the event_watch_subject index, not scan the watch's events. The test
// expands sqlc's `/*SLICE:types*/?` placeholder by hand, so it depends on the
// form sqlc generates for sqlc.slice; regenerate and adjust if it changes.
func TestGetLatestEventBySubjectUsesTheIndex(t *testing.T) {
	s, _ := openTest(t)
	rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+strings.Replace(getLatestEventBySubject, "/*SLICE:types*/?", "?,?", 1),
		"w1", "pypi:requests@1.0", "a", "b")
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
	if p := strings.Join(plan, "; "); !strings.Contains(p, "event_watch_subject") {
		t.Fatalf("query plan does not use event_watch_subject: %s", p)
	}
}
