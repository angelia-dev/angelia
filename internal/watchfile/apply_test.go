package watchfile

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

type snapshot struct {
	sinks   []store.Sink
	watches []store.Watch
	subs    []store.Subscription
}

func snap(t testing.TB, st store.Store, by string) snapshot {
	t.Helper()
	var s snapshot
	err := st.InConfigTx(t.Context(), func(tx store.ConfigTx) (err error) {
		if s.sinks, err = tx.ListManagedSinks(t.Context(), by); err != nil {
			return err
		}
		if s.watches, err = tx.ListManagedWatches(t.Context(), by); err != nil {
			return err
		}
		s.subs, err = tx.ListManagedSubscriptions(t.Context(), by)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func target(t testing.TB, st store.Store, kind string, coords map[string]string) (store.Target, error) {
	t.Helper()
	var tg store.Target
	err := st.InConfigTx(t.Context(), func(tx store.ConfigTx) (err error) {
		tg, err = tx.FindTarget(t.Context(), kind, coords)
		return err
	})
	return tg, err
}

// AC: the example file loads and produces the expected rows.
func TestExampleFileProducesExpectedRows(t *testing.T) {
	data, err := os.ReadFile("../../examples/watches.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f, err := Parse(data, "examples/watches.yaml", opts(t))
	if err != nil {
		t.Fatal(err)
	}
	st := openStore(t)
	plan, err := Apply(t.Context(), st, f, applyOpts())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Count(KindSink, Create) != 1 || plan.Count(KindWatch, Create) != 4 || plan.Count(KindSubscription, Create) != 2 || plan.Count(KindTarget, Create) != 4 {
		t.Fatalf("plan:\n%s", plan)
	}

	s := snap(t, st, store.ManagedByFile)
	if len(s.sinks) != 1 || s.sinks[0].Name != "ci-webhook" || s.sinks[0].Kind != "webhook" ||
		string(s.sinks[0].ConfigEncrypted) != `{"url":"https://ci.example.org/hooks/angelia","secret_env":"CI_WEBHOOK_SECRET","timeout":"10s"}` {
		t.Fatalf("sinks: %+v", s.sinks)
	}
	if len(s.watches) != 4 || len(s.subs) != 2 {
		t.Fatalf("watches=%d subs=%d", len(s.watches), len(s.subs))
	}
	byName := map[string]store.Watch{}
	for _, w := range s.watches {
		if w.BaselineDone || w.ManagedBy != store.ManagedByFile {
			t.Fatalf("watch %s: %+v", w.Name, w)
		}
		byName[w.Name] = w
	}
	req := byName["pypi-requests"]
	if req.Labels["team"] != "platform" || req.Labels["tier"] != "critical" ||
		string(req.Policy) != `{"prerelease":"none","include":[],"exclude":[],"bump":[],"cooldown":"72h0m0s","baseline":true}` {
		t.Fatalf("pypi-requests: %+v %s", req, req.Policy)
	}
	if got := string(byName["gh-cli"].Policy); !strings.Contains(got, `"scheme":"semver"`) {
		t.Fatalf("gh-cli policy: %s", got)
	}
	if got := string(byName["npm-react"].Policy); !strings.Contains(got, `"constraint":">=18 <20"`) || !strings.Contains(got, `"bump":["major","minor"]`) {
		t.Fatalf("npm-react policy: %s", got)
	}

	for _, c := range []struct {
		kind     string
		coords   map[string]string
		interval time.Duration
	}{
		{"pypi", map[string]string{"name": "requests"}, time.Hour},
		{"pypi", map[string]string{"name": "pydantic"}, 30 * time.Minute},
		{"npm", map[string]string{"name": "react"}, 2 * time.Hour},
		{"github-release", map[string]string{"repo": "cli/cli"}, 6 * time.Hour},
	} {
		tg, err := target(t, st, c.kind, c.coords)
		if err != nil {
			t.Fatalf("target %v: %v", c.coords, err)
		}
		if tg.Interval != c.interval || !tg.NextCheckAt.Equal(clock) {
			t.Fatalf("target %v: interval %s next %s", c.coords, tg.Interval, tg.NextCheckAt)
		}
	}
	sub := map[string]store.Subscription{}
	for _, x := range s.subs {
		sub[x.Name] = x
		if x.SinkID != s.sinks[0].ID {
			t.Fatalf("subscription %s points at %s", x.Name, x.SinkID)
		}
	}
	if got := string(sub["platform-to-ci"].Filter); got != `{"source":[],"labels":{"team":"platform"},"bump":[],"type":["release.published"]}` {
		t.Fatalf("filter: %s", got)
	}
	if got := string(sub["web-and-tooling-bumps"].Filter); got != `{"source":["npm","github-release"],"labels":{},"bump":["major","minor"],"package":"re*","type":["release.published"]}` {
		t.Fatalf("filter: %s", got)
	}

	// Idempotent: a second run changes nothing.
	again, err := Apply(t.Context(), st, f, applyOpts())
	if err != nil {
		t.Fatal(err)
	}
	if !again.Empty() || again.String() != "" {
		t.Fatalf("second run:\n%s", again)
	}
	if snap(t, st, store.ManagedByFile).watches[0].ID != s.watches[0].ID {
		t.Fatal("ids changed")
	}
}

const twoWatches = base + `
sinks:
  - {name: hook, kind: webhook, config: {url: 'https://h.example.org/x', secret_env: S}}
watches:
  - {name: req, source: pypi, coordinates: {name: requests}, interval: 1h, labels: {team: a}}
  - {name: django, source: pypi, coordinates: {name: django}}
subscriptions:
  - {name: all, sink: hook}
  - {name: teamb, sink: hook, filter: {labels: {team: b}}}
`

func ev(id, watchID string, subIDs ...string) (store.Event, []store.Delivery) {
	e := store.Event{ID: id, WatchID: watchID, Type: "release.published", Payload: []byte(`{}`), CreatedAt: clock}
	var ds []store.Delivery
	for _, s := range subIDs {
		ds = append(ds, store.Delivery{ID: "d-" + id + "-" + s, SubscriptionID: s, Status: store.DeliveryPending})
	}
	return e, ds
}

// AC: removing a watch from the file removes it, and the future deliveries of
// a removed subscription; history stays (D8).
func TestRemovingFromTheFile(t *testing.T) {
	st := openStore(t)
	mustApply(t, st, twoWatches)
	s := snap(t, st, store.ManagedByFile)
	subs := map[string]string{}
	for _, x := range s.subs {
		subs[x.Name] = x.ID
	}
	watches := map[string]store.Watch{}
	for _, w := range s.watches {
		watches[w.Name] = w
	}
	// Known versions on the django target, an event for each watch, deliveries
	// in each state for the subscription that will go.
	ctx := t.Context()
	if err := st.InTx(ctx, func(tx store.Tx) error {
		if _, err := tx.AddVersion(ctx, store.Version{TargetID: watches["django"].TargetID, Raw: "5.0", Normalized: "5.0", FirstSeenAt: clock}); err != nil {
			return err
		}
		e, ds := ev("e1", watches["django"].ID, subs["teamb"], subs["all"])
		ds[0].Status = store.DeliveryPending
		if err := tx.AppendEvent(ctx, e, ds); err != nil {
			return err
		}
		e2, ds2 := ev("e2", watches["django"].ID, subs["teamb"])
		ds2[0].Status = store.DeliveryDelivered
		if err := tx.AppendEvent(ctx, e2, ds2); err != nil {
			return err
		}
		e3, ds3 := ev("e3", watches["req"].ID, subs["teamb"])
		ds3[0].Status = store.DeliveryInFlight
		return tx.AppendEvent(ctx, e3, ds3)
	}); err != nil {
		t.Fatal(err)
	}

	// Remove the django watch and the teamb subscription.
	plan := mustApply(t, st, strings.Replace(strings.Replace(twoWatches,
		"  - {name: django, source: pypi, coordinates: {name: django}}\n", "", 1),
		"  - {name: teamb, sink: hook, filter: {labels: {team: b}}}\n", "", 1))
	if plan.Count(KindWatch, Delete) != 1 || plan.Count(KindSubscription, Delete) != 1 || plan.Count(KindTarget, Delete) != 1 {
		t.Fatalf("plan:\n%s", plan)
	}
	after := snap(t, st, store.ManagedByFile)
	if len(after.watches) != 1 || after.watches[0].Name != "req" || len(after.subs) != 1 || after.subs[0].Name != "all" {
		t.Fatalf("after: %+v", after)
	}
	if _, err := target(t, st, "pypi", map[string]string{"name": "django"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("django target should be gone: %v", err)
	}
	// Pending deliveries of the removed subscription are gone; in-flight and
	// delivered stay; the kept subscription's pending delivery is still queued.
	status := func(eventID string) []string {
		ds, err := st.ListDeliveries(ctx, eventID)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, d := range ds {
			out = append(out, d.SubscriptionID+":"+d.Status)
		}
		return out
	}
	if got := status("e1"); len(got) != 1 || got[0] != subs["all"]+":pending" {
		t.Fatalf("e1 deliveries: %v", got)
	}
	if got := status("e2"); len(got) != 1 || !strings.HasSuffix(got[0], ":delivered") {
		t.Fatalf("e2 deliveries: %v", got)
	}
	if got := status("e3"); len(got) != 1 || !strings.HasSuffix(got[0], ":in_flight") {
		t.Fatalf("e3 deliveries: %v", got)
	}
	evs, err := st.ListEventsAfter(ctx, 0, 10)
	if err != nil || len(evs) != 3 {
		t.Fatalf("events must be kept: %v %v", evs, err)
	}
	// The known versions of the deleted target went with it.
	if vs, err := st.ListVersions(ctx, watches["django"].TargetID); err != nil || len(vs) != 0 {
		t.Fatalf("versions: %v %v", vs, err)
	}
}

func TestEmptyFileDeletesWhatItManagesAndNothingElse(t *testing.T) {
	st := openStore(t)
	ctx := t.Context()
	// An API-managed set, which a file apply must never touch.
	mustNoErr(t, st.CreateTarget(ctx, store.Target{ID: "apit", Kind: "pypi", Coordinates: map[string]string{"name": "requests"}, NextCheckAt: clock}))
	mustNoErr(t, st.CreateWatch(ctx, store.Watch{ID: "apiw", TargetID: "apit", Name: "req"}))
	mustNoErr(t, st.CreateSink(ctx, store.Sink{ID: "apis", Kind: "webhook", Name: "hook"}))
	mustNoErr(t, st.CreateSubscription(ctx, store.Subscription{ID: "apisub", SinkID: "apis", Name: "all"}))

	// The file declares the same names and the same upstream: separate rows.
	mustApply(t, st, twoWatches)
	if got := snap(t, st, store.ManagedByFile); len(got.watches) != 2 || len(got.sinks) != 1 || len(got.subs) != 2 {
		t.Fatalf("file rows: %+v", got)
	}
	mustApply(t, st, base)
	if got := snap(t, st, store.ManagedByFile); len(got.watches)+len(got.sinks)+len(got.subs) != 0 {
		t.Fatalf("file rows left: %+v", got)
	}
	api := snap(t, st, store.ManagedByAPI)
	if len(api.watches) != 1 || len(api.sinks) != 1 || len(api.subs) != 1 || api.watches[0].ID != "apiw" {
		t.Fatalf("api rows touched: %+v", api)
	}
	// The shared target is kept because the API watch still uses it.
	if _, err := st.GetTarget(ctx, "apit"); err != nil {
		t.Fatal(err)
	}
}

func mustNoErr(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestUpdateKeepsIdAndBaselineUnlessTargetChanges(t *testing.T) {
	st := openStore(t)
	mustApply(t, st, twoWatches)
	ctx := t.Context()
	before := snap(t, st, store.ManagedByFile).watches
	req := before[1] // ordered by name: django, req
	if req.Name != "req" {
		t.Fatalf("order: %v", before)
	}
	mustNoErr(t, st.MarkBaselineDone(ctx, req.ID))

	// Labels, policy and interval change: id and baseline stay.
	plan := mustApply(t, st, strings.Replace(twoWatches,
		"interval: 1h, labels: {team: a}", "interval: 30m, labels: {team: z}, policy: {cooldown: 1h}", 1))
	if plan.Count(KindWatch, Update) != 1 || plan.Count(KindWatch, Create) != 0 {
		t.Fatalf("plan:\n%s", plan)
	}
	got, err := st.GetWatch(ctx, req.ID)
	mustNoErr(t, err)
	if !got.BaselineDone || got.Labels["team"] != "z" || !strings.Contains(string(got.Policy), `"cooldown":"1h0m0s"`) {
		t.Fatalf("watch: %+v %s", got, got.Policy)
	}
	tg, err := st.GetTarget(ctx, req.TargetID)
	mustNoErr(t, err)
	if tg.Interval != 30*time.Minute || !tg.NextCheckAt.Equal(clock) {
		t.Fatalf("target: %+v", tg)
	}

	// Changing coordinates points the same watch at another target and resets
	// the baseline; the old target goes with its known set.
	mustNoErr(t, st.InTx(ctx, func(tx store.Tx) error {
		_, err := tx.AddVersion(ctx, store.Version{TargetID: req.TargetID, Raw: "1.0", Normalized: "1.0", FirstSeenAt: clock})
		return err
	}))
	plan = mustApply(t, st, strings.Replace(twoWatches, "coordinates: {name: requests}", "coordinates: {name: httpx}", 1))
	if plan.Count(KindTarget, Create) != 1 || plan.Count(KindTarget, Delete) != 1 {
		t.Fatalf("plan:\n%s", plan)
	}
	got, err = st.GetWatch(ctx, req.ID)
	mustNoErr(t, err)
	if got.BaselineDone || got.TargetID == req.TargetID {
		t.Fatalf("watch after move: %+v", got)
	}
	if _, err := st.GetTarget(ctx, req.TargetID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old target: %v", err)
	}
	if vs, _ := st.ListVersions(ctx, req.TargetID); len(vs) != 0 {
		t.Fatalf("old versions kept: %v", vs)
	}
}

func TestRenameIsDeleteAndCreate(t *testing.T) {
	st := openStore(t)
	mustApply(t, st, twoWatches)
	old := snap(t, st, store.ManagedByFile).watches
	plan := mustApply(t, st, strings.Replace(twoWatches, "name: django", "name: django2", 1))
	if plan.Count(KindWatch, Create) != 1 || plan.Count(KindWatch, Delete) != 1 {
		t.Fatalf("plan:\n%s", plan)
	}
	for _, w := range snap(t, st, store.ManagedByFile).watches {
		if w.Name == "django2" && w.ID == old[0].ID {
			t.Fatal("rename kept the id")
		}
	}
}

func TestSharedTargetUsesSmallestInterval(t *testing.T) {
	st := openStore(t)
	mustApply(t, st, base+`
watches:
  - {name: a, source: pypi, coordinates: {name: requests}, interval: 2h}
  - {name: b, source: pypi, coordinates: {name: requests}, interval: 15m, labels: {x: y}}
`)
	tg, err := target(t, st, "pypi", map[string]string{"name": "requests"})
	mustNoErr(t, err)
	if tg.Interval != 15*time.Minute {
		t.Fatalf("interval = %s", tg.Interval)
	}
	ws := snap(t, st, store.ManagedByFile).watches
	if len(ws) != 2 || ws[0].TargetID != ws[1].TargetID {
		t.Fatalf("watches do not share a target: %+v", ws)
	}
	// Dropping b widens the interval and keeps the target.
	plan := mustApply(t, st, base+`
watches:
  - {name: a, source: pypi, coordinates: {name: requests}, interval: 2h}
`)
	tg, err = target(t, st, "pypi", map[string]string{"name": "requests"})
	mustNoErr(t, err)
	_ = plan
	if tg.Interval != 2*time.Hour {
		t.Fatalf("interval after removing b = %s (apply keeps the target's interval in step with the file)", tg.Interval)
	}
}

func TestSinkChangeAndRepointedSubscription(t *testing.T) {
	st := openStore(t)
	mustApply(t, st, twoWatches)
	plan := mustApply(t, st, strings.Replace(twoWatches, "https://h.example.org/x", "https://h2.example.org/x", 1))
	if plan.Count(KindSink, Update) != 1 {
		t.Fatalf("plan:\n%s", plan)
	}
	// Move the subscriptions to a new sink and delete the old one in one run.
	moved := strings.NewReplacer("name: hook,", "name: other,", "sink: hook", "sink: other").Replace(twoWatches)
	plan = mustApply(t, st, moved)
	if plan.Count(KindSink, Create) != 1 || plan.Count(KindSink, Delete) != 1 || plan.Count(KindSubscription, Update) != 2 {
		t.Fatalf("plan:\n%s", plan)
	}
}

// Apply is all-or-nothing: a failure rolls everything back.
func TestFailedApplyRollsBack(t *testing.T) {
	st := openStore(t)
	ctx := t.Context()
	mustApply(t, st, twoWatches)
	// An API subscription uses the file's sink; deleting the sink must fail the
	// whole apply, including the watch created earlier in the transaction.
	sinkID := snap(t, st, store.ManagedByFile).sinks[0].ID
	mustNoErr(t, st.CreateSubscription(ctx, store.Subscription{ID: "apisub", SinkID: sinkID, Name: "api"}))
	before := snap(t, st, store.ManagedByFile)
	_, err := Apply(ctx, st, mustParse(t, base+"watches:\n  - {name: new, source: npm, coordinates: {name: react}}\n"), applyOpts())
	if !errors.Is(err, store.ErrInvalidRef) {
		t.Fatalf("err = %v, want ErrInvalidRef", err)
	}
	after := snap(t, st, store.ManagedByFile)
	if len(after.watches) != len(before.watches) || len(after.sinks) != 1 || len(after.subs) != 2 {
		t.Fatalf("not rolled back: %+v", after)
	}
	if _, err := target(t, st, "npm", map[string]string{"name": "react"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("created target survived: %v", err)
	}
}

func TestPlanFileWritesNothing(t *testing.T) {
	st := openStore(t)
	f := mustParse(t, twoWatches)
	plan, err := PlanFile(t.Context(), st, f, applyOpts())
	mustNoErr(t, err)
	if plan.Count(KindWatch, Create) != 2 || plan.Count(KindTarget, Create) != 2 || plan.Count(KindSink, Create) != 1 {
		t.Fatalf("plan:\n%s", plan)
	}
	if got := snap(t, st, store.ManagedByFile); len(got.watches)+len(got.sinks)+len(got.subs) != 0 {
		t.Fatalf("dry run wrote rows: %+v", got)
	}
	mustApply(t, st, twoWatches)
	// A dry run of a deletion reports it without doing it.
	plan, err = PlanFile(t.Context(), st, mustParse(t, base), applyOpts())
	mustNoErr(t, err)
	if plan.Count(KindWatch, Delete) != 2 || plan.Count(KindTarget, Delete) != 2 || plan.Count(KindSubscription, Delete) != 2 || plan.Count(KindSink, Delete) != 1 {
		t.Fatalf("plan:\n%s", plan)
	}
	if got := snap(t, st, store.ManagedByFile); len(got.watches) != 2 {
		t.Fatal("dry run deleted rows")
	}
}

func TestApplyWrapsStoreErrors(t *testing.T) {
	st := openStore(t)
	mustNoErr(t, st.Close())
	_, err := Apply(t.Context(), st, mustParse(t, base), applyOpts())
	if err == nil || !strings.Contains(err.Error(), "apply watch file") {
		t.Fatalf("err = %v", err)
	}
}

func TestPlanStringAndDefaults(t *testing.T) {
	st := openStore(t)
	// Default options: real clock and ULIDs.
	p, err := Apply(t.Context(), st, mustParse(t, twoWatches), ApplyOptions{})
	mustNoErr(t, err)
	if !strings.Contains(p.String(), "create    watch req (pypi)") || p.Empty() {
		t.Fatalf("plan:\n%s", p)
	}
	for _, w := range snap(t, st, store.ManagedByFile).watches {
		if len(w.ID) != 26 {
			t.Fatalf("id %q is not a ULID", w.ID)
		}
	}
}

// A target one watch moves to is not deleted because another watch leaves it.
func TestSwapDoesNotDeleteTargetAWatchMovesTo(t *testing.T) {
	doc := func(a, b string) string {
		return base + "\nwatches:\n  - {name: a, source: pypi, coordinates: {name: " + a + "}}\n  - {name: b, source: pypi, coordinates: {name: " + b + "}}\n"
	}
	st := openStore(t)
	mustApply(t, st, doc("x", "y"))
	plan := mustApply(t, st, doc("y", "z"))
	for _, c := range plan.Changes {
		if c.Kind == KindTarget && c.Action == Delete {
			if tg, _ := target(t, st, "pypi", map[string]string{"name": "y"}); c.Name == tg.ID {
				t.Fatalf("plan deletes target y:\n%s", plan)
			}
		}
	}
	if plan.Count(KindTarget, Delete) != 1 || plan.Count(KindTarget, Create) != 1 {
		t.Fatalf("plan:\n%s", plan)
	}
	if _, err := target(t, st, "pypi", map[string]string{"name": "y"}); err != nil {
		t.Fatalf("target y: %v", err)
	}
	if _, err := target(t, st, "pypi", map[string]string{"name": "x"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("target x: %v", err)
	}
	// A dry run reports the same.
	f := mustParse(t, doc("z", "y"))
	p2, err := PlanFile(t.Context(), st, f, applyOpts())
	mustNoErr(t, err)
	if p2.Count(KindTarget, Delete) != 0 {
		t.Fatalf("dry plan:\n%s", p2)
	}
}
