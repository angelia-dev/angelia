package telemetry_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/angelia-dev/angelia/internal/dispatch"
	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/scheduler"
	"github.com/angelia-dev/angelia/internal/sink"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
	"github.com/angelia-dev/angelia/internal/telemetry/telemetrytest"
	"github.com/angelia-dev/angelia/internal/ulid"
)

// Secrets that must never reach a span. The sink URL carries one in its path
// (as chat webhooks do), the sink config another, and the failing sink quotes
// both in its error.
const (
	pathSecret   = "PATH-SECRET-9f3a"   //nolint:gosec // synthetic test value
	configSecret = "CONFIG-SECRET-77c1" //nolint:gosec // synthetic test value
)

type secretSink struct {
	calls atomic.Int32
	done  chan struct{}
}

func (*secretSink) Kind() string               { return "tracetest" }
func (*secretSink) Validate(sink.Config) error { return nil }

// Deliver fails the first attempt with an error that quotes the secrets, so
// the test covers the error path as well as the success path.
func (s *secretSink) Deliver(_ context.Context, req sink.Request) (sink.Result, error) {
	if s.calls.Add(1) == 1 {
		return sink.Result{}, fmt.Errorf("post https://hooks.example/%s?token=%s: %s: boom", pathSecret, configSecret, req.Config)
	}
	close(s.done)
	return sink.Result{StatusCode: 204}, nil
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// ACs: one trace per check, linked to delivery spans; no secrets in span
// attributes. The real scheduler, event.Commit, dispatcher and store run end
// to end; only the source is a fixture.
func TestCheckTraceLinksToDeliveries(t *testing.T) {
	rec := telemetrytest.Install(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "a.db"), discard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	past := time.Now().Add(-time.Hour)
	wid := ulid.New(past)
	tg := store.Target{ID: "t1", Kind: "pypi", Coordinates: map[string]string{"name": "requests"}, NextCheckAt: past}
	for _, err := range []error{
		st.CreateTarget(ctx, tg),
		st.CreateWatch(ctx, store.Watch{ID: wid, TargetID: tg.ID, Policy: json.RawMessage(`{}`)}),
		st.CreateSink(ctx, store.Sink{ID: "k1", Kind: "tracetest", ConfigEncrypted: []byte(`{"secret":"` + configSecret + `"}`)}),
		st.CreateSubscription(ctx, store.Subscription{ID: "s1", SinkID: "k1"}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	ver := func(raw string, age time.Duration) source.Version {
		return source.Version{Raw: raw, PublishedAt: past.Add(-age)}
	}
	// Baseline: the known set, no events (outside any trace).
	if _, err := event.Commit(ctx, st, event.CheckInput{
		Target: tg, Instance: "angelia", Now: past.Add(-time.Hour),
		Fetched: source.Result{Versions: []source.Version{ver("1.0.0", 48*time.Hour)}},
	}); err != nil {
		t.Fatal(err)
	}
	rec.Reset()

	checker := scheduler.CheckerFunc(func(ctx context.Context, t store.Target) (scheduler.Outcome, error) {
		_, err := event.Commit(ctx, st, event.CheckInput{
			Target: t, Instance: "angelia", Now: time.Now(),
			Fetched: source.Result{Versions: []source.Version{ver("1.1.0", 0), ver("1.0.0", 48*time.Hour)}},
		})
		return scheduler.Outcome{}, err
	})
	sch, err := scheduler.New(st, checker, scheduler.Options{Holder: "sched", PollInterval: 10 * time.Millisecond, Logger: discard()})
	if err != nil {
		t.Fatal(err)
	}
	snk := &secretSink{done: make(chan struct{})}
	reg := sink.NewRegistry()
	if err := reg.Register(snk); err != nil {
		t.Fatal(err)
	}
	disp, err := dispatch.New(st, reg, dispatch.Options{
		Holder: "disp", PollInterval: 10 * time.Millisecond, RetryBase: time.Millisecond, RetryMax: time.Millisecond, Logger: discard(),
	})
	if err != nil {
		t.Fatal(err)
	}

	errc := make(chan error, 2)
	rctx, stop := context.WithCancel(ctx)
	go func() { errc <- sch.Run(rctx) }()
	go func() { errc <- disp.Run(rctx) }()
	select {
	case <-snk.done:
	case <-ctx.Done():
		t.Fatal("the delivery did not complete")
	}
	// The attempt is recorded after the sink returns; wait for the roles to
	// finish their work, so every span has ended.
	time.Sleep(100 * time.Millisecond)
	stop()
	for range 2 {
		if err := <-errc; err != nil {
			t.Fatal(err)
		}
	}

	spans := rec.Ended()
	byName := map[string][]trace.SpanContext{}
	for _, s := range spans {
		byName[s.Name()] = append(byName[s.Name()], s.SpanContext())
	}
	checks := byName["angelia.check"]
	if len(checks) != 1 {
		t.Fatalf("%d check spans, want 1 (spans: %v)", len(checks), keys(byName))
	}
	var commit, check trace.SpanContext
	for _, s := range spans {
		switch s.Name() {
		case "angelia.check":
			check = s.SpanContext()
			if s.Parent().IsValid() {
				t.Error("the check span is not a root")
			}
		case "angelia.commit":
			commit = s.SpanContext()
			if s.Parent().SpanID() != checks[0].SpanID() {
				t.Errorf("commit parent = %v, want the check span", s.Parent())
			}
		}
	}
	if commit.TraceID() != check.TraceID() {
		t.Error("the commit is not in the check's trace")
	}

	// Two attempts (one failed, one delivered), each linked to the check.
	var deliveries int
	for _, s := range spans {
		if s.Name() != "angelia.delivery" {
			continue
		}
		deliveries++
		if s.Parent().IsValid() {
			t.Error("a delivery span has a parent: it must be a trace of its own, linked to the check")
		}
		if s.SpanContext().TraceID() == check.TraceID() {
			t.Error("a delivery shares the check's trace id")
		}
		links := s.Links()
		if len(links) != 1 || links[0].SpanContext.TraceID() != check.TraceID() || links[0].SpanContext.SpanID() != commit.SpanID() {
			t.Errorf("delivery links = %v, want one link to the commit span %v of the check trace", links, commit)
		}
	}
	if deliveries != 2 {
		t.Errorf("%d delivery spans, want 2 (one failed attempt, one delivered)", deliveries)
	}
	// The link survives in the store, for dispatchers in other processes.
	ev, err := st.ListEventsAfter(ctx, 0, 10)
	if err != nil || len(ev) != 1 {
		t.Fatalf("events: %v, %v", ev, err)
	}
	ds, err := st.ListDeliveries(ctx, ev[0].ID)
	if err != nil || len(ds) != 1 || !strings.Contains(ds[0].TraceParent, check.TraceID().String()) {
		t.Errorf("stored traceparent = %+v, %v; want the check's trace id %s", ds, err, check.TraceID())
	}

	// AC: no secrets in span attributes (nor events, links or status).
	dump := telemetrytest.Dump(spans)
	for _, secret := range []string{pathSecret, configSecret, "hooks.example", "token="} {
		if strings.Contains(dump, secret) {
			t.Errorf("span export contains %q:\n%s", secret, dump)
		}
	}
}

// A failing check marks its span as failed, without the error text.
func TestFailedCheckSpan(t *testing.T) {
	rec := telemetrytest.Install(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "a.db"), discard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.CreateTarget(ctx, store.Target{ID: "t1", Kind: "pypi", Coordinates: map[string]string{"name": "x"}, NextCheckAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	var called atomic.Bool
	checker := scheduler.CheckerFunc(func(context.Context, store.Target) (scheduler.Outcome, error) {
		defer called.Store(true)
		return scheduler.Outcome{}, errors.New("GET https://registry.example/x?token=" + configSecret + ": 500")
	})
	sch, err := scheduler.New(st, checker, scheduler.Options{Holder: "sched", PollInterval: 10 * time.Millisecond, Logger: discard()})
	if err != nil {
		t.Fatal(err)
	}
	rctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- sch.Run(rctx) }()
	for !called.Load() && ctx.Err() == nil {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	stop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	spans := rec.Ended()
	if len(spans) != 1 || spans[0].Status().Code.String() != "Error" {
		t.Fatalf("spans = %v, want one failed check span", spans)
	}
	if dump := telemetrytest.Dump(spans); strings.Contains(dump, configSecret) || strings.Contains(dump, "registry.example") {
		t.Errorf("error text leaked into the span:\n%s", dump)
	}
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
