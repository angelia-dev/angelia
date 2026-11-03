package event

import (
	"encoding/json"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
	"github.com/angelia-dev/angelia/internal/ulid"
)

// typesFixture is an npm target with one watch per prerelease mode, after a
// baseline check, and one subscription per event type filter.
func typesFixture(t *testing.T) (*sqlite.Store, store.Target, map[string]string) {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "a.db"), slog.New(slog.DiscardHandler))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	tg := store.Target{ID: "t-lib", Kind: "npm", Coordinates: map[string]string{"name": "lib"}, NextCheckAt: check1}
	must(t, st.CreateTarget(ctx, tg))
	watches := map[string]string{}
	for _, mode := range []string{"none", "only", "include"} {
		id := ulid.New(check1)
		watches[mode] = id
		must(t, st.CreateWatch(ctx, store.Watch{
			ID: id, TargetID: tg.ID, Labels: map[string]string{"mode": mode},
			Policy: json.RawMessage(`{"prerelease":"` + mode + `"}`),
		}))
	}
	must(t, st.CreateSink(ctx, store.Sink{ID: "k", Kind: "webhook", ConfigEncrypted: []byte{1}}))
	for id, f := range map[string]string{
		"s-published":  `{"type":["release.published"]}`,
		"s-prerelease": `{"type":["release.prerelease"]}`,
		"s-yanked":     `{"type":["release.yanked"]}`,
		"s-pub-yank":   `{"type":["release.published","release.yanked"]}`,
		"s-any":        `{}`,
	} {
		must(t, st.CreateSubscription(ctx, store.Subscription{ID: id, SinkID: "k", Filter: json.RawMessage(f)}))
	}
	_, err = Commit(ctx, st, CheckInput{Target: tg, Instance: "angelia", Now: check1, Fetched: source.Result{
		Versions: []source.Version{{Raw: "1.1.0"}, {Raw: "1.0.0"}},
	}})
	must(t, err)
	return st, tg, watches
}

func npmCheck(tg store.Target, now time.Time, yanked ...string) CheckInput {
	vs := []source.Version{{Raw: "2.0.0-rc.1"}, {Raw: "1.2.0"}, {Raw: "1.1.0"}, {Raw: "1.0.0"}}
	for i := range vs {
		vs[i].Yanked = slices.Contains(yanked, vs[i].Raw)
	}
	return CheckInput{Target: tg, Instance: "angelia", Now: now, Fetched: source.Result{Versions: vs}}
}

// AC 2 of #112: a pre-release emits release.prerelease when the watch policy
// lets pre-releases through (include, only), never release.published; a
// final release emits release.published; prerelease none drops pre-releases.
func TestPrereleaseEmitsPrereleaseNeverPublished(t *testing.T) {
	st, tg, watches := typesFixture(t)
	ctx := t.Context()
	_, err := Commit(ctx, st, npmCheck(tg, check2))
	must(t, err)
	got := map[string][]string{} // watch mode -> "type version"
	modeOf := map[string]string{}
	for m, id := range watches {
		modeOf[id] = m
	}
	for _, e := range eventsAfter(t, st, 0) {
		var env Envelope
		must(t, json.Unmarshal(e.Payload, &env))
		if env.Data.Prerelease != (e.Type == TypePrerelease) {
			t.Errorf("%s of %s has prerelease %v", e.Type, env.Data.Version, env.Data.Prerelease)
		}
		got[modeOf[e.WatchID]] = append(got[modeOf[e.WatchID]], ShortType(e.Type)+" "+env.Data.Version)
	}
	for m := range got {
		slices.Sort(got[m])
	}
	want := map[string][]string{
		"none":    {"release.published 1.2.0"},
		"only":    {"release.prerelease 2.0.0-rc.1"},
		"include": {"release.prerelease 2.0.0-rc.1", "release.published 1.2.0"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events by watch mode = %v, want %v", got, want)
	}
}

// AC 3 of #112: subscriptions filter by event type. Each subscription gets
// exactly the types it lists (all of them when it lists none), for
// published, pre-release and yanked events.
func TestSubscriptionsFilterByEventType(t *testing.T) {
	st, tg, watches := typesFixture(t)
	ctx := t.Context()
	_, err := Commit(ctx, st, npmCheck(tg, check2))
	must(t, err)
	_, err = Commit(ctx, st, npmCheck(tg, check2.Add(time.Hour), "2.0.0-rc.1", "1.2.0"))
	must(t, err)

	got := map[string]map[string]int{} // subscription -> short type -> deliveries
	types := map[string]int{}
	for _, e := range eventsAfter(t, st, 0) {
		if e.WatchID != watches["include"] {
			continue
		}
		short := ShortType(e.Type)
		types[short]++
		for sub := range deliveriesBySub(t, st, e.ID) {
			if got[sub] == nil {
				got[sub] = map[string]int{}
			}
			got[sub][short]++
		}
	}
	// The include watch emitted 1.2.0 and 2.0.0-rc.1, then a yank of each.
	if !reflect.DeepEqual(types, map[string]int{"release.published": 1, "release.prerelease": 1, "release.yanked": 2}) {
		t.Fatalf("event types of the include watch = %v", types)
	}
	want := map[string]map[string]int{
		"s-published":  {"release.published": 1},
		"s-prerelease": {"release.prerelease": 1},
		"s-yanked":     {"release.yanked": 2},
		"s-pub-yank":   {"release.published": 1, "release.yanked": 2},
		"s-any":        {"release.published": 1, "release.prerelease": 1, "release.yanked": 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("deliveries by subscription and type = %v, want %v", got, want)
	}
}
