package event

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/policy"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
	"github.com/angelia-dev/angelia/internal/subscription"
	"github.com/angelia-dev/angelia/internal/ulid"
)

var (
	check1 = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	check2 = check1.Add(time.Hour)
	// published is the upload time of 2.33.0, two hours before check2.
	published = check2.Add(-2 * time.Hour)

	wNow  = ulid.New(check1) // no cooldown, pre-releases included
	wCool = ulid.New(check1) // 72h cooldown: holds 2.33.0
)

func pypiVersion(raw string, at time.Time) source.Version {
	return source.Version{
		Raw: raw, PublishedAt: at, URL: "https://pypi.org/project/requests/" + raw + "/",
		Metadata: map[string]any{"project": "requests", "project_name": "requests"},
	}
}

// fixture is a pypi target with two watches and four subscriptions, after a
// baseline check that saw 2.31.0 and 2.32.0.
func fixture(t *testing.T) (*sqlite.Store, store.Target) {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "a.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	tg := store.Target{ID: "t-requests", Kind: "pypi", Coordinates: map[string]string{"name": "requests"}, NextCheckAt: check1}
	must(t, st.CreateTarget(ctx, tg))
	must(t, st.CreateWatch(ctx, store.Watch{
		ID: wNow, TargetID: tg.ID, Name: "requests", Labels: map[string]string{"team": "platform"},
		Policy: json.RawMessage(`{"prerelease":"include"}`),
	}))
	must(t, st.CreateWatch(ctx, store.Watch{ID: wCool, TargetID: tg.ID, Policy: json.RawMessage(`{"cooldown":"72h"}`)}))
	must(t, st.CreateSink(ctx, store.Sink{ID: "k", Kind: "webhook", ConfigEncrypted: []byte{1}}))
	for _, s := range []store.Subscription{
		{ID: "s-all", Filter: json.RawMessage(`{"type":["release.published","release.prerelease"]}`)},
		{ID: "s-cool", Filter: json.RawMessage(`{"labels":{"team":"platform"}}`), Policy: json.RawMessage(`{"cooldown":"24h"}`)},
		{ID: "s-final", Filter: json.RawMessage(`{"type":["release.published","release.prerelease"]}`), Policy: json.RawMessage(`{"prerelease":"none"}`)},
		{ID: "s-npm", Filter: json.RawMessage(`{"source":["npm"]}`)},
		{ID: "s-broken", Filter: json.RawMessage(`{"nope":1}`)},
	} {
		s.SinkID = "k"
		must(t, st.CreateSubscription(ctx, s))
	}
	n, err := Commit(ctx, st, CheckInput{Target: tg, Instance: "angelia", Now: check1, Fetched: source.Result{Versions: []source.Version{
		pypiVersion("2.32.0", check1.Add(-48*time.Hour)), pypiVersion("2.31.0", check1.Add(-96*time.Hour)),
	}}})
	if err != nil || n != (Emitted{}) {
		t.Fatalf("baseline check: %+v, %v", n, err)
	}
	return st, tg
}

// secondCheck lists a new final release and a new pre-release.
func secondCheck(tg store.Target) CheckInput {
	return CheckInput{Target: tg, Instance: "angelia", Now: check2, Fetched: source.Result{Versions: []source.Version{
		pypiVersion("2.34.0rc1", check2.Add(-time.Minute)), pypiVersion("2.33.0", published),
		pypiVersion("2.32.0", check1.Add(-48*time.Hour)), pypiVersion("2.31.0", check1.Add(-96*time.Hour)),
	}}}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestCommitWritesEventsAndOneDeliveryPerMatchingSubscription(t *testing.T) {
	st, tg := fixture(t)
	ctx := t.Context()
	n, err := Commit(ctx, st, secondCheck(tg))
	if err != nil {
		t.Fatal(err)
	}
	// wNow emits both versions; wCool holds 2.33.0 and drops the rc.
	// s-cool holds both events; s-broken gets a dead delivery for each.
	if n != (Emitted{Events: 2, Deliveries: 7, Held: 2, Dead: 2}) {
		t.Fatalf("emitted %+v", n)
	}
	s := loadSchemas(t)
	evs, err := st.ListEventsAfter(ctx, 0, 0)
	must(t, err)
	if len(evs) != 2 {
		t.Fatalf("events = %+v", evs)
	}
	byVersion := map[string][]store.Delivery{}
	for _, e := range evs {
		env := validJSON(t, s, e.Payload)
		data := env["data"].(map[string]any)
		if e.WatchID != wNow || env["id"] != e.ID || env["type"] != e.Type || !e.CreatedAt.Equal(check2) ||
			env["source"] != "angelia://angelia/watches/"+wNow || env["time"] != "2026-11-01T11:00:00Z" {
			t.Errorf("event row %+v does not agree with its payload %s", e, e.Payload)
		}
		ds, err := st.ListDeliveries(ctx, e.ID)
		must(t, err)
		byVersion[data["version"].(string)] = ds
		if data["purl"] != "pkg:pypi/requests@"+data["version"].(string) {
			t.Errorf("purl = %v", data["purl"])
		}
	}
	if evs[0].Type != TypePublished || evs[1].Type != TypePrerelease {
		t.Errorf("types = %s, %s", evs[0].Type, evs[1].Type)
	}

	subsOf := func(ds []store.Delivery) []string {
		var ids []string
		for _, d := range ds {
			ids = append(ids, d.SubscriptionID)
		}
		slices.Sort(ids)
		return ids
	}
	// 2.33.0: s-all and s-final due now; s-cool held 24h from the upload
	// time (pypi dates are trusted); s-npm does not match; s-broken is skipped.
	final := byVersion["2.33.0"]
	if got := subsOf(final); !reflect.DeepEqual(got, []string{"s-all", "s-broken", "s-cool", "s-final"}) {
		t.Fatalf("2.33.0 deliveries to %v", got)
	}
	for _, d := range final {
		if d.SubscriptionID == "s-broken" {
			// An undecodable subscription does not block the others; its
			// delivery is dead, with the reason, so it is visible.
			if d.Status != store.DeliveryDead || !strings.HasPrefix(d.LastError, InvalidSubscription) ||
				!strings.Contains(d.LastError, "nope") || d.Hold != nil || !d.NextAttemptAt.IsZero() {
				t.Errorf("delivery to the invalid subscription: %+v", d)
			}
			continue
		}
		if d.Status != store.DeliveryPending || d.Attempts != 0 || !ulid.Valid(d.ID) {
			t.Errorf("delivery %+v", d)
		}
		switch d.SubscriptionID {
		case "s-cool":
			until := published.Add(24 * time.Hour)
			h := d.Hold
			if h == nil || !h.Until.Equal(until) || !d.NextAttemptAt.Equal(until) || !h.StartedAt.Equal(published) ||
				h.WatchCooldown != 0 || h.Prerelease || h.WatchID != wNow || h.Raw != "2.33.0" {
				t.Fatalf("held delivery %+v hold %+v", d, h)
			}
			var in subscription.Input
			must(t, json.Unmarshal(h.Input, &in))
			if in.Source != "pypi" || in.Package != "requests" || in.Type != "release.published" || in.Labels["team"] != "platform" {
				t.Errorf("hold input %+v", in)
			}
		default:
			if d.Hold != nil || !d.NextAttemptAt.Equal(check2) {
				t.Errorf("due delivery %+v", d)
			}
		}
	}
	// The pre-release: s-final's policy drops it, s-cool holds it.
	if got := subsOf(byVersion["2.34.0rc1"]); !reflect.DeepEqual(got, []string{"s-all", "s-broken", "s-cool"}) {
		t.Fatalf("2.34.0rc1 deliveries to %v", got)
	}

	held, err := st.ListPendingReleases(ctx, tg.ID)
	must(t, err)
	if len(held) != 1 || held[0].WatchID != wCool || held[0].Raw != "2.33.0" {
		t.Fatalf("pending releases = %+v", held)
	}

	// The held version is emitted by the check after its cooldown, with
	// cooldown_satisfied_at and the original detection time.
	later := check2.Add(80 * time.Hour)
	in := secondCheck(tg)
	in.Now, in.Fetched = later, source.Result{NotModified: true}
	n, err = Commit(ctx, st, in)
	if err != nil || n.Events != 1 {
		t.Fatalf("after cooldown: %+v, %v", n, err)
	}
	evs, err = st.ListEventsAfter(ctx, evs[1].Seq, 0)
	must(t, err)
	data := validJSON(t, s, evs[0].Payload)["data"].(map[string]any)
	if evs[0].WatchID != wCool || data["detected_at"] != "2026-11-01T11:00:00Z" ||
		data["cooldown_satisfied_at"] != "2026-11-04T09:00:00Z" || data["watch"].(map[string]any)["name"] != nil {
		t.Fatalf("held release event %s", evs[0].Payload)
	}
}

// faultTx fails the n-th write of a check (counting from 1), standing for a
// crash between two steps.
type faultTx struct {
	store.Tx
	f *faults
}

type faults struct {
	failAt, writes int
	seen           map[string]bool
}

var errCrash = errors.New("injected crash")

func (f *faults) write(op string) error {
	f.writes++
	f.seen[op] = true
	if f.writes == f.failAt {
		return fmt.Errorf("%s: %w", op, errCrash)
	}
	return nil
}

func (x faultTx) AddVersion(ctx context.Context, v store.Version) (bool, error) {
	if err := x.f.write("AddVersion"); err != nil {
		return false, err
	}
	return x.Tx.AddVersion(ctx, v)
}

func (x faultTx) UpdateVersion(ctx context.Context, v store.Version) error {
	if err := x.f.write("UpdateVersion"); err != nil {
		return err
	}
	return x.Tx.UpdateVersion(ctx, v)
}

func (x faultTx) MarkBaselineDone(ctx context.Context, id string) error {
	if err := x.f.write("MarkBaselineDone"); err != nil {
		return err
	}
	return x.Tx.MarkBaselineDone(ctx, id)
}

func (x faultTx) AddPendingRelease(ctx context.Context, p store.PendingRelease) error {
	if err := x.f.write("AddPendingRelease"); err != nil {
		return err
	}
	return x.Tx.AddPendingRelease(ctx, p)
}

func (x faultTx) DeletePendingRelease(ctx context.Context, w, tg, raw string) error {
	if err := x.f.write("DeletePendingRelease"); err != nil {
		return err
	}
	return x.Tx.DeletePendingRelease(ctx, w, tg, raw)
}

func (x faultTx) DeletePendingReleasesOfWatch(ctx context.Context, w string) (int64, error) {
	if err := x.f.write("DeletePendingReleasesOfWatch"); err != nil {
		return 0, err
	}
	return x.Tx.DeletePendingReleasesOfWatch(ctx, w)
}

func (x faultTx) UpdateTarget(ctx context.Context, tg store.Target) error {
	if err := x.f.write("UpdateTarget"); err != nil {
		return err
	}
	return x.Tx.UpdateTarget(ctx, tg)
}

func (x faultTx) AppendEvent(ctx context.Context, e store.Event, ds []store.Delivery) error {
	if err := x.f.write("AppendEvent"); err != nil {
		return err
	}
	return x.Tx.AppendEvent(ctx, e, ds)
}

// failingStore runs every transaction through faultTx; failAt 0 never fails.
type failingStore struct {
	st *sqlite.Store
	f  *faults
}

func (s failingStore) InTx(ctx context.Context, fn func(store.Tx) error) error {
	return s.st.InTx(ctx, func(tx store.Tx) error { return fn(faultTx{Tx: tx, f: s.f}) })
}

// snapshot is everything a check can write.
type snapshot struct {
	Versions   []store.Version
	Pending    []store.PendingRelease
	Events     []store.Event
	Deliveries map[string][]store.Delivery
	Baselines  []bool
}

func snap(t *testing.T, st *sqlite.Store, targetID string, watches ...string) snapshot {
	t.Helper()
	ctx := t.Context()
	var s snapshot
	var err error
	s.Versions, err = st.ListVersions(ctx, targetID)
	must(t, err)
	s.Pending, err = st.ListPendingReleases(ctx, targetID)
	must(t, err)
	s.Events, err = st.ListEventsAfter(ctx, 0, 1000)
	must(t, err)
	s.Deliveries = map[string][]store.Delivery{}
	for _, e := range s.Events {
		s.Deliveries[e.ID], err = st.ListDeliveries(ctx, e.ID)
		must(t, err)
	}
	for _, id := range watches {
		w, err := st.GetWatch(ctx, id)
		must(t, err)
		s.Baselines = append(s.Baselines, w.BaselineDone)
	}
	return s
}

// faultTx must intercept every write of store.Tx: a method that is not
// List*, Get* or SubscriptionGeneration and that faultTx does not override would reach the nil
// embedded Tx and panic instead of returning the injected crash.
func TestFaultTxOverridesEveryWrite(t *testing.T) {
	typ := reflect.TypeFor[store.Tx]()
	x := reflect.ValueOf(faultTx{f: &faults{failAt: 1, seen: map[string]bool{}}})
	for m := range typ.Methods() {
		if strings.HasPrefix(m.Name, "List") || strings.HasPrefix(m.Name, "Get") || m.Name == "SubscriptionGeneration" {
			continue
		}
		t.Run(m.Name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("faultTx does not override the write %s", m.Name)
				}
			}()
			fn := x.MethodByName(m.Name)
			args := make([]reflect.Value, fn.Type().NumIn())
			for i := range args {
				args[i] = reflect.Zero(fn.Type().In(i))
			}
			args[0] = reflect.ValueOf(t.Context())
			x.Interface().(faultTx).f.writes = 0
			out := fn.Call(args)
			if err, _ := out[len(out)-1].Interface().(error); !errors.Is(err, errCrash) {
				t.Fatalf("%s = %v, want the injected crash", m.Name, err)
			}
		})
	}
}

// crashEveryWrite runs the check once per write, failing that write, and
// checks that each failure left the store as before; then it runs the check
// without a fault. It returns the operations faults were injected at.
func crashEveryWrite(t *testing.T, st *sqlite.Store, in CheckInput, watches []string) (map[string]bool, Emitted) {
	t.Helper()
	ctx := t.Context()
	before := snap(t, st, in.Target.ID, watches...)
	f := &faults{seen: map[string]bool{}}
	for f.failAt = 1; ; f.failAt++ {
		f.writes = 0
		n, err := Commit(ctx, failingStore{st: st, f: f}, in)
		if f.writes < f.failAt { // no write left to fail: the check committed
			if err != nil {
				t.Fatalf("check without a fault: %v", err)
			}
			return f.seen, n
		}
		if !errors.Is(err, errCrash) || n != (Emitted{}) {
			t.Fatalf("crash at write %d: %+v, %v", f.failAt, n, err)
		}
		if got := snap(t, st, in.Target.ID, watches...); !reflect.DeepEqual(got, before) {
			t.Fatalf("crash at write %d left a partial check:\n%+v\nwant\n%+v", f.failAt, got, before)
		}
	}
}

// Acceptance criterion 2 of #106: a crash injected between any two writes
// of a check leaves either everything or nothing. Three checks cover every
// write a check makes: new versions (with a baseline), a held version
// released after its cooldown, and a yank.
func TestCommitAllOrNothing(t *testing.T) {
	st, tg := fixture(t)
	ctx := t.Context()
	// A third watch without a baseline adds a MarkBaselineDone write.
	wNew := ulid.New(check1)
	must(t, st.CreateWatch(ctx, store.Watch{ID: wNew, TargetID: tg.ID}))
	watches := []string{wNow, wCool, wNew}
	seen := map[string]bool{}

	got, n := crashEveryWrite(t, st, secondCheck(tg), watches)
	maps.Copy(seen, got)
	after := snap(t, st, tg.ID, watches...)
	if n.Events != 2 || len(after.Versions) != 4 || len(after.Pending) != 1 || len(after.Events) != 2 || !after.Baselines[2] {
		t.Fatalf("committed check: %+v, %+v", n, after)
	}

	cooled := secondCheck(tg)
	cooled.Now, cooled.Fetched = check2.Add(80*time.Hour), source.Result{NotModified: true}
	got, n = crashEveryWrite(t, st, cooled, watches)
	maps.Copy(seen, got)
	if after = snap(t, st, tg.ID, watches...); n.Events != 1 || len(after.Pending) != 0 || len(after.Events) != 3 {
		t.Fatalf("after cooldown: %+v, %+v", n, after)
	}

	yank := secondCheck(tg)
	yank.Now = check2.Add(81 * time.Hour)
	yank.Fetched.Versions = slices.Clone(yank.Fetched.Versions)
	yank.Fetched.Versions[2].Yanked = true // 2.32.0
	got, n = crashEveryWrite(t, st, yank, watches)
	maps.Copy(seen, got)
	vs, err := st.ListVersions(ctx, tg.ID)
	must(t, err)
	yanked := slices.IndexFunc(vs, func(v store.Version) bool { return v.Raw == "2.32.0" && v.Yanked })
	if n.Events != 0 || yanked < 0 {
		t.Fatalf("yank check: %+v, versions %+v", n, vs)
	}

	for _, op := range []string{"AddVersion", "UpdateVersion", "MarkBaselineDone", "AddPendingRelease", "DeletePendingRelease", "AppendEvent"} {
		if !seen[op] {
			t.Errorf("no crash was injected at %s", op)
		}
	}
}

func TestEmitWithoutReleasesReadsNothing(t *testing.T) {
	n, err := Emit(t.Context(), nil, nil, EmitInput{})
	if err != nil || n != (Emitted{}) {
		t.Fatalf("Emit(nil) = %+v, %v", n, err)
	}
}

// errTx fails ListSubscriptions and AppendEvent.
type errTx struct{ store.Tx }

func (errTx) ListSubscriptions(context.Context) ([]store.Subscription, error) {
	return nil, errCrash
}

func TestEmitErrors(t *testing.T) {
	ctx := t.Context()
	tg := store.Target{ID: "t", Kind: "npm", Coordinates: map[string]string{"name": "x"}}
	rel := []policy.WatchReleases{{Watch: store.Watch{ID: wNow}, Releases: []policy.Release{{Raw: "1.0.0", Scheme: "semver", DetectedAt: check1}}}}
	if _, err := Emit(ctx, errTx{}, rel, EmitInput{Target: tg, Instance: "angelia", Now: check1}); !errors.Is(err, errCrash) {
		t.Errorf("list subscriptions error: %v", err)
	}
	st, _ := fixture(t)
	emit := func(in EmitInput) error {
		in.Target, in.Now = tg, check1
		return st.InTx(ctx, func(tx store.Tx) error {
			_, err := Emit(ctx, tx, rel, in)
			return err
		})
	}
	if err := emit(EmitInput{Instance: "Bad"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid instance: %v", err)
	}
	// Every delivery gets the same id: the second one conflicts after the
	// event and the first delivery were written.
	dup := func(time.Time) string { return "01JBX2A7C8D9E0F1G2H3J4K5M6" }
	if err := emit(EmitInput{Instance: "angelia", NewID: dup}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("deliveries with one id: %v", err)
	}
	evs, err := st.ListEventsAfter(ctx, 0, 0)
	must(t, err)
	if len(evs) != 0 {
		t.Fatalf("events persisted after a failed emit: %+v", evs)
	}
	ds, err := st.ListDeliveries(ctx, "01JBX2A7C8D9E0F1G2H3J4K5M6")
	must(t, err)
	if len(ds) != 0 {
		t.Fatalf("deliveries persisted after a failed emit: %+v", ds)
	}
}
