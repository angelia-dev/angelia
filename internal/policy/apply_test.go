package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/diff"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
	"github.com/angelia-dev/angelia/internal/version/calver"
	"github.com/angelia-dev/angelia/internal/version/schemes"
)

// fixture is one target ("t", an npm package) in a SQLite store on disk.
type fixture struct {
	t    *testing.T
	path string
	s    *sqlite.Store
	log  bytes.Buffer
}

func newFixture(t *testing.T, watches ...store.Watch) *fixture {
	t.Helper()
	f := &fixture{t: t, path: filepath.Join(t.TempDir(), "angelia.db")}
	f.open()
	ctx := t.Context()
	if err := f.s.CreateTarget(ctx, store.Target{ID: "t", Kind: "npm", Coordinates: map[string]string{"name": "x"}, NextCheckAt: t0}); err != nil {
		t.Fatal(err)
	}
	for _, w := range watches {
		w.TargetID = "t"
		if err := f.s.CreateWatch(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *fixture) open() {
	s, err := sqlite.Open(f.t.Context(), f.path, slog.New(slog.DiscardHandler))
	if err != nil {
		f.t.Fatal(err)
	}
	f.s = s
	f.t.Cleanup(func() { _ = s.Close() })
}

// restart closes the store and opens it again, as a process restart would.
func (f *fixture) restart() {
	if err := f.s.Close(); err != nil {
		f.t.Fatal(err)
	}
	f.open()
}

// check runs diff.Apply then Apply in one transaction at now and returns
// the emitted releases by watch ID.
func (f *fixture) check(now time.Time, res source.Result) map[string][]Release {
	f.t.Helper()
	ctx := f.t.Context()
	logger := slog.New(slog.NewTextHandler(&f.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	schemeFor := SchemeFor("npm", logger)
	got := map[string][]Release{}
	err := f.s.InTx(ctx, func(tx store.Tx) error {
		def, _ := schemes.Select("npm", "", logger)
		out, err := diff.Apply(ctx, tx, diff.Input{TargetID: "t", Fetched: res, Scheme: def, SchemeFor: schemeFor, Now: now, Logger: logger})
		if err != nil {
			return err
		}
		wrs, err := Apply(ctx, tx, out, Input{TargetID: "t", SchemeFor: schemeFor, Now: now, Logger: logger})
		for _, wr := range wrs {
			got[wr.Watch.ID] = wr.Releases
		}
		return err
	})
	if err != nil {
		f.t.Fatalf("check at %v: %v", now, err)
	}
	return got
}

func (f *fixture) held(watchID string) []string {
	f.t.Helper()
	ps, err := f.s.ListPendingReleases(f.t.Context(), "t")
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, p := range ps {
		if p.WatchID == watchID {
			out = append(out, p.Raw)
		}
	}
	return out
}

func listed(vs ...source.Version) source.Result { return source.Result{Versions: vs} }

func v(raw string, published time.Time) source.Version {
	return source.Version{Raw: raw, PublishedAt: published}
}

func yanked(raw string, published time.Time) source.Version {
	return source.Version{Raw: raw, PublishedAt: published, Yanked: true}
}

func policyJSON(s string) json.RawMessage { return json.RawMessage(s) }

// TestApplyCooldownYank is the cooldown acceptance criterion: a version
// yanked within the window never emits, even when it is un-yanked later; one
// that survives emits with cooldown_satisfied_at.
func TestApplyCooldownYank(t *testing.T) {
	f := newFixture(t,
		store.Watch{ID: "cool", Policy: policyJSON(`{"cooldown":"72h"}`)},
		store.Watch{ID: "now", Policy: policyJSON(`{}`)},
	)
	pub := t0
	base := v("1.0.0", t0.Add(-1000*time.Hour))

	if got := f.check(t0, listed(base)); len(got) != 0 {
		t.Fatalf("baseline emitted %v", got)
	}
	got := f.check(t0.Add(time.Hour), listed(v("1.1.0", pub), v("1.0.1", pub), base))
	if r := raws(got["now"]); !slices.Equal(r, []string{"1.0.1", "1.1.0"}) {
		t.Fatalf("watch without cooldown emitted %q", r)
	}
	if !got["now"][0].CooldownSatisfiedAt.IsZero() {
		t.Error("cooldown_satisfied_at set without a cooldown")
	}
	if len(got["cool"]) != 0 || !slices.Equal(f.held("cool"), []string{"1.0.1", "1.1.0"}) {
		t.Fatalf("cooldown watch: emitted %v, held %q", got["cool"], f.held("cool"))
	}
	if len(f.held("now")) != 0 {
		t.Fatal("watch without cooldown holds versions")
	}

	// 1.1.0 is yanked within the window: dropped at once.
	got = f.check(t0.Add(24*time.Hour), listed(yanked("1.1.0", pub), v("1.0.1", pub), base))
	if len(got) != 0 || !slices.Equal(f.held("cool"), []string{"1.0.1"}) {
		t.Fatalf("after yank: emitted %v, held %q", got, f.held("cool"))
	}
	if !strings.Contains(f.log.String(), "reason=yanked") {
		t.Errorf("yank drop not logged:\n%s", f.log.String())
	}

	// Still in the window, un-yanked: 1.1.0 does not come back.
	if got = f.check(t0.Add(48*time.Hour), listed(v("1.1.0", pub), v("1.0.1", pub), base)); len(got) != 0 {
		t.Fatalf("before the end of the window: %v", got)
	}

	// After the window: 1.0.1 emits with cooldown_satisfied_at = published + 72h.
	got = f.check(t0.Add(73*time.Hour), listed(v("1.1.0", pub), v("1.0.1", pub), base))
	if r := raws(got["cool"]); !slices.Equal(r, []string{"1.0.1"}) || len(got) != 1 {
		t.Fatalf("after the window: %v", got)
	}
	r := got["cool"][0]
	if !r.CooldownSatisfiedAt.Equal(pub.Add(72*time.Hour)) || r.Previous != "1.0.0" || r.Bump != "patch" || !r.DetectedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("released = %+v", r)
	}
	if len(f.held("cool")) != 0 {
		t.Fatalf("still held: %q", f.held("cool"))
	}
	// Emitted once only.
	if got = f.check(t0.Add(100*time.Hour), listed(v("1.1.0", pub), v("1.0.1", pub), base)); len(got) != 0 {
		t.Fatalf("emitted twice: %v", got)
	}
}

// TestApplyCooldownSurvivesRestart: held versions are in the store, so a
// restart in the window loses nothing, and a NotModified check (no changes)
// still releases them.
func TestApplyCooldownSurvivesRestart(t *testing.T) {
	f := newFixture(t, store.Watch{ID: "w", Policy: policyJSON(`{"cooldown":"1h","prerelease":"include"}`)})
	base := v("1.0.0", time.Time{})
	f.check(t0, listed(base))
	f.check(t0, listed(v("2.0.0-rc.1", time.Time{}), v("1.1.0", time.Time{}), base)) // no date: counts from detection
	if !slices.Equal(f.held("w"), []string{"1.1.0", "2.0.0-rc.1"}) {
		t.Fatalf("held %q", f.held("w"))
	}

	f.restart()
	if got := f.check(t0.Add(30*time.Minute), source.Result{NotModified: true}); len(got) != 0 {
		t.Fatalf("emitted in the window: %v", got)
	}
	f.restart()
	got := f.check(t0.Add(2*time.Hour), source.Result{NotModified: true})
	if r := raws(got["w"]); !slices.Equal(r, []string{"1.1.0", "2.0.0-rc.1"}) {
		t.Fatalf("after restart emitted %q", r)
	}
	for _, r := range got["w"] {
		if !r.CooldownSatisfiedAt.Equal(t0.Add(time.Hour)) || !r.DetectedAt.Equal(t0) {
			t.Errorf("%s: %+v", r.Raw, r)
		}
	}
	if got["w"][1].Prerelease != true || got["w"][1].Normalized != "2.0.0-rc.1" {
		t.Errorf("snapshot lost fields: %+v", got["w"][1])
	}
}

// TestApplyRemovedWhileHeld: a version upstream stops listing is kept but not
// emitted; it emits once listed again.
func TestApplyRemovedWhileHeld(t *testing.T) {
	f := newFixture(t, store.Watch{ID: "w", Policy: policyJSON(`{"cooldown":"1h"}`)})
	base := v("1.0.0", time.Time{})
	f.check(t0, listed(base))
	f.check(t0, listed(v("1.1.0", time.Time{}), base))
	if got := f.check(t0.Add(2*time.Hour), listed(base)); len(got) != 0 || !slices.Equal(f.held("w"), []string{"1.1.0"}) {
		t.Fatalf("removed: emitted %v, held %q", got, f.held("w"))
	}
	if got := f.check(t0.Add(3*time.Hour), listed(v("1.1.0", time.Time{}), base)); !slices.Equal(raws(got["w"]), []string{"1.1.0"}) {
		t.Fatalf("returned: %v", got)
	}
}

func TestApplyPerWatchScheme(t *testing.T) {
	f := newFixture(t,
		store.Watch{ID: "cal", Policy: policyJSON(`{"scheme":"calver","constraint":">=2026"}`)},
		store.Watch{ID: "sem", Policy: policyJSON(`{"constraint":">=2026"}`)},
	)
	base := v("2025.12.1", time.Time{})
	f.check(t0, listed(base))
	got := f.check(t0, listed(v("2026.1.0", time.Time{}), base))
	if !slices.Equal(raws(got["cal"]), []string{"2026.1.0"}) || got["cal"][0].Scheme != calver.Name {
		t.Fatalf("calver watch: %+v", got["cal"])
	}
	if !slices.Equal(raws(got["sem"]), []string{"2026.1.0"}) || got["sem"][0].Scheme != "semver" {
		t.Fatalf("semver watch: %+v", got["sem"])
	}
}

func TestApplyInvalidPolicySkipsWatch(t *testing.T) {
	f := newFixture(t,
		store.Watch{ID: "bad", Policy: policyJSON(`{"prerelease":"sometimes"}`)},
		store.Watch{ID: "badop", Policy: policyJSON(`{"constraint":">=latest"}`)},
		store.Watch{ID: "good"},
	)
	base := v("1.0.0", time.Time{})
	f.check(t0, listed(base))
	got := f.check(t0, listed(v("1.1.0", time.Time{}), base))
	if len(got) != 1 || !slices.Equal(raws(got["good"]), []string{"1.1.0"}) {
		t.Fatalf("emitted %v", got)
	}
	// Logged on every check (two here), with the number of lost changes.
	if n := strings.Count(f.log.String(), "invalid watch policy"); n != 4 || strings.Count(f.log.String(), "changes=1") != 2 {
		t.Fatalf("%d invalid-policy errors logged:\n%s", n, f.log.String())
	}
}

func TestSchemeFor(t *testing.T) {
	sf := SchemeFor("pypi", nil)
	for policy, want := range map[string]string{"": "pep440", "{}": "pep440", `{"scheme":"calver"}`: "calver", `{"scheme":"pep440"}`: "pep440"} {
		s, err := sf(store.Watch{Policy: policyJSON(policy)})
		if err != nil || s.Name() != want {
			t.Errorf("SchemeFor(%q) = %v, %v; want %s", policy, s, err, want)
		}
	}
	if _, err := sf(store.Watch{Policy: policyJSON(`{"scheme":1}`)}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad scheme type: %v", err)
	}
	if _, err := sf(store.Watch{Policy: policyJSON(`{"scheme":"maven"}`)}); err == nil {
		t.Error("unknown scheme accepted")
	}
}

// failTx fails the method named in fail; it holds one young version for w.
type failTx struct {
	store.Tx
	fail string
}

var errTx = errors.New("tx failed")

func (f failTx) err(m string) error {
	if f.fail == m {
		return errTx
	}
	return nil
}

func (f failTx) ListPendingReleases(context.Context, string) ([]store.PendingRelease, error) {
	b, _ := json.Marshal(Release{Raw: "0.9.0", DetectedAt: t0})
	return []store.PendingRelease{{WatchID: "w", TargetID: "t", Raw: "0.9.0", HeldAt: t0, Release: b}}, f.err("ListPendingReleases")
}

func (f failTx) AddPendingRelease(context.Context, store.PendingRelease) error {
	return f.err("AddPendingRelease")
}

func (f failTx) DeletePendingRelease(context.Context, string, string) error {
	return f.err("DeletePendingRelease")
}

func TestApplyErrors(t *testing.T) {
	ctx := t.Context()
	sf := SchemeFor("npm", nil)
	out := diff.Outcome{Watches: []diff.WatchChanges{{
		Watch:   store.Watch{ID: "w", TargetID: "t", Policy: policyJSON(`{"cooldown":"1h"}`)},
		Changes: newChanges(sem, []string{"1.0.0"}, fetched("1.1.0", "1.0.0"), t0),
	}}}
	if _, err := Apply(ctx, failTx{}, out, Input{TargetID: "t"}); err == nil {
		t.Error("nil SchemeFor accepted")
	}
	// Holding 1.1.0 and releasing 0.9.0 (cooldown over at t0+2h).
	for _, m := range []string{"ListPendingReleases", "AddPendingRelease", "DeletePendingRelease"} {
		if _, err := Apply(ctx, failTx{fail: m}, out, Input{TargetID: "t", SchemeFor: sf, Now: t0.Add(30 * time.Minute)}); m != "DeletePendingRelease" && !errors.Is(err, errTx) {
			t.Errorf("%s: %v", m, err)
		}
		if _, err := Apply(ctx, failTx{fail: m}, out, Input{TargetID: "t", SchemeFor: sf, Now: t0.Add(2 * time.Hour)}); !errors.Is(err, errTx) {
			t.Errorf("%s after the window: %v", m, err)
		}
	}
	wrs, err := Apply(ctx, failTx{}, out, Input{TargetID: "t", SchemeFor: sf, Now: t0.Add(2 * time.Hour)})
	if err != nil || len(wrs) != 1 || !slices.Equal(raws(wrs[0].Releases), []string{"0.9.0"}) {
		t.Fatalf("Apply = %+v, %v", wrs, err)
	}
}
