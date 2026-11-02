package event

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/diff"
	"github.com/angelia-dev/angelia/internal/policy"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
	"github.com/angelia-dev/angelia/internal/ulid"
	"github.com/angelia-dev/angelia/internal/version"
)

// pypiRelease is the release of the spec's yank example (example-lib).
func pypiRelease(raw, prev string, bump version.Bump) policy.Release {
	return policy.Release{
		Raw: raw, Normalized: raw, Scheme: "pep440", Previous: prev, Bump: bump,
		PublishedAt: time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC), DetectedAt: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC),
		URL:                 "https://pypi.org/project/example-lib/" + raw + "/",
		Metadata:            map[string]any{"project_name": "example-lib"},
		CooldownSatisfiedAt: time.Date(2026, 10, 23, 8, 0, 0, 0, time.UTC),
	}
}

func readSpec(t *testing.T, path string) []byte {
	t.Helper()
	b, err := fs.ReadFile(specFS, path)
	must(t, err)
	return b
}

// yankOf returns the second check's fetch with the given versions yanked.
func yankOf(tg store.Target, now time.Time, raws ...string) CheckInput {
	in := secondCheck(tg)
	in.Now = now
	in.Fetched.Versions = slices.Clone(in.Fetched.Versions)
	for i, v := range in.Fetched.Versions {
		if slices.Contains(raws, v.Raw) {
			in.Fetched.Versions[i].Yanked = true
		}
	}
	return in
}

func eventsAfter(t *testing.T, st *sqlite.Store, seq int64) []store.Event {
	t.Helper()
	evs, err := st.ListEventsAfter(t.Context(), seq, 1000)
	must(t, err)
	return evs
}

func deliveriesBySub(t *testing.T, st *sqlite.Store, eventID string) map[string]store.Delivery {
	t.Helper()
	ds, err := st.ListDeliveries(t.Context(), eventID)
	must(t, err)
	out := map[string]store.Delivery{}
	for _, d := range ds {
		out[d.SubscriptionID] = d
	}
	return out
}

// AC 1 of #112: a yank after publish emits release.yanked referencing the
// original event; the payload repeats the stored classification with
// yanked true, cooldown_satisfied_at null and detected_at the check that saw
// the yank; policy.* does not apply to it; it cancels held deliveries of the
// version (issue comment, ADR 0014).
func TestYankAfterPublishEmitsYankedReferencingTheOriginal(t *testing.T) {
	st, tg := fixture(t)
	ctx := t.Context()
	for _, s := range []store.Subscription{
		// Only yanks. Its policy would drop a final release and hold it 48h:
		// policy.* does not apply to release.yanked.
		{ID: "s-yank", Filter: json.RawMessage(`{"type":["release.yanked"]}`), Policy: json.RawMessage(`{"prerelease":"only","cooldown":"48h"}`)},
		// Yanks of patches only: 2.33.0 is a minor.
		{ID: "s-yank-patch", Filter: json.RawMessage(`{"type":["release.yanked"],"bump":["patch"]}`)},
	} {
		s.SinkID = "k"
		must(t, st.CreateSubscription(ctx, s))
	}
	_, err := Commit(ctx, st, secondCheck(tg))
	must(t, err)
	before := eventsAfter(t, st, 0)
	var orig store.Event
	for _, e := range before {
		if e.Type == TypePublished && e.WatchID == wNow {
			orig = e
		}
	}
	if orig.ID == "" {
		t.Fatalf("no published event for wNow: %+v", before)
	}
	// s-cool holds 2.33.0 until published+24h; its hold of 2.34.0rc1 must stay.
	if d := deliveriesBySub(t, st, orig.ID)["s-cool"]; d.Hold == nil {
		t.Fatalf("s-cool delivery of 2.33.0 is not held: %+v", d)
	}

	// The labels change between the release and the yank: the yank carries
	// the current ones.
	w, err := st.GetWatch(ctx, wNow)
	must(t, err)
	must(t, st.InConfigTx(ctx, func(tx store.ConfigTx) error {
		w.Labels = map[string]string{"team": "platform", "tier": "1"}
		return tx.UpdateWatch(ctx, w)
	}))

	check3 := check2.Add(time.Hour + 250*time.Millisecond)
	n, err := Commit(ctx, st, yankOf(tg, check3, "2.33.0", "2.32.0"))
	must(t, err)
	// One event: wNow emitted 2.33.0; wCool held it (dropped now, no event);
	// 2.32.0 is from the baseline (never emitted, no event). Deliveries:
	// s-cool (no type filter), s-yank, s-broken (dead). s-cool's held
	// delivery of 2.33.0 is cancelled.
	if n != (Emitted{Events: 1, Deliveries: 3, Dead: 1, Cancelled: 1}) {
		t.Fatalf("emitted %+v", n)
	}
	evs := eventsAfter(t, st, before[len(before)-1].Seq)
	if len(evs) != 1 {
		t.Fatalf("events = %+v", evs)
	}
	y := evs[0]
	if y.Type != TypeYanked || y.WatchID != wNow || y.RefEventID != orig.ID || !y.CreatedAt.Equal(check3) || !ulid.Valid(y.ID) || y.ID == orig.ID {
		t.Fatalf("yank event row %+v (original %s)", y, orig.ID)
	}

	s := loadSchemas(t)
	got := validJSON(t, s, y.Payload)
	want := validJSON(t, s, orig.Payload)
	gd, wd := got["data"].(map[string]any), want["data"].(map[string]any)
	if got["id"] != y.ID || got["type"] != TypeYanked || got["time"] != "2026-11-01T12:00:00.25Z" ||
		got["subject"] != want["subject"] || got["source"] != want["source"] {
		t.Errorf("yank envelope %s", y.Payload)
	}
	if gd["yanked"] != true || gd["detected_at"] != "2026-11-01T12:00:00.25Z" || gd["cooldown_satisfied_at"] != nil {
		t.Errorf("yank data %s", y.Payload)
	}
	if !reflect.DeepEqual(gd["watch"], map[string]any{"id": wNow, "name": "requests", "labels": map[string]any{"team": "platform", "tier": "1"}}) {
		t.Errorf("watch = %v", gd["watch"])
	}
	// Everything else is the original's, so it matches what the subscriber got.
	for _, k := range []string{"yanked", "detected_at", "cooldown_satisfied_at", "watch"} {
		delete(gd, k)
		delete(wd, k)
	}
	if !reflect.DeepEqual(gd, wd) {
		t.Errorf("yank data differs from the original:\n%v\n%v", gd, wd)
	}

	ds := deliveriesBySub(t, st, y.ID)
	if got := slices.Sorted(maps.Keys(ds)); !reflect.DeepEqual(got, []string{"s-broken", "s-cool", "s-yank"}) {
		t.Fatalf("yank deliveries to %v", got)
	}
	for _, id := range []string{"s-cool", "s-yank"} {
		if d := ds[id]; d.Status != store.DeliveryPending || d.Hold != nil || !d.NextAttemptAt.Equal(check3) {
			t.Errorf("%s yank delivery %+v: not due now", id, d)
		}
	}
	if d := ds["s-broken"]; d.Status != store.DeliveryDead || !strings.HasPrefix(d.LastError, InvalidSubscription) {
		t.Errorf("s-broken yank delivery %+v", d)
	}

	// The held delivery of 2.33.0 is gone; the other deliveries of the
	// original, and s-cool's hold of the pre-release, are not touched.
	od := deliveriesBySub(t, st, orig.ID)
	if _, ok := od["s-cool"]; ok {
		t.Errorf("held delivery of the yanked version not cancelled: %+v", od["s-cool"])
	}
	if len(od) != 3 {
		t.Errorf("original deliveries after the yank: %+v", od)
	}
	for _, e := range before {
		if e.Type == TypePrerelease {
			if d := deliveriesBySub(t, st, e.ID)["s-cool"]; d.Hold == nil {
				t.Errorf("hold of the pre-release was cancelled: %+v", d)
			}
		}
	}

	// The same yank state at the next check is no change: nothing more.
	n, err = Commit(ctx, st, yankOf(tg, check3.Add(time.Hour), "2.33.0", "2.32.0"))
	if err != nil || n != (Emitted{}) {
		t.Fatalf("repeated yank: %+v, %v", n, err)
	}
}

// One event per watch per yank: both watches emitted 2.33.0, so each gets
// one release.yanked, referencing its own event.
func TestYankEmitsOneEventPerWatch(t *testing.T) {
	st, tg := fixture(t)
	ctx := t.Context()
	_, err := Commit(ctx, st, secondCheck(tg))
	must(t, err)
	cooled := secondCheck(tg)
	cooled.Now, cooled.Fetched = check2.Add(80*time.Hour), source.Result{NotModified: true}
	_, err = Commit(ctx, st, cooled)
	must(t, err)
	before := eventsAfter(t, st, 0)
	origOf := map[string]string{}
	for _, e := range before {
		if e.Type == TypePublished {
			origOf[e.WatchID] = e.ID
		}
	}
	if len(origOf) != 2 {
		t.Fatalf("published events: %+v", before)
	}
	n, err := Commit(ctx, st, yankOf(tg, check2.Add(81*time.Hour), "2.33.0"))
	must(t, err)
	if n.Events != 2 {
		t.Fatalf("emitted %+v", n)
	}
	for _, y := range eventsAfter(t, st, before[len(before)-1].Seq) {
		if y.Type != TypeYanked || y.RefEventID != origOf[y.WatchID] {
			t.Errorf("yank %+v, want a reference to %s", y, origOf[y.WatchID])
		}
		var env Envelope
		must(t, json.Unmarshal(y.Payload, &env))
		if env.Data.Watch.ID != y.WatchID || env.Source != "angelia://angelia/watches/"+y.WatchID {
			t.Errorf("yank of watch %s carries %+v", y.WatchID, env.Data.Watch)
		}
	}
}

// A yanked pre-release keeps prerelease true; un-yank emits nothing (no
// event in v1); a later yank of the same version is a new yank and emits
// again, referencing the same original.
func TestYankOfPrereleaseAndUnyank(t *testing.T) {
	st, tg := fixture(t)
	ctx := t.Context()
	_, err := Commit(ctx, st, secondCheck(tg))
	must(t, err)
	var orig store.Event
	for _, e := range eventsAfter(t, st, 0) {
		if e.Type == TypePrerelease {
			orig = e
		}
	}
	at := check2.Add(time.Hour)
	var refs []string
	for i, yanked := range []bool{true, false, true} {
		in := secondCheck(tg)
		in.Now = at.Add(time.Duration(i) * time.Hour)
		if yanked {
			in = yankOf(tg, in.Now, "2.34.0rc1")
		}
		var logs bytes.Buffer
		in.Logger = slog.New(slog.NewTextHandler(&logs, nil))
		n, err := Commit(ctx, st, in)
		must(t, err)
		if !yanked {
			if n.Events != 0 || !strings.Contains(logs.String(), "un-yanked") {
				t.Fatalf("un-yank: %+v, logs %s", n, logs.String())
			}
			continue
		}
		evs := eventsAfter(t, st, 0)
		y := evs[len(evs)-1]
		var env Envelope
		must(t, json.Unmarshal(y.Payload, &env))
		if n.Events != 1 || y.Type != TypeYanked || !env.Data.Prerelease || !env.Data.Yanked || env.Data.Version != "2.34.0rc1" {
			t.Fatalf("yank %d: %+v %s", i, n, y.Payload)
		}
		refs = append(refs, y.RefEventID)
	}
	if !reflect.DeepEqual(refs, []string{orig.ID, orig.ID}) {
		t.Fatalf("references %v, want the pre-release event %s twice", refs, orig.ID)
	}
}

// A yank of a version a watch emitted while it watched another target is not
// a yank of that watch's current package: the subject differs, no event, and
// the holds of the previous package's version (same raw string) are kept.
func TestYankMatchesTheSubjectNotOnlyTheVersion(t *testing.T) {
	st, tg := fixture(t)
	ctx := t.Context()
	_, err := Commit(ctx, st, secondCheck(tg))
	must(t, err)
	var orig store.Event
	for _, e := range eventsAfter(t, st, 0) {
		if e.Type == TypePublished && e.WatchID == wNow {
			orig = e
		}
	}
	// s-cool holds requests 2.33.0 until published+24h (check2+22h).
	if d := deliveriesBySub(t, st, orig.ID)["s-cool"]; d.Hold == nil {
		t.Fatalf("s-cool delivery of requests 2.33.0 is not held: %+v", d)
	}
	other := store.Target{ID: "t-other", Kind: "pypi", Coordinates: map[string]string{"name": "other"}, NextCheckAt: check1}
	must(t, st.CreateTarget(ctx, other))
	w, err := st.GetWatch(ctx, wNow)
	must(t, err)
	must(t, st.InConfigTx(ctx, func(tx store.ConfigTx) error {
		w.TargetID = other.ID
		return tx.UpdateWatch(ctx, w)
	}))
	// The first fill of the new target is a baseline: 2.33.0 of "other" is
	// never emitted.
	in := secondCheck(other)
	in.Fetched.Versions = in.Fetched.Versions[1:3]
	n, err := Commit(ctx, st, in)
	if err != nil || n.Events != 0 {
		t.Fatalf("baseline of the new target: %+v, %v", n, err)
	}
	var logs bytes.Buffer
	in = yankOf(other, check2.Add(time.Hour), "2.33.0")
	in.Fetched.Versions = in.Fetched.Versions[1:3]
	in.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	n, err = Commit(ctx, st, in)
	must(t, err)
	if n != (Emitted{}) {
		t.Fatalf("yank on the new target emitted %+v", n)
	}
	if !strings.Contains(logs.String(), "never emitted") {
		t.Errorf("logs: %s", logs.String())
	}
	if d := deliveriesBySub(t, st, orig.ID)["s-cool"]; d.Hold == nil || d.Status != store.DeliveryPending {
		t.Fatalf("yank of other 2.33.0 cancelled the hold of requests 2.33.0: %+v", d)
	}
}

// Commit loads the subscriptions at most once per transaction, even when it
// emits both releases and yanks and the cache is cold (nil Matcher).
func TestCommitLoadsSubscriptionsOncePerTransaction(t *testing.T) {
	st, tg := fixture(t)
	ctx := t.Context()
	cs := &countingStore{Store: st}
	in := secondCheck(tg)
	_, err := Commit(ctx, cs, in)
	must(t, err)
	// A check that both emits (wCool's 2.33.0 after its cooldown) and yanks
	// (wNow's 2.34.0rc1).
	in = yankOf(tg, check2.Add(80*time.Hour), "2.34.0rc1")
	cs.inside.Store(0)
	n, err := Commit(ctx, cs, in)
	must(t, err)
	if n.Events != 2 {
		t.Fatalf("emitted %+v, want a release and a yank", n)
	}
	if got := cs.inside.Load(); got != 1 {
		t.Fatalf("subscriptions listed %d times in the transaction, want 1", got)
	}
}

func TestBuildYank(t *testing.T) {
	s := loadSchemas(t)
	w := store.Watch{ID: "01JBX5D6E7F8G9H0J1K2M3N4P5", Name: "example-lib"}
	orig, err := Build(Input{
		Instance: "ci.example", Target: target("pypi", map[string]string{"name": "example-lib"}), Watch: w,
		Release: pypiRelease("0.4.1", "0.4.0", "patch"), Time: detected,
	})
	must(t, err)
	if orig.Data.CooldownSatisfiedAt == nil {
		t.Fatal("the original needs a cooldown_satisfied_at for this test")
	}
	b, err := json.Marshal(orig)
	must(t, err)
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	y, err := BuildYank(YankInput{ID: "01JBXA1B2C3D4E5F6G7H8J9K0M", Instance: "ci.example", Watch: w, Original: b, Time: at})
	must(t, err)
	got := validEvent(t, s, y)
	// The spec's yank example, from the same release.
	ex := decode(t, readSpec(t, "examples/valid/release/envelope/v1/yanked-pypi.json")).(map[string]any)
	// The example's time is a second after detected_at (the end of a longer
	// check); Angelia stamps both with the check time, as for the other types.
	ex["time"] = "2026-10-31T12:00:00Z"
	if !reflect.DeepEqual(got, ex) {
		gb, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("yank event differs from the spec example:\n%s", gb)
	}

	// Without ID a new ULID is generated for the time.
	y, err = BuildYank(YankInput{Instance: "ci.example", Watch: w, Original: b, Time: at})
	if err != nil || !ulid.Valid(y.ID) {
		t.Fatalf("generated id %q, %v", y.ID, err)
	}

	pub := orig
	pub.Type = TypeYanked
	yanked, _ := json.Marshal(pub)
	pub.Type = TypePublished
	pub.Data.Watch.ID = "01JBX1Z3M4N5P6Q7R8S9T0V1W2"
	otherWatch, _ := json.Marshal(pub)
	pub = orig
	pub.Data.Version = ""
	noVersion, _ := json.Marshal(pub)
	for name, in := range map[string]YankInput{
		"not json":         {Instance: "ci.example", Watch: w, Original: []byte("{"), Time: at},
		"bad instance":     {Instance: "Bad", Watch: w, Original: b, Time: at},
		"bad watch id":     {Instance: "ci.example", Watch: store.Watch{ID: "w1"}, Original: b, Time: at},
		"zero time":        {Instance: "ci.example", Watch: w, Original: b},
		"original yanked":  {Instance: "ci.example", Watch: w, Original: yanked, Time: at},
		"other watch":      {Instance: "ci.example", Watch: w, Original: otherWatch, Time: at},
		"original no data": {Instance: "ci.example", Watch: w, Original: noVersion, Time: at},
		"bad labels":       {Instance: "ci.example", Watch: store.Watch{ID: w.ID, Labels: map[string]string{"-bad": "x"}}, Original: b, Time: at},
	} {
		if _, err := BuildYank(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
}

// errYankTx fails the reads and writes EmitYanks makes.
type errYankTx struct {
	store.Tx
	cancel, get error
}

func (x errYankTx) CancelHeldDeliveries(context.Context, string, time.Time) (int64, error) {
	return 0, x.cancel
}

func (x errYankTx) GetLatestEventBySubject(context.Context, string, string, ...string) (store.Event, error) {
	return store.Event{}, x.get
}

func TestEmitYanksErrors(t *testing.T) {
	ctx := t.Context()
	tg := store.Target{ID: "t", Kind: "pypi", Coordinates: map[string]string{"name": "requests"}}
	watches := []diff.WatchChanges{{
		Watch:   store.Watch{ID: wNow},
		Changes: []diff.Change{{Kind: diff.KindRemoved, Raw: "0.9"}, {Kind: diff.KindYanked, Raw: "1.0"}},
	}}
	in := EmitInput{Target: tg, Instance: "angelia", Now: check1}
	if _, err := EmitYanks(ctx, errYankTx{cancel: errCrash}, watches, in); !errors.Is(err, errCrash) {
		t.Errorf("cancel error: %v", err)
	}
	if _, err := EmitYanks(ctx, errYankTx{get: errCrash}, watches, in); !errors.Is(err, errCrash) {
		t.Errorf("lookup error: %v", err)
	}
	orig := store.Event{ID: "e1", Payload: []byte(`{"type":"` + TypePublished + `"}`)}
	if _, err := EmitYanks(ctx, origTx{orig: orig}, watches, in); !errors.Is(err, ErrInvalid) {
		t.Errorf("unusable original: %v", err)
	}
	if n, err := EmitYanks(ctx, nil, nil, in); err != nil || n != (Emitted{}) {
		t.Errorf("no watches: %+v, %v", n, err)
	}
}

// origTx cancels nothing and returns orig as the original event.
type origTx struct {
	store.Tx
	orig store.Event
}

func (origTx) CancelHeldDeliveries(context.Context, string, time.Time) (int64, error) {
	return 0, nil
}

func (x origTx) GetLatestEventBySubject(context.Context, string, string, ...string) (store.Event, error) {
	return x.orig, nil
}
