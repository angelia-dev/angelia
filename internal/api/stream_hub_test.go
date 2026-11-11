package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/supervisor"
)

// countingStore counts the reads of the event log and the largest page asked.
type countingStore struct {
	store.APIStore
	reads   atomic.Int64
	maxPage atomic.Int64
}

func (s *countingStore) ListEventsAfter(ctx context.Context, after int64, limit int) ([]store.Event, error) {
	s.reads.Add(1)
	for {
		m := s.maxPage.Load()
		if int64(limit) <= m || s.maxPage.CompareAndSwap(m, int64(limit)) {
			break
		}
	}
	return s.APIStore.ListEventsAfter(ctx, after, limit)
}

func (s *countingStore) HasEventsAfter(ctx context.Context, seq int64) (bool, error) {
	s.reads.Add(1)
	return s.APIStore.HasEventsAfter(ctx, seq)
}

func armed(t *testing.T, srv *Server, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		srv.hub.mu.Lock()
		got := len(srv.hub.armed)
		srv.hub.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d streams armed on the shared poller, want %d", got, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 50 idle streams cost about one store call per interval in total.
func TestEventStreamSharedPoller(t *testing.T) {
	var cs *countingStore
	e, ts := streamEnv(t, func(o *Options) {
		cs = &countingStore{APIStore: o.Store}
		o.Store = cs
		o.StreamPoll = 50 * time.Millisecond
		o.AuthRecheck = 30 * time.Second
	})
	const streams = 50
	for range streams {
		openStream(t, ts.URL+"/v1/events/stream", tokRead).expect("retry: 5000")
	}
	armed(t, e.srv, streams)
	before := cs.reads.Load()
	time.Sleep(10 * 50 * time.Millisecond) // 10 intervals
	if got := cs.reads.Load() - before; got > 25 {
		t.Errorf("%d store reads over 10 intervals for %d idle streams, want about 10 to 20", got, streams)
	}
}

// Ten streams with different filters see exactly their events, in order,
// while four writers append.
func TestEventStreamConcurrentWritersFilters(t *testing.T) {
	e, ts := streamEnv(t)
	e.seedConfig()
	e.must(http.StatusCreated, e.do("PUT", "/v1/watches/flask", tokWrite, map[string]any{
		"source": "pypi", "coordinates": map[string]any{"name": "flask"}, "interval": "30m",
	}))
	wids := []string{e.watchID("requests"), e.watchID("flask")}
	filters := []struct {
		query string
		match func(w int, typ, src string) bool
	}{
		{"", func(int, string, string) bool { return true }},
		{"type=release.published", func(_ int, typ, _ string) bool { return typ == event.TypePublished }},
		{"type=release.yanked", func(_ int, typ, _ string) bool { return typ == event.TypeYanked }},
		{"watch=requests", func(w int, _, _ string) bool { return w == 0 }},
		{"watch=flask", func(w int, _, _ string) bool { return w == 1 }},
		{"source=pypi", func(_ int, _, src string) bool { return src == "pypi" }},
		{"source=npm", func(_ int, _, src string) bool { return src == "npm" }},
		{"watch=requests&type=release.yanked", func(w int, typ, _ string) bool { return w == 0 && typ == event.TypeYanked }},
		{"type=release.published&source=npm", func(_ int, typ, src string) bool { return typ == event.TypePublished && src == "npm" }},
		{"watch=flask&source=pypi", func(w int, _, src string) bool { return w == 1 && src == "pypi" }},
	}
	clients := make([]*sseClient, len(filters))
	for i, f := range filters {
		clients[i] = openStream(t, ts.URL+"/v1/events/stream?"+f.query, tokRead)
		clients[i].expect("retry: 5000")
	}

	const writers, perWriter = 4, 125
	type appended struct {
		seq      int64
		w        int
		typ, src string
	}
	var mu sync.Mutex
	var all []appended
	var wg sync.WaitGroup
	for g := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWriter {
				n := g*perWriter + i
				w, typ, src := n%2, []string{event.TypePublished, event.TypeYanked}[(n/2)%2], []string{"pypi", "npm"}[(n/4)%2]
				ev := e.appendEvent(wids[w], typ, src, e.now.Add(time.Duration(n)*time.Millisecond))
				mu.Lock()
				all = append(all, appended{ev.Seq, w, typ, src})
				mu.Unlock()
				if i%25 == 0 {
					time.Sleep(15 * time.Millisecond) // let the streams catch up now and then
				}
			}
		}()
	}
	wg.Wait()

	for i, f := range filters {
		var want []int64
		for _, a := range all {
			if f.match(a.w, a.typ, a.src) {
				want = append(want, a.seq)
			}
		}
		slices.Sort(want)
		if len(want) == 0 {
			t.Fatalf("filter %q matches nothing: the test is vacuous", f.query)
		}
		var got []int64
		for len(got) < len(want) {
			line, closed := clients[i].next()
			if closed {
				t.Fatalf("filter %q: stream closed after %d of %d events", f.query, len(got), len(want))
			}
			if id, ok := strings.CutPrefix(line, "id: "); ok {
				seq, _ := strconv.ParseInt(id, 10, 64)
				got = append(got, seq)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("filter %q: got %d events, want %d (first difference below)\n got %v\nwant %v", f.query, len(got), len(want), head(got), head(want))
		}
		// Nothing more arrives: no duplicate, no stray event.
		select {
		case l := <-clients[i].lines:
			if strings.HasPrefix(l, "id: ") {
				t.Errorf("filter %q: extra event %q", f.query, l)
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func head(s []int64) []int64 { return s[:min(len(s), 20)] }

// A stream far behind reads bounded pages, stays alive with keep-alives, joins
// the shared poller at the head, and stops at once on a drain.
func TestEventStreamCatchUpJoinsSharedPoller(t *testing.T) {
	var cs *countingStore
	e, ts := streamEnv(t, func(o *Options) {
		cs = &countingStore{APIStore: o.Store}
		o.Store = slowStore{APIStore: cs, delay: 15 * time.Millisecond}
		o.EventScan = 500
		o.KeepAlive = 50 * time.Millisecond
	})
	e.seedConfig()
	wid := e.watchID("requests")
	const total = 20000
	payload := envelope("01JBX2A7C8D9E0F1G2H3J4K5M6", event.TypePublished, wid, "pypi", "1.0.0", e.now) // the stream does not read it for this filter
	evs := make([]store.Event, 0, total)
	for i := range total {
		id := fmt.Sprintf("%026d", i+1)
		evs = append(evs, store.Event{ID: id, WatchID: wid, Type: event.TypePublished, Payload: payload, CreatedAt: e.now})
	}
	for chunk := range slices.Chunk(evs, 1000) { // one transaction each: 20,000 single commits are slow
		err := e.st.InTx(context.Background(), func(tx store.Tx) error {
			for _, ev := range chunk {
				if err := tx.AppendEvent(context.Background(), ev, nil); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	c := openStream(t, ts.URL+"/v1/events/stream?after=0&type=release.yanked", tokRead)
	c.expect("retry: 5000")
	c.expect(": keep-alive") // while it catches up
	armed(t, e.srv, 1)       // caught up: waiting on the shared poller
	if got := cs.maxPage.Load(); got > 1000 {
		t.Errorf("a page of %d events asked of the store, want at most 1000", got)
	}
	y := e.appendEvent(wid, event.TypeYanked, "pypi", e.now)
	for {
		l, closed := c.next()
		if closed {
			t.Fatal("stream closed")
		}
		if l == "id: "+strconv.FormatInt(y.Seq, 10) {
			break
		}
	}
	start := time.Now()
	e.srv.Drain()
	for {
		if _, closed := c.next(); closed {
			break
		}
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("the stream closed %v after the drain", d)
	}
}

// A revoked token closes a stream that waits on the shared poller within
// AuthRecheck + StreamPoll.
func TestEventStreamRevokedWhileWaiting(t *testing.T) {
	e, ts := streamEnv(t, func(o *Options) {
		o.StreamPoll = 100 * time.Millisecond
		o.AuthRecheck = 300 * time.Millisecond
	})
	c := openStream(t, ts.URL+"/v1/events/stream", tokRead)
	c.expect("retry: 5000")
	armed(t, e.srv, 1)
	e.auth.revoke(tokRead)
	start := time.Now()
	c.expectClosed()
	if d := time.Since(start); d > 300*time.Millisecond+100*time.Millisecond+500*time.Millisecond {
		t.Errorf("closed %v after the revocation", d)
	}
}

type streamCounter struct{ open atomic.Int64 }

func (c *streamCounter) EventStreamOpened() { c.open.Add(1) }
func (c *streamCounter) EventStreamClosed() { c.open.Add(-1) }

func TestEventStreamLimit(t *testing.T) {
	var gauge streamCounter
	e, ts := streamEnv(t, func(o *Options) { o.MaxStreams = 2; o.Metrics = &gauge })
	dial := func(token string) (int, http.Header, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/v1/events/stream", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cancel(); _ = resp.Body.Close() })
		return resp.StatusCode, resp.Header, cancel
	}
	dial(tokRead)
	_, _, cancel2 := dial(tokRead)
	if got := gauge.open.Load(); got != 2 {
		t.Fatalf("gauge = %d, want 2", got)
	}

	// Authentication comes first: a bad token still gets 401.
	if status, _, _ := dial("nope-nope-nope-nope-nope-nope-00"); status != http.StatusUnauthorized {
		t.Errorf("bad token at the limit: %d", status)
	}
	status, hdr, _ := dial(tokRead)
	if status != http.StatusServiceUnavailable || hdr.Get("Retry-After") == "" || hdr.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("stream 3: %d %v", status, hdr)
	}
	if got := gauge.open.Load(); got != 2 {
		t.Errorf("gauge = %d after a refused stream, want 2", got)
	}
	// Other requests are not limited.
	e.must(http.StatusOK, e.do("GET", "/v1/watches", tokRead, nil))

	// Closing a stream frees its slot.
	cancel2()
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, _, _ := dial(tokRead)
		if status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the slot was not freed: %d", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Behind the supervisor's http.Server and a short WriteTimeout, a stream
// outlives the timeout many times over, and a plain request still gets it.
func TestEventStreamBehindSupervisorWriteTimeout(t *testing.T) {
	const wt = 200 * time.Millisecond
	e := newEnv(t, func(o *Options) {
		o.StreamPoll = 20 * time.Millisecond
		o.KeepAlive = 100 * time.Millisecond
		o.AuthRecheck = 30 * time.Second
		o.Store = slowWatches{APIStore: o.Store, delay: 3 * wt}
	})
	e.seedConfig()
	wid := e.watchID("requests")
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	sup := &supervisor.Supervisor{Roles: []supervisor.Role{e.srv.Role()}, Listener: ln, WriteTimeout: wt, DrainTimeout: time.Second}
	go func() { done <- sup.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("supervisor: %v", err)
		}
	})
	base := "http://" + ln.Addr().String()

	c := openStream(t, base+"/v1/events/stream", tokRead)
	c.expect("retry: 5000")
	start := time.Now()
	keepAlives, events := 0, 0
	var sent bool
	for time.Since(start) < 5*wt+wt/2 {
		if !sent && time.Since(start) > 2*wt {
			e.appendEvent(wid, event.TypePublished, "pypi", e.now)
			sent = true
		}
		select {
		case l, ok := <-c.lines:
			if !ok {
				t.Fatalf("the stream closed after %v, within 5 write timeouts", time.Since(start))
			}
			switch {
			case l == ": keep-alive":
				keepAlives++
			case strings.HasPrefix(l, "id: "):
				events++
			}
		case <-time.After(10 * time.Millisecond):
		}
	}
	if events != 1 || keepAlives < 5 {
		t.Errorf("events %d, keep-alives %d over 5 write timeouts", events, keepAlives)
	}

	// A plain request that is slower than the write timeout is cut off.
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/v1/watches", nil)
	req.Header.Set("Authorization", "Bearer "+tokRead)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Errorf("GET /v1/watches slower than the write timeout got %d", resp.StatusCode)
	}
}

// slowWatches delays the watch listing, a plain request.
type slowWatches struct {
	store.APIStore
	delay time.Duration
}

func (s slowWatches) ListNamedWatches(ctx context.Context, afterName, afterID string, n int) ([]store.Watch, error) {
	time.Sleep(s.delay)
	return s.APIStore.ListNamedWatches(ctx, afterName, afterID, n)
}
