package apiclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const problemPrefix = "https://github.com/angelia-dev/spec/blob/main/docs/api.md#problem-"

func TestNewNeedsServer(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("no server accepted")
	}
}

func TestBearerAndUserAgent(t *testing.T) {
	var gotAuth, gotUA, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotUA, gotPath = r.Header.Get("Authorization"), r.Header.Get("User-Agent"), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"next_cursor":null}`))
	}))
	defer srv.Close()

	a, err := New(Options{Server: srv.URL + "/base", Token: "tok-123", UserAgent: "test-agent"})
	if err != nil {
		t.Fatal(err)
	}
	var page WatchPage
	resp, err := a.ListWatches(t.Context(), &ListWatchesParams{}) //nolint:bodyclose // Decode closes it
	if _, err := Decode(resp, err, &page); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer tok-123" || gotUA != "test-agent" || gotPath != "/base/v1/watches" {
		t.Errorf("auth %q, agent %q, path %q", gotAuth, gotUA, gotPath)
	}

	a, _ = New(Options{Server: srv.URL})
	resp, err = a.ListWatches(t.Context(), &ListWatchesParams{}) //nolint:bodyclose // Decode closes it
	if _, err := Decode(resp, err, &page); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "" {
		t.Errorf("a client without token sent %q", gotAuth)
	}
}

func TestDecodeErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		ctype  string
		body   string
		want   []string
		typ    string
	}{
		{
			"problem", 404, "application/problem+json", `{"type":"` + problemPrefix + `not-found","title":"Not found","status":404,"detail":"no watch named x"}`,
			[]string{"404", "Not found", "(not-found)", "no watch named x"},
			"not-found",
		},
		{
			"validation errors", 422, "application/problem+json", `{"type":"` + problemPrefix + `validation-failed","title":"Validation failed","status":422,"errors":[{"pointer":"/interval","detail":"too short"},{"pointer":"","detail":"bad body"}]}`,
			[]string{"422", "/interval: too short", "- bad body"},
			"validation-failed",
		},
		{"not a problem", 502, "text/html", "<html>bad gateway</html>", []string{"502", "bad gateway"}, ""},
		{"empty", 500, "", "", []string{"500 Internal Server Error"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if c.ctype != "" {
					w.Header().Set("Content-Type", c.ctype)
				}
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			a, _ := New(Options{Server: srv.URL})
			resp, err := a.GetWatch(t.Context(), "x") //nolint:bodyclose // Decode closes it
			_, err = Decode(resp, err, new(Watch))
			var ae *Error
			if !errors.As(err, &ae) {
				t.Fatalf("err = %v", err)
			}
			if ae.Status != c.status || ae.Type() != c.typ {
				t.Errorf("status %d type %q", ae.Status, ae.Type())
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("%q lacks %q", err, w)
				}
			}
		})
	}
}

func TestDecodeChangeHeaderAndNoContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Angelia-Change", "deleted")
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Angelia-Change", "created")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"name":"x"}`))
	}))
	defer srv.Close()
	a, _ := New(Options{Server: srv.URL})

	resp, err := a.DeleteWatch(t.Context(), "x", &DeleteWatchParams{}) //nolint:bodyclose // Decode closes it
	res, err := Decode(resp, err, nil)
	if err != nil || res.Change != "deleted" {
		t.Errorf("delete: %v, %v", res, err)
	}
	resp, err = a.PutWatchWithBody(t.Context(), "x", &PutWatchParams{}, "application/json", strings.NewReader(`{}`)) //nolint:bodyclose // Decode closes it
	var w Watch
	res, err = Decode(resp, err, &w)
	if err != nil || res.Change != "created" || w.Name != "x" {
		t.Errorf("put: %v, %v, %+v", res, err, w)
	}
}

func TestDecodeBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{")) }))
	defer srv.Close()
	a, _ := New(Options{Server: srv.URL})
	resp, err := a.GetWatch(t.Context(), "x") //nolint:bodyclose // Decode closes it
	if _, err := Decode(resp, err, new(Watch)); err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Errorf("err = %v", err)
	}
}

func record(seq int64) string {
	return fmt.Sprintf("id: %d\ndata: {\"seq\":%d,\"ref_event_id\":null,\"event\":{\"id\":\"e%d\"}}\n\n", seq, seq, seq)
}

func TestStreamParsesMessages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "retry: 5000\n\n: keep-alive\n\n"+record(1)+": hi\n\n"+record(3))
	}))
	defer srv.Close()
	a, _ := New(Options{Server: srv.URL})
	var seqs []int64
	err := a.Stream(t.Context(), StreamEventsParams{}, StreamOptions{}, func(r EventRecord) error {
		seqs = append(seqs, r.Seq)
		if !strings.Contains(string(r.Event), `"id":"e`) {
			t.Errorf("event = %s", r.Event)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(seqs) != "[1 3]" {
		t.Errorf("seqs = %v", seqs)
	}
}

func TestStreamReconnectsWithLastEventID(t *testing.T) {
	var mu sync.Mutex
	var lastIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		lastIDs = append(lastIDs, r.Header.Get("Last-Event-ID"))
		n := len(lastIDs)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		switch n {
		case 1:
			_, _ = fmt.Fprint(w, "retry: 10\n\n"+record(5)+record(6))
		default:
			_, _ = fmt.Fprint(w, record(7))
		}
	}))
	defer srv.Close()
	a, _ := New(Options{Server: srv.URL})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var seqs []int64
	err := a.Stream(ctx, StreamEventsParams{}, StreamOptions{Reconnect: true, RetryDelay: time.Millisecond}, func(r EventRecord) error {
		seqs = append(seqs, r.Seq)
		if r.Seq == 7 {
			cancel()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(seqs) != "[5 6 7]" {
		t.Errorf("seqs = %v", seqs)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lastIDs) < 2 || lastIDs[0] != "" || lastIDs[1] != "6" {
		t.Errorf("Last-Event-ID per connection = %q", lastIDs)
	}
}

func TestStreamHonoursRetryHint(t *testing.T) {
	var mu sync.Mutex
	var times []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		times = append(times, time.Now())
		n := len(times)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "retry: 150\n\n"+record(int64(n)))
	}))
	defer srv.Close()
	a, _ := New(Options{Server: srv.URL})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	n := 0
	err := a.Stream(ctx, StreamEventsParams{}, StreamOptions{Reconnect: true}, func(EventRecord) error {
		if n++; n == 2 {
			cancel()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(times) < 2 || times[1].Sub(times[0]) < 140*time.Millisecond {
		t.Errorf("reconnected too early: %v", times)
	}
}

func TestStreamDoesNotRetryRefusals(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusGone)
		_, _ = fmt.Fprint(w, `{"type":"`+problemPrefix+`cursor-expired","title":"Cursor expired","status":410,"oldest_seq":9}`)
	}))
	defer srv.Close()
	a, _ := New(Options{Server: srv.URL})
	err := a.Stream(t.Context(), StreamEventsParams{}, StreamOptions{Reconnect: true, RetryDelay: time.Millisecond}, func(EventRecord) error { return nil })
	var ae *Error
	if !errors.As(err, &ae) || ae.Status != 410 || ae.Type() != "cursor-expired" || ae.Problem.OldestSeq == nil || *ae.Problem.OldestSeq != 9 {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("%d requests", calls.Load())
	}
}

func TestStreamStopsOnCallbackError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, record(1)+record(2))
	}))
	defer srv.Close()
	a, _ := New(Options{Server: srv.URL})
	boom := errors.New("boom")
	calls := 0
	err := a.Stream(t.Context(), StreamEventsParams{}, StreamOptions{Reconnect: true, RetryDelay: time.Millisecond}, func(EventRecord) error {
		calls++
		return boom
	})
	if !errors.Is(err, boom) || calls != 1 {
		t.Errorf("err = %v after %d calls", err, calls)
	}
}

func TestStreamBadData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "id: 1\ndata: {not json\n\n")
	}))
	defer srv.Close()
	a, _ := New(Options{Server: srv.URL})
	err := a.Stream(t.Context(), StreamEventsParams{}, StreamOptions{}, func(EventRecord) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "decode stream event") {
		t.Errorf("err = %v", err)
	}
}

func TestStreamSendsAfterAndFilters(t *testing.T) {
	var uriV atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { uriV.Store(r.URL.RequestURI()) }))
	defer srv.Close()
	a, _ := New(Options{Server: srv.URL})
	after := int64(42)
	_ = a.Stream(t.Context(), StreamEventsParams{After: &after, Watch: &[]string{"requests"}}, StreamOptions{}, func(EventRecord) error { return nil })
	if uri, _ := uriV.Load().(string); uri != "/v1/events/stream?after=42&watch=requests" {
		t.Errorf("uri = %s", uri)
	}
}
