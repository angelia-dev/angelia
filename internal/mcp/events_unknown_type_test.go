package mcp

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// TestEventsSinceUnknownType: events_since returns a stored event of a type
// the spec does not define (spec 0.16.0, the open event-type set of
// angelia-dev/angelia#102) unchanged, its result still matches the output
// schema, and a type filter that does not name it skips it.
func TestEventsSinceUnknownType(t *testing.T) {
	srv, st := realAPI(t)
	putWatch(t, srv.URL, st, "requests", `{"source":"pypi","coordinates":{"name":"requests"}}`)
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	const id = "01JBX2A7C8D9E0F1G2H3J4K5M7"
	payload := `{"specversion":"1.0","type":"dev.angelia.example.future.v1",` +
		`"source":"angelia://angelia/watches/01JBX1Z3M4N5P6Q7R8S9T0V1W2","id":"` + id + `",` +
		`"subject":"example:requests#EX-0001","time":"2026-11-01T00:00:00.000Z","datacontenttype":"application/json",` +
		`"dataschema":"https://example.com/schemas/future/v1.json","data":{"ref":"EX-0001","nested":{"n":[1,2]}}}`
	ev := store.Event{ID: id, WatchID: "01JBX1Z3M4N5P6Q7R8S9T0V1W2", Type: "dev.angelia.example.future.v1", Payload: json.RawMessage(payload), CreatedAt: t0}
	if err := st.AppendEvent(t.Context(), ev, nil); err != nil {
		t.Fatal(err)
	}

	cs := connect(t, srv.URL, tokRead)
	got := success(t, "events_since", callTool(t, cs, "events_since", map[string]any{}))
	items, _ := got["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("events_since: %v", got)
	}
	var want any
	if err := json.Unmarshal([]byte(payload), &want); err != nil {
		t.Fatal(err)
	}
	if item := items[0].(map[string]any); !jsonEqual(item["event"], want) || item["ref_event_id"] != nil {
		t.Errorf("event changed: %v", item)
	}

	got = success(t, "events_since", callTool(t, cs, "events_since", map[string]any{"type": []string{"release.published"}}))
	if items, _ := got["items"].([]any); len(items) != 0 {
		t.Errorf("type filter: %v", got)
	}
}
