package diff

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
	"github.com/angelia-dev/angelia/internal/version"
	"github.com/angelia-dev/angelia/internal/version/schemes"
)

// checkKind runs Apply for a target of the given source kind, choosing each
// watch's scheme from a "scheme" key in its policy (the override).
func checkKind(t *testing.T, s *sqlite.Store, kind string, res source.Result) Outcome {
	t.Helper()
	def, err := schemes.Select(kind, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	schemeFor := func(w store.Watch) (version.Scheme, error) {
		var p struct{ Scheme string }
		if err := json.Unmarshal(w.Policy, &p); err != nil {
			return nil, err
		}
		return schemes.Select(kind, p.Scheme, nil)
	}
	var out Outcome
	err = s.InTx(t.Context(), func(tx store.Tx) error {
		var err error
		out, err = Apply(t.Context(), tx, Input{TargetID: "t", Fetched: res, Scheme: def, SchemeFor: schemeFor, Now: t1})
		return err
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return out
}

func TestApplyClassifiesPerWatchScheme(t *testing.T) {
	s := openStore(t,
		store.Watch{ID: "w-cal", Policy: json.RawMessage(`{"scheme":"calver"}`), BaselineDone: true},
		store.Watch{ID: "w-def", BaselineDone: true},
		store.Watch{ID: "w-sem", Policy: json.RawMessage(`{"scheme":"semver"}`), BaselineDone: true},
	)
	// The target is a github-tag (coercing semver by default). One version
	// first: a single first version is reported, not a baseline.
	checkKind(t, s, "github-tag", list("2026.9.0"))
	out := checkKind(t, s, "github-tag", list("2026.11.0", "2026.10.1.1", "2026.10.1", "2026.9.0!"))

	got := map[string][]string{}
	for _, wc := range out.Watches {
		got[wc.Watch.ID+"/"+wc.Scheme] = summaries(wc.Changes)
	}
	want := map[string][]string{
		// semver cannot parse four numbers: lexical fallback, ranked below
		// everything, so no previous version and no backport.
		"w-def/semver": {
			"new 2026.10.1.1 prev= bump=",
			"new 2026.10.1 prev=2026.9.0 bump=minor",
			"new 2026.11.0 prev=2026.10.1 bump=minor",
			"yanked 2026.9.0 yanked",
		},
		"w-sem/semver": { // an override naming the default keeps it
			"new 2026.10.1.1 prev= bump=",
			"new 2026.10.1 prev=2026.9.0 bump=minor",
			"new 2026.11.0 prev=2026.10.1 bump=minor",
			"yanked 2026.9.0 yanked",
		},
		"w-cal/calver": {
			"new 2026.10.1 prev=2026.9.0 bump=minor",
			"new 2026.10.1.1 prev=2026.10.1 bump=patch",
			"new 2026.11.0 prev=2026.10.1.1 bump=minor",
			"yanked 2026.9.0 yanked",
		},
	}
	for k, w := range want {
		g, ok := got[k]
		if !ok {
			t.Errorf("no watch %s in %v", k, got)
			continue
		}
		if !slices.Equal(g, w) {
			t.Errorf("%s:\n got %q\nwant %q", k, g, w)
		}
	}

	// The known set is written with the default scheme only.
	vs, err := s.ListVersions(t.Context(), "t")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		if v.Raw == "2026.10.1.1" && v.Normalized != "2026.10.1.1" {
			t.Errorf("row normalized with a watch scheme: %+v", v)
		}
		if v.Raw == "2026.10.1" && v.Normalized != "2026.10.1" {
			t.Errorf("row = %+v", v)
		}
	}

	// Each watch owns its slice.
	var a, b *WatchChanges
	for i := range out.Watches {
		switch out.Watches[i].Watch.ID {
		case "w-def":
			a = &out.Watches[i]
		case "w-sem":
			b = &out.Watches[i]
		}
	}
	a.Changes[0].Raw = "mutated"
	if b.Changes[0].Raw == "mutated" {
		t.Error("watches share a Changes slice")
	}
}

func TestApplyClassifiesBackportPerScheme(t *testing.T) {
	s := openStore(t,
		store.Watch{ID: "w-cal", Policy: json.RawMessage(`{"scheme":"calver"}`), BaselineDone: true},
		store.Watch{ID: "w-def", BaselineDone: true},
	)
	checkKind(t, s, "github-tag", list("2026.11.0"))
	out := checkKind(t, s, "github-tag", list("2026.11.0", "2026.10.1.1"))
	for _, wc := range out.Watches {
		c := wc.Changes[0]
		switch wc.Scheme {
		case "calver":
			if !c.Backport || c.Previous != "" {
				t.Errorf("calver: %s", summary(c))
			}
		default:
			if c.Backport || c.Previous != "" {
				t.Errorf("semver (fallback): %s", summary(c))
			}
		}
	}
}

func TestApplyEmptyFirstFetchThenBulkIsBaseline(t *testing.T) {
	s := openStore(t, store.Watch{ID: "w1"})
	if out := check(t, s, source.Result{}, nil); !out.Watches[0].Baseline {
		t.Fatalf("empty first fetch: %+v", out)
	}
	var many []string
	for i := range 50 {
		many = append(many, fmt.Sprintf("1.%d.0", i))
	}
	out := check(t, s, list(many...), nil)
	if len(out.Add) != 50 || !out.Watches[0].Baseline || len(out.Watches[0].Changes) != 0 {
		t.Fatalf("bulk fill after empty baseline: add=%d %+v", len(out.Add), out.Watches[0])
	}
	if w, _ := s.GetWatch(t.Context(), "w1"); !w.BaselineDone {
		t.Error("baseline_done lost")
	}
	// From then on, changes are reported again.
	out = check(t, s, list(append(many, "2.0.0")...), nil)
	if got := summaries(out.Watches[0].Changes); !slices.Equal(got, []string{"new 2.0.0 prev=1.49.0 bump=major"}) {
		t.Errorf("after bulk baseline: %q", got)
	}
}
