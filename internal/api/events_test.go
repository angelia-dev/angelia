package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/store"
)

type eventPage struct {
	Items []struct {
		Seq   int64           `json:"seq"`
		Event json.RawMessage `json:"event"`
	} `json:"items"`
	NextAfter int64 `json:"next_after"`
	HasMore   bool  `json:"has_more"`
}

func (e *env) eventPage(path string) eventPage {
	e.t.Helper()
	r := e.must(http.StatusOK, e.do("GET", path, tokRead, nil))
	var p eventPage
	if err := json.Unmarshal(r.Body, &p); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// TestEventPaginationConcurrentWriters pages through the log with a small
// limit while writers append: every event is seen once, in seq order, and
// none is skipped (acceptance criterion 2).
func TestEventPaginationConcurrentWriters(t *testing.T) {
	e := newEnv(t)
	e.seedConfig()
	wid := e.watchID("requests")
	const writers, perWriter = 4, 50

	var wg sync.WaitGroup
	done := make(chan struct{})
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWriter {
				e.appendEvent(wid, event.TypePublished, "pypi", e.now.Add(time.Duration(w*perWriter+i)*time.Millisecond))
			}
		}()
	}
	go func() { wg.Wait(); close(done) }()

	var seen []int64
	after := int64(0)
	finished := false
	for {
		select {
		case <-done:
			finished = true
		default:
		}
		p := e.eventPage("/v1/events?limit=7&after=" + strconv.FormatInt(after, 10))
		for _, it := range p.Items {
			seen = append(seen, it.Seq)
		}
		if p.NextAfter < after {
			t.Fatalf("next_after went back: %d < %d", p.NextAfter, after)
		}
		after = p.NextAfter
		// Stop only after a full drain that started once every writer was done.
		if finished && !p.HasMore {
			break
		}
	}
	if len(seen) != writers*perWriter {
		t.Fatalf("saw %d events, want %d", len(seen), writers*perWriter)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Fatalf("seq %d after %d", seen[i], seen[i-1])
		}
	}
}

func TestEventFilters(t *testing.T) {
	e := newEnv(t)
	e.seedConfig()
	wid := e.watchID("requests")
	a := e.appendEvent(wid, event.TypePublished, "pypi", e.now)
	b := e.appendEvent(wid, event.TypeYanked, "pypi", e.now.Add(time.Second))
	c := e.appendEvent(wid, event.TypePublished, "npm", e.now.Add(2*time.Second))

	for _, tc := range []struct {
		query string
		want  []int64
	}{
		{"", []int64{a.Seq, b.Seq, c.Seq}},
		{"type=release.yanked", []int64{b.Seq}},
		{"type=release.published&type=release.yanked&source=pypi", []int64{a.Seq, b.Seq}},
		{"watch=requests&source=npm", []int64{c.Seq}},
		{"watch=" + wid + "&type=release.yanked", []int64{b.Seq}},
		{"watch=nothing", nil},
	} {
		p := e.eventPage("/v1/events?" + tc.query)
		var got []int64
		for _, it := range p.Items {
			got = append(got, it.Seq)
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s: got %v, want %v", tc.query, got, tc.want)
		}
		// The cursor moves past every event read, matching or not.
		if p.NextAfter != c.Seq || p.HasMore {
			t.Errorf("%s: next_after %d has_more %v", tc.query, p.NextAfter, p.HasMore)
		}
	}
	// A limit stops at the last returned event.
	p := e.eventPage("/v1/events?limit=1")
	if len(p.Items) != 1 || p.NextAfter != a.Seq || !p.HasMore {
		t.Errorf("limit=1: %+v", p)
	}
	// Past the end: an empty page that keeps the cursor.
	p = e.eventPage("/v1/events?after=" + strconv.FormatInt(c.Seq, 10))
	if len(p.Items) != 0 || p.NextAfter != c.Seq || p.HasMore {
		t.Errorf("past the end: %+v", p)
	}
	r := e.do("GET", "/v1/events?type=release.nope", tokRead, nil)
	if r.Status != http.StatusBadRequest && r.Status != http.StatusUnprocessableEntity {
		t.Errorf("unknown type: %d %s", r.Status, r.Body)
	}
	e.must(http.StatusNotFound, e.do("GET", "/v1/events/01JBX2A7C8D9E0F1G2H3J4K5M6", tokRead, nil))
}

// A scan budget bounds the work of one request: a page may be empty while
// has_more is true, and the cursor still moves.
func TestEventScanBudget(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.EventScan = 3 })
	e.seedConfig()
	wid := e.watchID("requests")
	var last int64
	for i := range 5 {
		last = e.appendEvent(wid, event.TypePublished, "pypi", e.now.Add(time.Duration(i)*time.Second)).Seq
	}
	p := e.eventPage("/v1/events?limit=1&type=release.yanked")
	if len(p.Items) != 0 || !p.HasMore || p.NextAfter == 0 || p.NextAfter == last {
		t.Fatalf("first page: %+v", p)
	}
	p = e.eventPage("/v1/events?limit=1&type=release.yanked&after=" + strconv.FormatInt(p.NextAfter, 10))
	if len(p.Items) != 0 || p.HasMore || p.NextAfter != last {
		t.Fatalf("second page: %+v", p)
	}
}

// A non-zero cursor below the highest pruned seq gets 410 with oldest_seq
// past every pruned event, even while an older event is kept for a pending
// delivery (spec docs/api.md decision 12).
func TestEventCursorExpired(t *testing.T) {
	e := newEnv(t)
	e.seedConfig()
	wid, sid := e.watchID("requests"), e.subscriptionID("platform-ci")
	old := e.now.Add(-100 * 24 * time.Hour)
	kept := e.appendEvent(wid, event.TypePublished, "pypi", old,
		store.Delivery{SubscriptionID: sid, Status: store.DeliveryPending, NextAttemptAt: e.now})
	e.appendEvent(wid, event.TypePublished, "pypi", old.Add(time.Second))
	pruned := e.appendEvent(wid, event.TypePublished, "pypi", old.Add(2*time.Second))
	fresh := e.appendEvent(wid, event.TypePublished, "pypi", e.now)
	if _, err := e.st.PruneHistory(context.Background(), e.now.Add(-90*24*time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/events", "/v1/events/stream"} {
		r := e.do("GET", path+"?after="+strconv.FormatInt(kept.Seq, 10), tokRead, nil)
		if r.Status != http.StatusGone || r.problemType(t) != SlugCursorExpired {
			t.Fatalf("%s: %d %s", path, r.Status, r.Body)
		}
		if got := r.json(t)["oldest_seq"]; got != float64(pruned.Seq+1) {
			t.Errorf("%s: oldest_seq = %v, want %d", path, got, pruned.Seq+1)
		}
	}
	p := e.eventPage("/v1/events?after=" + strconv.FormatInt(pruned.Seq, 10))
	if len(p.Items) != 1 || p.Items[0].Seq != fresh.Seq {
		t.Errorf("after the highest pruned: %+v", p)
	}
	p = e.eventPage("/v1/events")
	if len(p.Items) != 2 || p.Items[0].Seq != kept.Seq {
		t.Errorf("after=0 starts at the oldest kept: %+v", p)
	}
}

// sseClient reads server-sent events from a live server.
type sseClient struct {
	t     *testing.T
	resp  *http.Response
	lines chan string
}

func openStream(t *testing.T, url, token string, hdr ...string) *sseClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	c := &sseClient{t: t, resp: resp, lines: make(chan string, 100)}
	go func() {
		defer close(c.lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			c.lines <- sc.Text()
		}
	}()
	return c
}

// next returns the next non-empty line; closed is true at the end of the
// stream.
func (c *sseClient) next() (line string, closed bool) {
	c.t.Helper()
	for {
		select {
		case l, ok := <-c.lines:
			if !ok {
				return "", true
			}
			if l != "" {
				return l, false
			}
		case <-time.After(5 * time.Second):
			c.t.Fatal("no line from the stream")
		}
	}
}

func (c *sseClient) expect(want string) {
	c.t.Helper()
	got, closed := c.next()
	if closed || got != want {
		c.t.Fatalf("got %q (closed %v), want %q", got, closed, want)
	}
}

func (c *sseClient) expectEvent(seq int64) {
	c.t.Helper()
	c.expect("id: " + strconv.FormatInt(seq, 10))
	data, _ := c.next()
	var rec struct {
		Seq   int64          `json:"seq"`
		Event map[string]any `json:"event"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(data, "data: ")), &rec); err != nil || rec.Seq != seq || rec.Event["id"] == nil {
		c.t.Fatalf("data %q: %v", data, err)
	}
}

func (c *sseClient) expectClosed() {
	c.t.Helper()
	if l, closed := c.next(); !closed {
		c.t.Fatalf("stream still open: %q", l)
	}
}

func streamEnv(t *testing.T, mod ...func(*Options)) (*env, *httptest.Server) {
	t.Helper()
	fast := func(o *Options) {
		o.StreamPoll = 10 * time.Millisecond
		o.KeepAlive = time.Hour
		o.AuthRecheck = 10 * time.Millisecond
	}
	e := newEnv(t, append([]func(*Options){fast}, mod...)...)
	ts := httptest.NewServer(e.srv)
	t.Cleanup(ts.Close)
	return e, ts
}

func TestEventStream(t *testing.T) {
	e, ts := streamEnv(t)
	e.seedConfig()
	wid := e.watchID("requests")
	a := e.appendEvent(wid, event.TypePublished, "pypi", e.now)
	b := e.appendEvent(wid, event.TypeYanked, "pypi", e.now.Add(time.Second))

	c := openStream(t, ts.URL+"/v1/events/stream", tokRead)
	if ct := c.resp.Header.Get("Content-Type"); c.resp.StatusCode != 200 || ct != "text/event-stream" {
		t.Fatalf("%d %q", c.resp.StatusCode, ct)
	}
	c.expect("retry: 5000")
	c.expectEvent(a.Seq)
	c.expectEvent(b.Seq)
	n := e.appendEvent(wid, event.TypePublished, "pypi", e.now.Add(2*time.Second))
	c.expectEvent(n.Seq)

	// Last-Event-ID wins over after; the filter applies.
	c2 := openStream(t, ts.URL+"/v1/events/stream?after=0&type=release.published", tokRead, "Last-Event-ID", strconv.FormatInt(a.Seq, 10))
	c2.expect("retry: 5000")
	c2.expectEvent(n.Seq)

	// A revoked token closes the stream.
	e.auth.revoke(tokRead)
	c.expectClosed()
	c2.expectClosed()
}

func TestEventStreamKeepAliveAndDrain(t *testing.T) {
	e, ts := streamEnv(t, func(o *Options) { o.KeepAlive = 20 * time.Millisecond })
	c := openStream(t, ts.URL+"/v1/events/stream", tokRead)
	c.expect("retry: 5000")
	c.expect(": keep-alive")
	e.srv.Drain()
	c.expectClosed()
	r := e.do("GET", "/v1/watches", tokRead, nil)
	if r.Status != http.StatusServiceUnavailable || r.Header.Get("Retry-After") == "" || r.problemType(t) != SlugUnavailable {
		t.Fatalf("draining: %d %v %s", r.Status, r.Header, r.Body)
	}
}
