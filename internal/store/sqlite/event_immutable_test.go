package sqlite

import (
	"testing"

	"github.com/angelia-dev/angelia/internal/store"
)

// The schema itself rejects an UPDATE of an event from any path (#106); the
// store API's share of the rule is in the shared suite.
func TestEventImmutableTrigger(t *testing.T) {
	s, _ := openTest(t)
	seed(t, s)
	ctx := t.Context()
	must(t, s.AppendEvent(ctx, event("e1"), []store.Delivery{pending("s1")}))
	for _, stmt := range []string{
		`UPDATE event SET payload_json = '{"v":3}' WHERE id = 'e1'`,
		`UPDATE event SET type = 'x', created_at = 0 WHERE id = 'e1'`,
		`UPDATE event SET id = 'e2' WHERE id = 'e1'`,
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err == nil {
			t.Errorf("%s: no error, want the immutability trigger to abort", stmt)
		}
	}
	evs, err := s.ListEventsAfter(ctx, 0, 0)
	must(t, err)
	if len(evs) != 1 || evs[0].ID != "e1" || string(evs[0].Payload) != `{"v":1}` || evs[0].Type != "release.published" || !evs[0].CreatedAt.Equal(t0) {
		t.Fatalf("events after refused writes = %+v, want e1 unchanged", evs)
	}
}
