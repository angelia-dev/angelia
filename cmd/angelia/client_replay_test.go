package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/api"
	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
	"github.com/angelia-dev/angelia/internal/ulid"
)

const (
	fixDelivery = "01JBX2A9QWERTY0123456789AB"
	fixEvent    = "01JBX2A7C8D9E0F1G2H3J4K5M6"
)

// TestReplayGolden checks the output of `replay` and of the dead-letter view
// (`deliveries list --status dead`) in both formats, and the request each
// sends.
func TestReplayGolden(t *testing.T) {
	isolate(t)
	cases := []struct {
		name   string
		route  string
		file   string
		args   []string
		uri    string
		body   string
		noBody bool
	}{
		{
			name: "deliveries-list-dead", route: "GET /v1/deliveries", file: "deliveries",
			args: []string{"deliveries", "list", "--status", "dead"}, uri: "/v1/deliveries?status=dead", noBody: true,
		},
		{
			name: "replay-delivery", route: "POST /v1/deliveries/" + fixDelivery + "/replay", file: "delivery-replayed",
			args: []string{"replay", "--delivery", fixDelivery}, uri: "/v1/deliveries/" + fixDelivery + "/replay", body: `{}`,
		},
		{
			name: "replay-event", route: "POST /v1/replays", file: "replay-result",
			args: []string{"replay", "--event", fixEvent}, uri: "/v1/replays", body: `{"event":"` + fixEvent + `"}`,
		},
		{
			name: "replay-dry-run", route: "POST /v1/replays", file: "replay-dry-run",
			args: []string{"replay", "--subscription", "platform-ci", "--since", "2026-11-02T01:00:00+01:00", "--until", "2026-11-03T00:00:00.123456Z", "--status", "dead,delivered", "--dry-run"},
			uri:  "/v1/replays", body: `{"dry_run":true,"since":"2026-11-02T00:00:00Z","status":["dead","delivered"],"subscription":"platform-ci","until":"2026-11-03T00:00:00.123Z"}`,
		},
		{
			name: "replay-none", route: "POST /v1/replays", file: "replay-none",
			args: []string{"replay", "--event", fixEvent}, uri: "/v1/replays", body: `{"event":"` + fixEvent + `"}`,
		},
	}
	for _, c := range cases {
		for _, format := range []string{formatTable, formatJSON} {
			t.Run(c.name+"."+format, func(t *testing.T) {
				srv := newFixtureServer(t, map[string]string{c.route: c.file})
				args := append(append([]string{}, c.args...), "--server", srv.URL, "--token", testToken, "-o", format)
				out, _, err := runCLI(t, args...)
				if err != nil {
					t.Fatal(err)
				}
				golden(t, c.name+"."+format, out)
				req := srv.last()
				if req.URI != c.uri {
					t.Errorf("request %s, want %s", req.URI, c.uri)
				}
				if c.noBody {
					return
				}
				if req.ContentType != jsonContentType {
					t.Errorf("Content-Type %q", req.ContentType)
				}
				if got := compactJSON(t, req.Body); got != c.body {
					t.Errorf("body %s\nwant %s", got, c.body)
				}
			})
		}
	}
}

func compactJSON(t *testing.T, s string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("body %q: %v", s, err)
	}
	b, _ := json.Marshal(v) // keys sorted
	return string(b)
}

// TestReplayFlagErrors: a selector that the API would refuse, or that does not
// say what to replay, fails before any request.
func TestReplayFlagErrors(t *testing.T) {
	isolate(t)
	srv := newFixtureServer(t, nil)
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, "give --delivery"},
		{[]string{"--delivery", fixDelivery, "--event", fixEvent}, "--delivery cannot be combined with --event"},
		{[]string{"--delivery", fixDelivery, "--dry-run"}, "--delivery cannot be combined with --dry-run"},
		{[]string{"--delivery", fixDelivery, "--status", "dead"}, "--delivery cannot be combined with --status"},
		{[]string{"--since", "24h"}, "--since and --until need --subscription"},
		{[]string{"--event", fixEvent, "--until", "now"}, "--since and --until need --subscription"},
		{[]string{"--subscription", "platform-ci"}, "--subscription needs --since"},
		{[]string{"--subscription", "platform-ci", "--until", "now", "--event", fixEvent}, "--until needs --since"},
		{[]string{"--subscription", "platform-ci", "--since", "yesterday"}, "--since"},
		{[]string{"--subscription", "platform-ci", "--since", "1h", "--until", "2h"}, "--until must be after --since"},
		{[]string{"--event", fixEvent, "--status", "pending"}, `--status: "pending"`},
		{[]string{"--delivery", ""}, "give --delivery"},
		{[]string{"extra-arg", "--event", fixEvent}, "unknown command"},
	} {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			args := append(append([]string{"replay"}, c.args...), "--server", srv.URL)
			_, _, err := runCLI(t, args...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
	if len(srv.reqs) != 0 {
		t.Errorf("sent %d requests", len(srv.reqs))
	}
}

func TestParseReplayTime(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 987654321, time.UTC)
	for in, want := range map[string]string{
		"now":                            "2026-11-05T12:00:00.987Z",
		"24h":                            "2026-11-04T12:00:00.987Z",
		"90m":                            "2026-11-05T10:30:00.987Z",
		"7d":                             "2026-10-29T12:00:00.987Z",
		"2026-11-02T00:00:00Z":           "2026-11-02T00:00:00.000Z",
		"2026-11-02T02:30:00.5+02:00":    "2026-11-02T00:30:00.500Z",
		"2026-11-02T00:00:00.123999999Z": "2026-11-02T00:00:00.123Z",
	} {
		got, err := parseReplayTime(in, now)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if s := fmtTime(got); s != want {
			t.Errorf("%s = %s, want %s", in, s, want)
		}
		if got.Location() != time.UTC {
			t.Errorf("%s: not UTC", in)
		}
	}
	for _, in := range []string{"", "yesterday", "-1h", "0s", "0d", "-2d", "1.5d", "2026-11-02", "2026-11-02 00:00:00"} {
		if _, err := parseReplayTime(in, now); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

// A relative --since ends at now when --until is not given.
func TestReplayRelativeWindow(t *testing.T) {
	isolate(t)
	srv := newFixtureServer(t, map[string]string{"POST /v1/replays": "replay-none"})
	before := time.Now().UTC().Truncate(time.Millisecond)
	if _, _, err := runCLI(t, "replay", "--subscription", "platform-ci", "--since", "24h", "--server", srv.URL); err != nil {
		t.Fatal(err)
	}
	var body struct{ Since, Until time.Time }
	if err := json.Unmarshal([]byte(srv.last().Body), &body); err != nil {
		t.Fatal(err)
	}
	if body.Until.Before(before) || time.Since(body.Until) > time.Minute {
		t.Errorf("until %s is not now", body.Until)
	}
	if d := body.Until.Sub(body.Since); d != 24*time.Hour {
		t.Errorf("window %s, want 24h", d)
	}
}

func TestReplayNotReplayable(t *testing.T) {
	isolate(t)
	srv := newFixtureServer(t, map[string]string{"POST /v1/deliveries/" + fixDelivery + "/replay": "problem-not-replayable"})
	out, _, err := runCLI(t, "replay", "--delivery", fixDelivery, "--server", srv.URL)
	if err == nil || !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), "only dead or delivered") {
		t.Errorf("err = %v", err)
	}
	if out != "" {
		t.Errorf("stdout %q", out)
	}
}

// replayE2E is the real API handler on a SQLite store.
type replayE2E struct {
	srv *httptest.Server
	st  *sqlite.Store
}

// logBuffer is a log sink safe for concurrent writes.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// newReplayE2E runs the handler with its log (access and audit) kept.
func newReplayE2E(t *testing.T) (*replayE2E, *logBuffer) {
	t.Helper()
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "api.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sources, err := watchSources(&config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	lb := &logBuffer{}
	h, err := api.New(api.Options{
		Store: st, Auth: e2eAuth{}, Sources: sources, Plugins: builtinPlugins(sources.Kinds()),
		API: config.API{AllowedEnv: []string{"CI_WEBHOOK_SECRET"}}, Logger: slog.New(slog.NewJSONHandler(lb, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &replayE2E{srv: srv, st: st}, lb
}

// deadDelivery stores an event at time at with one dead delivery to the
// subscription and returns the event and delivery ids.
func (r *replayE2E) deadDelivery(t *testing.T, subName string, at time.Time) (string, string) {
	t.Helper()
	ctx := context.Background()
	sub, err := r.st.SubscriptionByName(ctx, subName)
	if err != nil {
		t.Fatal(err)
	}
	w, err := r.st.WatchByName(ctx, "requests")
	if err != nil {
		t.Fatal(err)
	}
	evID, dID := ulid.New(at), ulid.New(at)
	payload := fmt.Sprintf(`{"specversion":"1.0","id":%q,"type":"dev.angelia.release.published.v1","time":%q,"data":{"package":"requests","version":"2.33.0"}}`,
		evID, at.UTC().Format("2006-01-02T15:04:05.000Z"))
	ev := store.Event{ID: evID, WatchID: w.ID, Type: "dev.angelia.release.published.v1", Payload: []byte(payload), CreatedAt: at}
	d := store.Delivery{ID: dID, EventID: evID, SubscriptionID: sub.ID, Status: store.DeliveryDead, Attempts: 5, LastError: "webhook: unexpected status 500"}
	if err := r.st.AppendEvent(ctx, ev, []store.Delivery{d}); err != nil {
		t.Fatal(err)
	}
	return evID, dID
}

// auditLines returns the audit records of the server log.
func auditLines(t *testing.T, log string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(log), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["audit"] == true {
			out = append(out, m)
		}
	}
	return out
}

// TestReplayAgainstAPI drives the dead-letter view and every replay form
// against the real handler, and checks the audit records the server writes.
func TestReplayAgainstAPI(t *testing.T) {
	isolate(t)
	r, serverLog := newReplayE2E(t)
	conn := []string{"--server", r.srv.URL, "--token", e2eAdmin}
	must := func(args ...string) string {
		t.Helper()
		out, errOut, err := runCLI(t, append(append([]string{}, args...), conn...)...)
		if err != nil {
			t.Fatalf("angelia %s: %v\nstdout: %s\nstderr: %s", strings.Join(args, " "), err, out, errOut)
		}
		return out
	}
	sink := writeTemp(t, "sink.yaml", "kind: webhook\nconfig:\n  url: https://ci.example.com/hooks/angelia\n  secret_env: CI_WEBHOOK_SECRET\n")
	watch := writeTemp(t, "watch.json", `{"source":"pypi","coordinates":{"name":"requests"}}`)
	sub := writeTemp(t, "sub.yaml", "sink: ci-webhook\n")
	must("sink", "put", "ci-webhook", "-f", sink)
	must("watch", "put", "requests", "-f", watch)
	must("sub", "put", "platform-ci", "-f", sub)

	base := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	ev1, d1 := r.deadDelivery(t, "platform-ci", base)
	ev2, d2 := r.deadDelivery(t, "platform-ci", base.Add(time.Hour))
	_, d3 := r.deadDelivery(t, "platform-ci", base.Add(48*time.Hour))

	// The dead-letter view lists the three.
	out := must("deliveries", "list", "--status", "dead")
	for _, id := range []string{d1, d2, d3} {
		if !strings.Contains(out, id) {
			t.Errorf("dead list lacks %s:\n%s", id, out)
		}
	}

	// One delivery: queued again, same id, attempts kept.
	var one apiclientReplay
	if err := json.Unmarshal([]byte(must("replay", "--delivery", d1, "-o", "json")), &one); err != nil {
		t.Fatal(err)
	}
	if one.Replayed != 1 || len(one.Deliveries) != 1 || one.Deliveries[0].ID != d1 || one.Deliveries[0].Status != "pending" || one.Deliveries[0].Attempts != 5 {
		t.Errorf("replay --delivery: %+v", one)
	}
	if one.Deliveries[0].IdempotencyKey != ev1+":"+one.Deliveries[0].SubscriptionID {
		t.Errorf("idempotency key %q", one.Deliveries[0].IdempotencyKey)
	}
	// Again: it is pending now, so not replayable.
	if _, _, err := runCLI(t, append([]string{"replay", "--delivery", d1}, conn...)...); err == nil || !strings.Contains(err.Error(), "409") {
		t.Errorf("replay of a pending delivery: %v", err)
	}

	// A window: the dry run lists d2 and writes nothing.
	out = must("replay", "--subscription", "platform-ci", "--since", "2026-11-02T00:00:00Z", "--until", "2026-11-03T00:00:00Z", "--dry-run")
	if !strings.HasPrefix(out, "1 delivery would be queued again (dry run)") || !strings.Contains(out, d2) || strings.Contains(out, d3) {
		t.Errorf("dry run:\n%s", out)
	}
	if out := must("deliveries", "list", "--status", "dead", "--event", ev2); !strings.Contains(out, d2) {
		t.Errorf("the dry run replayed:\n%s", out)
	}
	out = must("replay", "--subscription", "platform-ci", "--since", "2026-11-02T00:00:00Z", "--until", "2026-11-03T00:00:00Z")
	if !strings.HasPrefix(out, "1 delivery queued again\n") || !strings.Contains(out, d2) {
		t.Errorf("window:\n%s", out)
	}

	// --until defaults to now.
	out = must("replay", "--subscription", "platform-ci", "--since", "2026-11-04T00:00:00Z")
	if !strings.HasPrefix(out, "1 delivery queued again\n") || !strings.Contains(out, d3) {
		t.Errorf("window to now:\n%s", out)
	}
	// By event: ev1's delivery is pending since the first replay.
	out = must("replay", "--event", ev1)
	if !strings.HasPrefix(out, "no delivery matched; nothing was queued") {
		t.Errorf("replay of a pending event:\n%s", out)
	}
	if out := must("deliveries", "list", "--status", "dead"); strings.Count(out, "\n") != 1 {
		t.Errorf("deliveries still dead:\n%s", out)
	}

	// A read token cannot replay.
	if _, _, err := runCLI(t, "replay", "--event", ev1, "--server", r.srv.URL, "--token", e2eRead); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("read token: %v", err)
	}

	// The audit trail: who, what and how many for each replay that ran,
	// none for the dry run or the refused requests.
	recs := auditLines(t, serverLog.String())
	if len(recs) != 4 {
		t.Fatalf("%d audit records, want 4:\n%s", len(recs), serverLog.String())
	}
	for _, rec := range recs {
		if rec["action"] != "replay" || rec["principal"] != "admin" || rec["request_id"] == "" {
			t.Errorf("audit record: %v", rec)
		}
	}
	if recs[0]["delivery"] != d1 || recs[0]["deliveries"] != float64(1) {
		t.Errorf("audit of replay --delivery: %v", recs[0])
	}
	if recs[1]["subscription"] != "platform-ci" || recs[1]["since"] != "2026-11-02T00:00:00Z" || recs[1]["deliveries"] != float64(1) ||
		fmt.Sprint(recs[1]["ids"]) != "["+d2+"]" {
		t.Errorf("audit of the window replay: %v", recs[1])
	}
	if recs[3]["event"] != ev1 || recs[3]["deliveries"] != float64(0) {
		t.Errorf("audit of replay --event: %v", recs[3])
	}
}

// apiclientReplay is the JSON output of `replay`, decoded loosely.
type apiclientReplay struct {
	Replayed   int  `json:"replayed"`
	DryRun     bool `json:"dry_run"`
	Truncated  bool `json:"truncated"`
	Deliveries []struct {
		ID             string `json:"id"`
		SubscriptionID string `json:"subscription_id"`
		IdempotencyKey string `json:"idempotency_key"`
		Status         string `json:"status"`
		Attempts       int    `json:"attempts"`
	} `json:"deliveries"`
}
