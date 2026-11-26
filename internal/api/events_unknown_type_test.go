package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/ulid"
)

// unknownTypeEnvelope is an event of a type spec 0.16.0 does not define
// (spec examples/valid/release/envelope/v1/unknown-type-future.json, with this
// event's id and watch): the event-type set is open, and its data fits no data
// schema of the spec. Compact, as the store keeps payloads.
func unknownTypeEnvelope(id, watchID string, at time.Time) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"specversion":"1.0","type":"dev.angelia.example.future.v1",`+
		`"source":"angelia://angelia/watches/%s","id":"%s","subject":"example:requests#EX-0001","time":"%s",`+
		`"datacontenttype":"application/json","dataschema":"https://example.com/schemas/future/v1.json",`+
		`"data":{"kind":"a payload this version of the spec does not define","ref":"EX-0001"}}`,
		watchID, id, at.UTC().Format("2006-01-02T15:04:05.000Z")))
}

func (e *env) appendUnknownTypeEvent(watchID string, at time.Time) store.Event {
	e.t.Helper()
	id := ulid.New(at)
	ev := store.Event{ID: id, WatchID: watchID, Type: "dev.angelia.example.future.v1", Payload: unknownTypeEnvelope(id, watchID, at), CreatedAt: at}
	if err := e.st.AppendEvent(context.Background(), ev, nil); err != nil {
		e.t.Fatal(err)
	}
	got, err := e.st.GetEvent(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

// TestEventUnknownType: a stored event of a type the spec does not define
// (spec 0.16.0, the open event-type set of angelia-dev/angelia#102) is listed,
// fetched and streamed with its envelope unchanged, and the filters that do
// not name it skip it without failing.
func TestEventUnknownType(t *testing.T) {
	e, ts := streamEnv(t)
	e.seedConfig()
	wid := e.watchID("requests")
	a := e.appendEvent(wid, event.TypePublished, "pypi", e.now)
	u := e.appendUnknownTypeEvent(wid, e.now.Add(time.Second))
	b := e.appendEvent(wid, event.TypeYanked, "pypi", e.now.Add(2*time.Second))

	p := e.eventPage("/v1/events")
	if len(p.Items) != 3 || p.Items[1].Seq != u.Seq {
		t.Fatalf("GET /v1/events: %+v", p)
	}
	if string(p.Items[1].Event) != string(u.Payload) {
		t.Errorf("event changed:\n got %s\nwant %s", p.Items[1].Event, u.Payload)
	}

	for _, tc := range []struct {
		query string
		want  []int64
	}{
		{"type=release.published&type=release.yanked", []int64{a.Seq, b.Seq}},
		{"source=pypi", []int64{a.Seq, b.Seq}},
		{"watch=requests", []int64{a.Seq, u.Seq, b.Seq}},
		{"order=desc", []int64{b.Seq, u.Seq, a.Seq}},
	} {
		var got []int64
		for _, it := range e.eventPage("/v1/events?" + tc.query).Items {
			got = append(got, it.Seq)
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s: got %v, want %v", tc.query, got, tc.want)
		}
	}

	r := e.must(http.StatusOK, e.do("GET", "/v1/events/"+u.ID, tokRead, nil))
	var one struct {
		Seq        int64           `json:"seq"`
		Event      json.RawMessage `json:"event"`
		RefEventID *string         `json:"ref_event_id"`
	}
	if err := json.Unmarshal(r.Body, &one); err != nil {
		t.Fatal(err)
	}
	if one.Seq != u.Seq || string(one.Event) != string(u.Payload) || one.RefEventID != nil {
		t.Errorf("GET /v1/events/{id}: %s", r.Body)
	}

	// The stream sends it unchanged between the release events.
	c := openStream(t, ts.URL+"/v1/events/stream", tokRead)
	c.expect("retry: 5000")
	c.expectEvent(a.Seq)
	c.expect("id: " + strconv.FormatInt(u.Seq, 10))
	data, _ := c.next()
	var rec struct {
		Seq   int64           `json:"seq"`
		Event json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(data, "data: ")), &rec); err != nil || rec.Seq != u.Seq || string(rec.Event) != string(u.Payload) {
		t.Fatalf("stream data %q: %v", data, err)
	}
	c.expectEvent(b.Seq)
}
