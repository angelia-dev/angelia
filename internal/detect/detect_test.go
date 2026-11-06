package detect

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/sourcetest"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
	"github.com/angelia-dev/angelia/internal/ulid"
)

var (
	t0  = time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	t1  = t0.Add(time.Hour)
	wid = ulid.New(t0)
)

type fixture struct {
	st  *sqlite.Store
	tg  store.Target
	src *sourcetest.Fake
	reg *source.Registry
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "a.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	tg := store.Target{ID: "t1", Kind: "pypi", Coordinates: map[string]string{"name": "demo"}, NextCheckAt: t0}
	for _, err := range []error{
		st.CreateTarget(ctx, tg),
		st.CreateWatch(ctx, store.Watch{ID: wid, TargetID: tg.ID, Name: "demo", Policy: json.RawMessage(`{}`)}),
		st.CreateSink(ctx, store.Sink{ID: "k1", Kind: "webhook", ConfigEncrypted: []byte(`{"url":"https://hooks.example.org/x"}`)}),
		st.CreateSubscription(ctx, store.Subscription{ID: "s1", SinkID: "k1"}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	src := sourcetest.NewFake("pypi")
	reg := source.NewRegistry()
	if err := reg.Register(src); err != nil {
		t.Fatal(err)
	}
	return &fixture{st: st, tg: tg, src: src, reg: reg}
}

func (f *fixture) checker(now time.Time) *Checker {
	return &Checker{
		Store: f.st, Sources: f.reg, Instance: "test", Matcher: event.NewMatcherCache(),
		RegistryURL: func(kind string) string {
			if kind == "pypi" {
				return "https://mirror.example.org"
			}
			return ""
		},
		Now: func() time.Time { return now },
	}
}

func ver(raw string, at time.Time) source.Version {
	return source.Version{Raw: raw, PublishedAt: at, Metadata: map[string]any{"project_name": "demo"}}
}

// The first check records the baseline; a later one with a new version
// commits its event and delivery, and the fetch state travels through the
// scheduler's Outcome.
func TestCheckBaselineThenRelease(t *testing.T) {
	ctx := t.Context()
	f := newFixture(t)
	f.src.Set("demo", ver("1.0.0", t0.Add(-48*time.Hour)), ver("0.9.0", t0.Add(-96*time.Hour)))
	out, err := f.checker(t0).Check(ctx, f.tg)
	if err != nil {
		t.Fatal(err)
	}
	var st source.FetchState
	if err := json.Unmarshal(out.FetchState, &st); err != nil || st.ETag != `"1"` {
		t.Fatalf("fetch state = %s, %v; want the fake's ETag", out.FetchState, err)
	}
	if ev, _ := f.st.ListEventsAfter(ctx, 0, 10); len(ev) != 0 {
		t.Fatalf("baseline emitted %d events", len(ev))
	}

	// Same ETag: not modified, state kept as the source returned it.
	f.tg.FetchState = out.FetchState
	out, err = f.checker(t1).Check(ctx, f.tg)
	if err != nil || string(out.FetchState) != `{"etag":"\"1\""}` {
		t.Fatalf("not modified: %s, %v", out.FetchState, err)
	}

	f.src.Set("demo", ver("1.1.0", t1.Add(-time.Minute)), ver("1.0.0", t0.Add(-48*time.Hour)), ver("0.9.0", t0.Add(-96*time.Hour)))
	if _, err := f.checker(t1).Check(ctx, f.tg); err != nil {
		t.Fatal(err)
	}
	ev, err := f.st.ListEventsAfter(ctx, 0, 10)
	if err != nil || len(ev) != 1 {
		t.Fatalf("events = %d, %v; want 1", len(ev), err)
	}
	p := string(ev[0].Payload)
	for _, want := range []string{`"1.1.0"`, "angelia://test/watches/" + wid, "repository_url=https"} {
		if !strings.Contains(p, want) {
			t.Errorf("event payload lacks %s:\n%s", want, p)
		}
	}
	ds, err := f.st.ListDeliveries(ctx, ev[0].ID)
	if err != nil || len(ds) != 1 || ds[0].SubscriptionID != "s1" || ds[0].Status != store.DeliveryPending {
		t.Fatalf("deliveries = %+v, %v", ds, err)
	}
}

type notModified struct{ source.Source }

func (notModified) List(context.Context, source.Target, source.FetchState) (source.Result, error) {
	return source.Result{NotModified: true}, nil
}

// A 304 without validators keeps the stored state: Outcome.FetchState nil.
func TestCheckNotModifiedWithoutStateKeepsState(t *testing.T) {
	f := newFixture(t)
	reg := source.NewRegistry()
	if err := reg.Register(notModified{f.src}); err != nil {
		t.Fatal(err)
	}
	c := f.checker(t0)
	c.Sources = reg
	f.tg.FetchState = json.RawMessage(`{"etag":"\"x\""}`)
	out, err := c.Check(t.Context(), f.tg)
	if err != nil || out.FetchState != nil {
		t.Fatalf("out = %s, %v; want nil state", out.FetchState, err)
	}
}

// An unreadable stored state costs a full fetch, not a stuck target.
func TestCheckIgnoresUnreadableState(t *testing.T) {
	f := newFixture(t)
	f.src.Set("demo", ver("1.0.0", t0))
	f.tg.FetchState = json.RawMessage(`"not an object"`)
	out, err := f.checker(t0).Check(t.Context(), f.tg)
	if err != nil || !strings.Contains(string(out.FetchState), "etag") {
		t.Fatalf("out = %s, %v", out.FetchState, err)
	}
}

func TestCheckErrors(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	// The package is unknown upstream: the source error is returned.
	if _, err := f.checker(t0).Check(ctx, f.tg); err == nil || !strings.Contains(err.Error(), `package "demo" not found`) {
		t.Fatalf("source error: %v", err)
	}
	other := f.tg
	other.Kind = "npm"
	if _, err := f.checker(t0).Check(ctx, other); !errors.Is(err, source.ErrUnknownKind) {
		t.Fatalf("unknown kind: %v", err)
	}
	if _, err := (&Checker{}).Check(ctx, f.tg); err == nil {
		t.Fatal("nil store and sources")
	}
	// A commit that fails (closed store) is returned, nothing is kept.
	f.src.Set("demo", ver("1.0.0", t0))
	_ = f.st.Close()
	if _, err := f.checker(t0).Check(ctx, f.tg); err == nil || !strings.Contains(err.Error(), "commit") {
		t.Fatalf("commit error: %v", err)
	}
}
