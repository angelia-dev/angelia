package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/store"
)

// Consumers (story #79, sub-story 79-C): named server-side event cursors.

type consumerBody struct {
	Name   string `json:"name"`
	ID     string `json:"id"`
	Filter struct {
		Watch  []string `json:"watch"`
		Type   []string `json:"type"`
		Source []string `json:"source"`
	} `json:"filter"`
	AckedSeq  int64      `json:"acked_seq"`
	AckedAt   *time.Time `json:"acked_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

func (e *env) consumer(r result) consumerBody {
	e.t.Helper()
	var c consumerBody
	if err := json.Unmarshal(r.Body, &c); err != nil {
		e.t.Fatalf("consumer body: %v: %s", err, r.Body)
	}
	return c
}

func (e *env) getConsumer(name string) consumerBody {
	e.t.Helper()
	return e.consumer(e.must(http.StatusOK, e.do("GET", "/v1/consumers/"+name, tokRead, nil)))
}

// seedEvents appends n events of the requests watch, alternating published
// (pypi) and yanked (github-release), one second apart from at.
func (e *env) seedEvents(n int, at time.Time) []store.Event {
	e.t.Helper()
	wid := e.watchID("requests")
	out := make([]store.Event, 0, n)
	for i := range n {
		typ, src := event.TypePublished, "pypi"
		if i%2 == 1 {
			typ, src = event.TypeYanked, "github-release"
		}
		out = append(out, e.appendEvent(wid, typ, src, at.Add(time.Duration(i)*time.Second)))
	}
	return out
}

func wantProblem(t *testing.T, r result, status int, slug string) {
	t.Helper()
	if r.Status != status || r.problemType(t) != slug {
		t.Fatalf("status %d, want %d %s: %s", r.Status, status, slug, r.Body)
	}
}

// PUT creates (201, Location), then is idempotent (unchanged), replaces the
// filter (updated) and never moves acked_seq; start is ignored on an
// existing consumer. GET, list paging and DELETE with dry runs.
func TestConsumerCRUD(t *testing.T) {
	e := newEnv(t)
	e.seedConfig()
	evs := e.seedEvents(3, e.now.Add(-time.Hour))
	put := map[string]any{"filter": map[string]any{"watch": []any{"requests"}, "type": []any{"release.published"}}}

	// A dry run creates nothing.
	r := e.must(http.StatusCreated, e.do("PUT", "/v1/consumers/bot?dry_run=true", tokWrite, put))
	if r.Header.Get("Angelia-Change") != "created" {
		t.Errorf("dry run: %v", r.Header)
	}
	e.must(http.StatusNotFound, e.do("GET", "/v1/consumers/bot", tokRead, nil))

	r = e.must(http.StatusCreated, e.do("PUT", "/v1/consumers/bot", tokWrite, put))
	if r.Header.Get("Location") != "/v1/consumers/bot" || r.Header.Get("Angelia-Change") != "created" {
		t.Errorf("create headers: %v", r.Header)
	}
	c := e.consumer(r)
	if c.Name != "bot" || c.ID == "" || c.AckedSeq != evs[2].Seq || c.AckedAt != nil ||
		!c.CreatedAt.Equal(e.now) || !c.UpdatedAt.Equal(e.now) {
		t.Errorf("created: %+v", c)
	}
	if strings.Join(c.Filter.Watch, ",") != "requests" || strings.Join(c.Filter.Type, ",") != "release.published" ||
		c.Filter.Source == nil || len(c.Filter.Source) != 0 {
		t.Errorf("filter: %+v (%s)", c.Filter, r.Body)
	}

	// The same body again: unchanged.
	e.now = e.now.Add(time.Minute)
	r = e.must(http.StatusOK, e.do("PUT", "/v1/consumers/bot", tokWrite, put))
	if r.Header.Get("Angelia-Change") != "unchanged" || r.Header.Get("Location") != "" {
		t.Errorf("second PUT: %v", r.Header)
	}
	if got := e.consumer(r); got.ID != c.ID || !got.UpdatedAt.Equal(c.UpdatedAt) {
		t.Errorf("unchanged: %+v", got)
	}

	// Ack, then a new filter with start: earliest: the filter changes, the
	// cursor does not.
	next := e.seedEvents(1, e.now)[0]
	e.must(http.StatusOK, e.do("POST", "/v1/consumers/bot/ack", tokWrite, map[string]any{"seq": next.Seq}))
	r = e.must(http.StatusOK, e.do("PUT", "/v1/consumers/bot", tokWrite, map[string]any{
		"name": "bot", "filter": map[string]any{"source": []any{"pypi"}}, "start": "earliest",
	}))
	got := e.consumer(r)
	if r.Header.Get("Angelia-Change") != "updated" || got.AckedSeq != next.Seq || got.ID != c.ID ||
		!got.UpdatedAt.Equal(e.now) || !got.CreatedAt.Equal(c.CreatedAt) ||
		len(got.Filter.Watch) != 0 || strings.Join(got.Filter.Source, ",") != "pypi" {
		t.Errorf("update: %v %+v", r.Header, got)
	}
	// A dry-run update changes nothing.
	e.must(http.StatusOK, e.do("PUT", "/v1/consumers/bot?dry_run=true", tokWrite, map[string]any{}))
	if g := e.getConsumer("bot"); strings.Join(g.Filter.Source, ",") != "pypi" {
		t.Errorf("dry-run update kept: %+v", g)
	}

	// The body's name must match the path; the schema's rules apply.
	r = e.do("PUT", "/v1/consumers/bot", tokWrite, map[string]any{"name": "other"})
	wantProblem(t, r, http.StatusUnprocessableEntity, SlugValidationFailed)
	if !strings.Contains(string(r.Body), `"pointer":"/name"`) {
		t.Errorf("name mismatch: %s", r.Body)
	}
	for _, bad := range []map[string]any{
		{"start": "middle"},
		{"filter": map[string]any{"type": []any{"release.nope"}}},
		{"filter": map[string]any{"watch": []any{"a", "a"}}},
		{"filter": map[string]any{"labels": map[string]any{}}},
	} {
		wantProblem(t, e.do("PUT", "/v1/consumers/bot", tokWrite, bad), http.StatusUnprocessableEntity, SlugValidationFailed)
	}

	// Paging in name order.
	e.must(http.StatusCreated, e.do("PUT", "/v1/consumers/alpha", tokWrite, map[string]any{}))
	e.must(http.StatusCreated, e.do("PUT", "/v1/consumers/zulu", tokWrite, map[string]any{}))
	var names []string
	cursor := ""
	for range 5 {
		var page struct {
			Items      []consumerBody `json:"items"`
			NextCursor *string        `json:"next_cursor"`
		}
		r := e.must(http.StatusOK, e.do("GET", "/v1/consumers?limit=2"+cursor, tokRead, nil))
		if err := json.Unmarshal(r.Body, &page); err != nil {
			t.Fatal(err)
		}
		for _, it := range page.Items {
			names = append(names, it.Name)
		}
		if page.NextCursor == nil {
			break
		}
		cursor = "&cursor=" + *page.NextCursor
	}
	if strings.Join(names, ",") != "alpha,bot,zulu" {
		t.Errorf("list: %v", names)
	}
	wantProblem(t, e.do("GET", "/v1/consumers?cursor=eyJrIjoidyJ9", tokRead, nil), http.StatusBadRequest, SlugBadRequest)

	// Delete: dry run, then for real, then 404.
	r = e.must(http.StatusNoContent, e.do("DELETE", "/v1/consumers/bot?dry_run=true", tokWrite, nil))
	if r.Header.Get("Angelia-Change") != "deleted" {
		t.Errorf("dry-run delete: %v", r.Header)
	}
	e.getConsumer("bot")
	e.must(http.StatusNoContent, e.do("DELETE", "/v1/consumers/bot", tokWrite, nil))
	wantProblem(t, e.do("GET", "/v1/consumers/bot", tokRead, nil), http.StatusNotFound, SlugNotFound)
	wantProblem(t, e.do("DELETE", "/v1/consumers/bot", tokWrite, nil), http.StatusNotFound, SlugNotFound)
	wantProblem(t, e.do("POST", "/v1/consumers/bot/ack", tokWrite, map[string]any{"seq": 1}), http.StatusNotFound, SlugNotFound)
}

// start: latest is the head of the log, pruned events included; earliest is
// the highest pruned seq (0 on a log never pruned).
func TestConsumerStart(t *testing.T) {
	e := newEnv(t)
	e.seedConfig()
	e.must(http.StatusCreated, e.do("PUT", "/v1/consumers/empty", tokWrite, map[string]any{}))
	if c := e.getConsumer("empty"); c.AckedSeq != 0 {
		t.Errorf("latest on an empty log: %d", c.AckedSeq)
	}
	old := e.now.Add(-100 * 24 * time.Hour)
	evs := e.seedEvents(3, old)
	e.must(http.StatusCreated, e.do("PUT", "/v1/consumers/early", tokWrite, map[string]any{"start": "earliest"}))
	e.must(http.StatusCreated, e.do("PUT", "/v1/consumers/late", tokWrite, map[string]any{"start": "latest"}))
	if a, b := e.getConsumer("early").AckedSeq, e.getConsumer("late").AckedSeq; a != 0 || b != evs[2].Seq {
		t.Errorf("unpruned: earliest %d, latest %d (head %d)", a, b, evs[2].Seq)
	}
	// Every event pruned: latest is still the head, earliest the highest
	// pruned seq, and neither gets 410.
	if _, err := e.st.PruneHistory(context.Background(), e.now.Add(-90*24*time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	e.must(http.StatusCreated, e.do("PUT", "/v1/consumers/early2", tokWrite, map[string]any{"start": "earliest"}))
	e.must(http.StatusCreated, e.do("PUT", "/v1/consumers/late2", tokWrite, map[string]any{}))
	for _, name := range []string{"early2", "late2"} {
		if c := e.getConsumer(name); c.AckedSeq != evs[2].Seq {
			t.Errorf("%s on a pruned log: %d, want %d", name, c.AckedSeq, evs[2].Seq)
		}
		p := e.eventPage("/v1/events?consumer=" + name)
		if len(p.Items) != 0 || p.NextAfter != evs[2].Seq || p.HasMore {
			t.Errorf("%s: %+v", name, p)
		}
	}
}

// GET /v1/events?consumer=c is GET /v1/events?after=<acked_seq>&<filter>,
// byte for byte, next_after and has_more included; reading never acks.
func TestConsumerReadEqualsAfter(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.EventScan = 3 }) // pages with has_more and empty pages
	e.seedConfig()
	e.seedEvents(9, e.now.Add(-time.Hour))
	wid := e.watchID("requests")
	for _, tc := range []struct {
		filter map[string]any
		query  string
	}{
		{map[string]any{}, ""},
		{map[string]any{"type": []any{"release.yanked"}}, "&type=release.yanked"},
		{map[string]any{"source": []any{"pypi"}, "watch": []any{"requests"}}, "&source=pypi&watch=requests"},
		{map[string]any{"watch": []any{wid, "nope"}}, "&watch=" + wid + "&watch=nope"},
		{map[string]any{"source": []any{"npm"}}, "&source=npm"},
	} {
		e.must(http.StatusCreated, e.do("PUT", "/v1/consumers/c", tokWrite, map[string]any{"filter": tc.filter, "start": "earliest"}))
		for range 6 {
			c := e.getConsumer("c")
			for _, limit := range []string{"", "&limit=1", "&limit=2"} {
				got := e.must(http.StatusOK, e.do("GET", "/v1/events?consumer=c"+limit, tokRead, nil))
				want := e.must(http.StatusOK, e.do("GET", "/v1/events?after="+strconv.FormatInt(c.AckedSeq, 10)+tc.query+limit, tokRead, nil))
				if !bytes.Equal(got.Body, want.Body) {
					t.Fatalf("filter %v at %d%s:\n got %s\nwant %s", tc.filter, c.AckedSeq, limit, got.Body, want.Body)
				}
			}
			if after := e.getConsumer("c"); after.AckedSeq != c.AckedSeq {
				t.Fatalf("reading moved the cursor: %d -> %d", c.AckedSeq, after.AckedSeq)
			}
			p := e.eventPage("/v1/events?consumer=c&limit=1")
			e.must(http.StatusOK, e.do("POST", "/v1/consumers/c/ack", tokWrite, map[string]any{"seq": p.NextAfter}))
		}
		e.must(http.StatusNoContent, e.do("DELETE", "/v1/consumers/c", tokWrite, nil))
	}

	wantProblem(t, e.do("GET", "/v1/events?consumer=nope", tokRead, nil), http.StatusNotFound, SlugNotFound)
	e.must(http.StatusCreated, e.do("PUT", "/v1/consumers/c", tokWrite, map[string]any{}))
	for _, q := range []string{"after=0", "after=5", "watch=", "watch=requests", "type=", "type=release.published", "source=", "source=pypi"} {
		for _, path := range []string{"/v1/events", "/v1/events/stream"} {
			r := e.do("GET", path+"?consumer=c&"+q, tokRead, nil)
			wantProblem(t, r, http.StatusBadRequest, SlugBadRequest)
			if d := r.json(t)["detail"]; d != "query parameter consumer: cannot be combined with after, watch, type or source" {
				t.Errorf("%s?%s: %v", path, q, d)
			}
		}
	}
	// The exclusivity is by presence of consumer: an invalid consumer name
	// alone is an ordinary 400, and limit combines.
	wantProblem(t, e.do("GET", "/v1/events?consumer=Not_A_Name", tokRead, nil), http.StatusBadRequest, SlugBadRequest)
	e.must(http.StatusOK, e.do("GET", "/v1/events?consumer=c&limit=5", tokRead, nil))
}

// Ack by seq and by event_id; monotonic; 422 beyond the head or for an
// event the log does not hold; no Angelia-Change header; not audited.
func TestConsumerAck(t *testing.T) {
	e := newEnv(t)
	e.seedConfig()
	evs := e.seedEvents(4, e.now.Add(-time.Hour))
	e.must(http.StatusCreated, e.do("PUT", "/v1/consumers/bot", tokWrite, map[string]any{"start": "earliest"}))
	audits := len(e.auditRecords())

	e.now = e.now.Add(time.Minute)
	r := e.must(http.StatusOK, e.do("POST", "/v1/consumers/bot/ack", tokWrite, map[string]any{"seq": evs[1].Seq}))
	c := e.consumer(r)
	if c.AckedSeq != evs[1].Seq || c.AckedAt == nil || !c.AckedAt.Equal(e.now) || c.UpdatedAt.Equal(e.now) {
		t.Errorf("ack by seq: %+v", c)
	}
	if r.Header.Get("Angelia-Change") != "" {
		t.Errorf("ack sent Angelia-Change: %v", r.Header)
	}
	// An older position, by seq or by event id, is a no-op: 200 and the
	// consumer unchanged, acked_at included.
	e.now = e.now.Add(time.Minute)
	for _, body := range []map[string]any{{"seq": evs[0].Seq}, {"seq": evs[1].Seq}, {"event_id": evs[0].ID}, {"seq": 0}} {
		got := e.consumer(e.must(http.StatusOK, e.do("POST", "/v1/consumers/bot/ack", tokWrite, body)))
		if got.AckedSeq != c.AckedSeq || !got.AckedAt.Equal(*c.AckedAt) {
			t.Errorf("older ack %v moved the consumer: %+v", body, got)
		}
	}
	// By event id: up to that event's seq.
	got := e.consumer(e.must(http.StatusOK, e.do("POST", "/v1/consumers/bot/ack", tokWrite, map[string]any{"event_id": evs[2].ID})))
	if got.AckedSeq != evs[2].Seq || !got.AckedAt.Equal(e.now) {
		t.Errorf("ack by event_id: %+v", got)
	}
	// The head itself is accepted; beyond it is 422 at /seq.
	e.must(http.StatusOK, e.do("POST", "/v1/consumers/bot/ack", tokWrite, map[string]any{"seq": evs[3].Seq}))
	r = e.do("POST", "/v1/consumers/bot/ack", tokWrite, map[string]any{"seq": evs[3].Seq + 1})
	wantProblem(t, r, http.StatusUnprocessableEntity, SlugValidationFailed)
	if !strings.Contains(string(r.Body), `"pointer":"/seq"`) {
		t.Errorf("seq beyond the head: %s", r.Body)
	}
	r = e.do("POST", "/v1/consumers/bot/ack", tokWrite, map[string]any{"event_id": "01JBXC4D5E6F7G8H9J0K1M2N3P"})
	wantProblem(t, r, http.StatusUnprocessableEntity, SlugValidationFailed)
	if !strings.Contains(string(r.Body), `"pointer":"/event_id"`) {
		t.Errorf("unknown event_id: %s", r.Body)
	}
	// Exactly one of seq and event_id.
	for _, body := range []any{map[string]any{}, map[string]any{"seq": 1, "event_id": evs[0].ID}, map[string]any{"seq": -1}, map[string]any{"after": 1}} {
		wantProblem(t, e.do("POST", "/v1/consumers/bot/ack", tokWrite, body), http.StatusUnprocessableEntity, SlugValidationFailed)
	}
	if n := len(e.auditRecords()); n != audits {
		t.Errorf("acks wrote %d audit records", n-audits)
	}
	if !strings.Contains(e.log.String(), `"route":"/v1/consumers/{name}/ack"`) {
		t.Errorf("ack not in the access log")
	}
}

// PUT and DELETE are audited like the other writes; dry runs and unchanged
// PUTs are not.
func TestConsumerAudit(t *testing.T) {
	e := newEnv(t)
	e.must(http.StatusCreated, e.do("PUT", "/v1/consumers/bot?dry_run=true", tokWrite, map[string]any{}))
	e.must(http.StatusCreated, e.do("PUT", "/v1/consumers/bot", tokWrite, map[string]any{}))
	e.must(http.StatusOK, e.do("PUT", "/v1/consumers/bot", tokWrite, map[string]any{}))
	e.must(http.StatusOK, e.do("PUT", "/v1/consumers/bot", tokAdmin, map[string]any{"filter": map[string]any{"source": []any{"pypi"}}}))
	e.must(http.StatusNoContent, e.do("DELETE", "/v1/consumers/bot?dry_run=true", tokWrite, nil))
	e.must(http.StatusNoContent, e.do("DELETE", "/v1/consumers/bot", tokWrite, nil))
	recs := e.auditRecords()
	want := []struct{ action, principal string }{
		{"consumer.create", "tok-write"}, {"consumer.update", "tok-admin"}, {"consumer.delete", "tok-write"},
	}
	if len(recs) != len(want) {
		t.Fatalf("%d audit records, want %d:\n%s", len(recs), len(want), e.log.String())
	}
	for i, w := range want {
		r := recs[i]
		if r["action"] != w.action || r["principal"] != w.principal || r["consumer"] != "bot" || r["id"] == "" || r["request_id"] == nil {
			t.Errorf("record %d: %v, want %v", i, r, w)
		}
	}
}

// A consumer behind retention gets 410 with oldest_seq, at acked_seq 0
// too; acknowledging oldest_seq - 1 recovers it. Like TestEventCursorExpired.
func TestConsumerExpired(t *testing.T) {
	e, ts := streamEnv(t)
	e.seedConfig()
	wid, sid := e.watchID("requests"), e.subscriptionID("platform-ci")
	e.must(http.StatusCreated, e.do("PUT", "/v1/consumers/zero", tokWrite, map[string]any{}))
	old := e.now.Add(-100 * 24 * time.Hour)
	kept := e.appendEvent(wid, event.TypePublished, "pypi", old,
		store.Delivery{SubscriptionID: sid, Status: store.DeliveryPending, NextAttemptAt: e.now})
	e.appendEvent(wid, event.TypePublished, "pypi", old.Add(time.Second))
	pruned := e.appendEvent(wid, event.TypePublished, "pypi", old.Add(2*time.Second))
	e.must(http.StatusCreated, e.do("PUT", "/v1/consumers/bot", tokWrite, map[string]any{"start": "earliest"}))
	e.must(http.StatusOK, e.do("POST", "/v1/consumers/bot/ack", tokWrite, map[string]any{"seq": kept.Seq}))
	fresh := e.appendEvent(wid, event.TypePublished, "pypi", e.now)
	if _, err := e.st.PruneHistory(context.Background(), e.now.Add(-90*24*time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	if c := e.getConsumer("zero"); c.AckedSeq != 0 {
		t.Fatalf("zero: %+v", c)
	}
	for _, name := range []string{"zero", "bot"} {
		for _, path := range []string{"/v1/events", "/v1/events/stream"} {
			r := e.do("GET", path+"?consumer="+name, tokRead, nil)
			wantProblem(t, r, http.StatusGone, SlugCursorExpired)
			m := r.json(t)
			if m["oldest_seq"] != float64(pruned.Seq+1) {
				t.Errorf("%s %s: oldest_seq = %v, want %d", name, path, m["oldest_seq"], pruned.Seq+1)
			}
			at := strconv.FormatInt(e.getConsumer(name).AckedSeq, 10)
			if want := "consumer " + name + " is at " + at + ", but events after " + at + " were pruned; acknowledge oldest_seq - 1 to resume"; m["detail"] != want {
				t.Errorf("detail %v, want %s", m["detail"], want)
			}
			if path == "/v1/events" {
				cc := &contract{t: t, e: e, covered: map[string]bool{}}
				_, v := cc.call("listEvents", http.StatusGone, path+"?consumer="+name, tokRead, nil)
				cc.sameAsExample("ProblemConsumerCursorExpired", v, "detail", "oldest_seq")
			}
		}
	}
	// A pruned event cannot be acknowledged by id.
	r := e.do("POST", "/v1/consumers/bot/ack", tokWrite, map[string]any{"event_id": pruned.ID})
	wantProblem(t, r, http.StatusUnprocessableEntity, SlugValidationFailed)
	// oldest_seq - 1 recovers, accepting the gap.
	e.must(http.StatusOK, e.do("POST", "/v1/consumers/bot/ack", tokWrite, map[string]any{"seq": pruned.Seq}))
	p := e.eventPage("/v1/events?consumer=bot")
	if len(p.Items) != 1 || p.Items[0].Seq != fresh.Seq {
		t.Errorf("after recovery: %+v", p)
	}
	// On the stream, Last-Event-ID takes precedence over the expired
	// acked_seq, and is checked itself.
	c := openStream(t, ts.URL+"/v1/events/stream?consumer=zero", tokRead, "Last-Event-ID", strconv.FormatInt(pruned.Seq, 10))
	c.expect("retry: 5000")
	c.expectEvent(fresh.Seq)
	r = e.do("GET", "/v1/events/stream?consumer=bot", tokRead, nil, "Last-Event-ID", strconv.FormatInt(kept.Seq, 10))
	wantProblem(t, r, http.StatusGone, SlugCursorExpired)
}

// The stream starts after acked_seq with the stored filter, Last-Event-ID
// wins, it never acks, and it keeps the consumer as it was at the start.
func TestConsumerStream(t *testing.T) {
	e, ts := streamEnv(t)
	e.seedConfig()
	evs := e.seedEvents(4, e.now.Add(-time.Hour)) // published, yanked, published, yanked
	e.must(http.StatusCreated, e.do("PUT", "/v1/consumers/bot", tokWrite, map[string]any{
		"filter": map[string]any{"type": []any{"release.published"}}, "start": "earliest",
	}))
	e.must(http.StatusOK, e.do("POST", "/v1/consumers/bot/ack", tokWrite, map[string]any{"seq": evs[0].Seq}))

	c := openStream(t, ts.URL+"/v1/events/stream?consumer=bot", tokRead)
	if c.resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", c.resp.StatusCode)
	}
	c.expect("retry: 5000")
	c.expectEvent(evs[2].Seq)

	// Last-Event-ID wins over acked_seq; the filter still applies.
	c2 := openStream(t, ts.URL+"/v1/events/stream?consumer=bot", tokRead, "Last-Event-ID", strconv.FormatInt(evs[2].Seq, 10))
	c2.expect("retry: 5000")

	if g := e.getConsumer("bot"); g.AckedSeq != evs[0].Seq {
		t.Errorf("the stream moved the cursor: %+v", g)
	}

	// A later PUT and DELETE do not affect the open streams.
	e.must(http.StatusOK, e.do("PUT", "/v1/consumers/bot", tokWrite, map[string]any{"filter": map[string]any{"type": []any{"release.yanked"}}}))
	more := e.seedEvents(2, e.now) // published, yanked
	c.expectEvent(more[0].Seq)
	c2.expectEvent(more[0].Seq)
	e.must(http.StatusNoContent, e.do("DELETE", "/v1/consumers/bot", tokWrite, nil))
	more2 := e.seedEvents(1, e.now.Add(time.Minute))
	c.expectEvent(more2[0].Seq)

	// A stream that read the whole log has not acked anything either.
	e.must(http.StatusCreated, e.do("PUT", "/v1/consumers/watcher", tokWrite, map[string]any{"start": "earliest"}))
	c3 := openStream(t, ts.URL+"/v1/events/stream?consumer=watcher", tokRead)
	c3.expect("retry: 5000")
	for _, ev := range append(append(evs, more...), more2...) {
		c3.expectEvent(ev.Seq)
	}
	if g := e.getConsumer("watcher"); g.AckedSeq != 0 || g.AckedAt != nil {
		t.Errorf("the stream acked: %+v", g)
	}

	wantProblem(t, e.do("GET", "/v1/events/stream?consumer=bot", tokRead, nil), http.StatusNotFound, SlugNotFound)
}
