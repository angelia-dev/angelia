package sqlite

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

func subjectEvent(id, watch, typ, subject string) store.Event {
	return store.Event{
		ID: id, WatchID: watch, Type: typ, CreatedAt: t0,
		Payload: json.RawMessage(`{"subject":` + mustJSON(subject) + `}`),
	}
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// AC of #112: a yank event references the original event; the reference is
// stored with the event and read back.
func TestEventRefEventIDRoundTrip(t *testing.T) {
	s, _ := openTest(t)
	seed(t, s)
	ctx := t.Context()
	must(t, s.AppendEvent(ctx, subjectEvent("e1", "w1", "published", "pypi:requests@1.0"), nil))
	y := subjectEvent("e2", "w1", "yanked", "pypi:requests@1.0")
	y.RefEventID = "e1"
	must(t, s.AppendEvent(ctx, y, nil))
	evs, err := s.ListEventsAfter(ctx, 0, 0)
	must(t, err)
	if len(evs) != 2 || evs[0].RefEventID != "" || evs[1].RefEventID != "e1" {
		t.Fatalf("events = %+v", evs)
	}
}

func TestGetLatestEventBySubject(t *testing.T) {
	s, _ := openTest(t)
	seed(t, s)
	ctx := t.Context()
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w2", TargetID: "t1"}))
	for _, e := range []store.Event{
		subjectEvent("e1", "w1", "published", "pypi:requests@1.0"),
		subjectEvent("e2", "w1", "published", "pypi:requests@1.1"),
		subjectEvent("e3", "w2", "published", "pypi:requests@1.0"),
		subjectEvent("e4", "w1", "prerelease", "pypi:requests@1.0"), // later, same subject
		subjectEvent("e5", "w1", "yanked", "pypi:requests@1.0"),     // a type not asked for
	} {
		must(t, s.AppendEvent(ctx, e, nil))
	}
	got := func(watch, subject string, types ...string) string {
		t.Helper()
		e, err := s.GetLatestEventBySubject(ctx, watch, subject, types...)
		if err != nil {
			wantErr(t, err, store.ErrNotFound, watch+" "+subject)
			return ""
		}
		return e.ID
	}
	for _, c := range []struct {
		watch, subject string
		types          []string
		want           string
	}{
		{"w1", "pypi:requests@1.0", []string{"published", "prerelease"}, "e4"},
		{"w1", "pypi:requests@1.0", []string{"published"}, "e1"},
		{"w2", "pypi:requests@1.0", []string{"published", "prerelease"}, "e3"},
		{"w1", "pypi:requests@1.1", []string{"published", "prerelease"}, "e2"},
		{"w2", "pypi:requests@1.1", []string{"published", "prerelease"}, ""},
		{"w1", "pypi:requests@2.0", []string{"published", "prerelease"}, ""},
		{"w1", "pypi:requests@1.0", nil, ""},
	} {
		if g := got(c.watch, c.subject, c.types...); g != c.want {
			t.Errorf("%s %s %v = %q, want %q", c.watch, c.subject, c.types, g, c.want)
		}
	}
	e, err := s.GetLatestEventBySubject(ctx, "w1", "pypi:requests@1.0", "published")
	must(t, err)
	if e.Seq == 0 || e.Type != "published" || !e.CreatedAt.Equal(t0) || !strings.Contains(string(e.Payload), "1.0") {
		t.Errorf("event = %+v", e)
	}
}

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

// AC (ADR 0014 section 4): a yank cancels the deliveries of the original
// event that are still held, and nothing else: not a hold that ended, not a
// plain pending delivery, not the holds of another event even when they name
// the same watch and raw version (a watch moved to another target).
func TestCancelHeldDeliveries(t *testing.T) {
	s, _ := openTest(t)
	seed(t, s)
	ctx := t.Context()
	until := t0.Add(72 * time.Hour)
	ended := heldDelivery("d-ended", "s2", t0) // its hold ended at t0
	must(t, s.AppendEvent(ctx, event("e1"), []store.Delivery{heldDelivery("d1", "s1", until), heldDelivery("d2", "s4", until), ended, pending("s3")}))
	// Same watch and raw version in the hold, another event.
	must(t, s.AppendEvent(ctx, event("e2"), []store.Delivery{heldDelivery("d-other-event", "s1", until)}))

	n, err := s.CancelHeldDeliveries(ctx, "e1", t0.Add(time.Hour))
	must(t, err)
	if n != 2 {
		t.Fatalf("cancelled %d, want 2", n)
	}
	left := map[string]bool{}
	for _, e := range []string{"e1", "e2"} {
		ds, err := s.ListDeliveries(ctx, e)
		must(t, err)
		for _, d := range ds {
			left[d.ID] = true
		}
	}
	for _, id := range []string{"d-ended", "d-s3", "d-other-event"} {
		if !left[id] {
			t.Errorf("%s was cancelled", id)
		}
	}
	for _, id := range []string{"d1", "d2"} {
		if left[id] {
			t.Errorf("%s is still there", id)
		}
	}
	evs, err := s.ListEventsAfter(ctx, 0, 0)
	must(t, err)
	if len(evs) != 2 {
		t.Fatalf("events after cancel = %d, want 2 (events are kept)", len(evs))
	}
	if n, err := s.CancelHeldDeliveries(ctx, "e1", t0.Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("second cancel = %d, %v", n, err)
	}
}
