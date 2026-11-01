package policy

import (
	"bytes"
	"context"
	"database/sql"
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

// fixture is a SQLite store on disk with one target ("t", an npm package)
// and the watches given to newFixture; addTarget adds more.
type fixture struct {
	t     *testing.T
	path  string
	s     *sqlite.Store
	log   bytes.Buffer
	kinds map[string]string // target ID -> kind
}

func newFixture(t *testing.T, watches ...store.Watch) *fixture {
	t.Helper()
	f := &fixture{t: t, path: filepath.Join(t.TempDir(), "angelia.db"), kinds: map[string]string{}}
	f.open()
	ctx := t.Context()
	f.addTarget("t", "npm")
	for _, w := range watches {
		w.TargetID = "t"
		if err := f.s.CreateWatch(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *fixture) addTarget(id, kind string) {
	f.t.Helper()
	coords := map[string]string{"name": "x-" + id}
	if kind == "github-tag" || kind == "github-release" {
		coords = map[string]string{"repo": "example/" + id}
	}
	if err := f.s.CreateTarget(f.t.Context(), store.Target{ID: id, Kind: kind, Coordinates: coords, NextCheckAt: t0}); err != nil {
		f.t.Fatal(err)
	}
	f.kinds[id] = kind
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

// check runs diff.Apply then Apply on target "t" in one transaction at now
// and returns the emitted releases by watch ID.
func (f *fixture) check(now time.Time, res source.Result) map[string][]Release {
	f.t.Helper()
	got, err := f.checkOn("t", now, res, nil)
	if err != nil {
		f.t.Fatalf("check at %v: %v", now, err)
	}
	return got
}

// checkOn runs a check of target at now, as the scheduler would: diff.Apply,
// Apply, then after (the caller's work in the same transaction, such as
// appending the events), and commits only if all succeed.
func (f *fixture) checkOn(target string, now time.Time, res source.Result, after func(store.Tx) error) (map[string][]Release, error) {
	ctx := f.t.Context()
	kind := f.kinds[target]
	logger := slog.New(slog.NewTextHandler(&f.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	schemeFor := SchemeFor(kind, logger)
	got := map[string][]Release{}
	err := f.s.InTx(ctx, func(tx store.Tx) error {
		def, _ := schemes.Select(kind, "", logger)
		out, err := diff.Apply(ctx, tx, diff.Input{TargetID: target, Fetched: res, Scheme: def, SchemeFor: schemeFor, Now: now, Logger: logger})
		if err != nil {
			return err
		}
		wrs, err := Apply(ctx, tx, out, Input{
			TargetID: target, SchemeFor: schemeFor, Now: now, Logger: logger, TrustPublishedAt: TrustsPublishedAt(kind),
		})
		if err != nil {
			return err
		}
		for _, wr := range wrs {
			got[wr.Watch.ID] = wr.Releases
		}
		if after != nil {
			return after(tx)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return got, nil
}

func (f *fixture) held(watchID string) []string {
	f.t.Helper()
	return f.heldOn("t", watchID)
}

func (f *fixture) heldOn(target, watchID string) []string {
	f.t.Helper()
	ps, err := f.s.ListPendingReleases(f.t.Context(), target)
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

// TestApplyRetargetedWatch: a watch moved to another target while it holds
// a version, and the same raw version is then held on the new target. The
// rows are keyed by target, so the check does not fail with a conflict and
// each target's row is released on its own.
func TestApplyRetargetedWatch(t *testing.T) {
	f := newFixture(t, store.Watch{ID: "w", Policy: policyJSON(`{"cooldown":"1h"}`)})
	f.addTarget("t2", "npm")
	base := v("1.0.0", time.Time{})
	f.check(t0, listed(base))
	f.check(t0, listed(v("1.1.0", time.Time{}), base))
	if !slices.Equal(f.held("w"), []string{"1.1.0"}) {
		t.Fatalf("held on t: %q", f.held("w"))
	}

	// The watch-file apply (#48) re-targets the watch; it does not call
	// DeletePendingReleasesOfWatch here, the worst case.
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE watch SET target_id = 't2' WHERE id = 'w'`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	// t2 then lists 1.1.0 too: it is held on t2 next to the stale row on t
	// (before the fix, AddPendingRelease failed with ErrConflict and the
	// whole check rolled back, on every check).
	// Its first fill (several versions) is a baseline for every watch.
	old := v("0.9.0", time.Time{})
	if _, err := f.checkOn("t2", t0.Add(5*time.Minute), listed(base, old), nil); err != nil {
		t.Fatalf("first check of the new target: %v", err)
	}
	if _, err := f.checkOn("t2", t0.Add(10*time.Minute), listed(v("1.1.0", time.Time{}), base, old), nil); err != nil {
		t.Fatalf("check of the new target: %v", err)
	}
	if got := f.heldOn("t2", "w"); !slices.Equal(got, []string{"1.1.0"}) {
		t.Fatalf("held on t2: %q", got)
	}
	got, err := f.checkOn("t2", t0.Add(2*time.Hour), source.Result{NotModified: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := raws(got["w"]); !slices.Equal(r, []string{"1.1.0"}) {
		t.Fatalf("released on t2: %q", r)
	}
	if got := f.heldOn("t2", "w"); len(got) != 0 {
		t.Fatalf("still held on t2: %q", got)
	}
	// The stale row on t is not listed for t's checks and is forgotten
	// with DeletePendingReleasesOfWatch.
	if got := f.check(t0.Add(3*time.Hour), source.Result{NotModified: true}); len(got) != 0 {
		t.Fatalf("t released a moved watch's row: %v", got)
	}
	if n, err := f.s.DeletePendingReleasesOfWatch(t.Context(), "w"); err != nil || n != 1 {
		t.Fatalf("DeletePendingReleasesOfWatch = %d, %v; want the stale row on t", n, err)
	}
}

// TestApplyRollback: when the caller's work after Apply fails (appending the
// events, #106), the transaction rolls back: held rows are as before and the
// released versions come out again at the next check.
func TestApplyRollback(t *testing.T) {
	f := newFixture(t, store.Watch{ID: "w", Policy: policyJSON(`{"cooldown":"1h"}`)})
	base := v("1.0.0", time.Time{})
	f.check(t0, listed(base))
	f.check(t0, listed(v("1.1.0", time.Time{}), base))
	errAppend := errors.New("append events failed")
	fail := func(store.Tx) error { return errAppend }

	// A new version to hold and a held one to release, then a failure.
	got, err := f.checkOn("t", t0.Add(2*time.Hour), listed(v("1.2.0", time.Time{}), v("1.1.0", time.Time{}), base), fail)
	if !errors.Is(err, errAppend) || got != nil {
		t.Fatalf("checkOn = %v, %v; want the caller's error", got, err)
	}
	if h := f.held("w"); !slices.Equal(h, []string{"1.1.0"}) {
		t.Fatalf("held after rollback: %q, want unchanged", h)
	}
	// Nothing was emitted: the next check releases 1.1.0 and holds 1.2.0.
	got = f.check(t0.Add(2*time.Hour), listed(v("1.2.0", time.Time{}), v("1.1.0", time.Time{}), base))
	if r := raws(got["w"]); !slices.Equal(r, []string{"1.1.0"}) || !slices.Equal(f.held("w"), []string{"1.2.0"}) {
		t.Fatalf("after retry: emitted %q, held %q", r, f.held("w"))
	}
}

// TestApplyUntrustedPublishedAt: a github-tag target's tag dated 2020 is
// held for the full cooldown from detection; an npm version with the same
// date emits at once.
func TestApplyUntrustedPublishedAt(t *testing.T) {
	f := newFixture(t, store.Watch{ID: "npm", Policy: policyJSON(`{"cooldown":"72h"}`)})
	f.addTarget("tag", "github-tag")
	ctx := t.Context()
	if err := f.s.CreateWatch(ctx, store.Watch{ID: "tag", TargetID: "tag", Policy: policyJSON(`{"cooldown":"72h"}`)}); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	base := v("1.0.0", old)
	for _, target := range []string{"t", "tag"} {
		if _, err := f.checkOn(target, t0, listed(base), nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.checkOn("t", t0, listed(v("1.1.0", old), base), nil)
	if err != nil || !slices.Equal(raws(got["npm"]), []string{"1.1.0"}) {
		t.Fatalf("npm: %v, %v", got, err)
	}
	for _, at := range []time.Duration{0, 72*time.Hour - time.Minute} {
		got, err := f.checkOn("tag", t0.Add(at), listed(v("1.1.0", old), base), nil)
		if err != nil || len(got) != 0 {
			t.Fatalf("github-tag at +%v: %v, %v; want held", at, got, err)
		}
	}
	got, err = f.checkOn("tag", t0.Add(72*time.Hour), listed(v("1.1.0", old), base), nil)
	if err != nil || !slices.Equal(raws(got["tag"]), []string{"1.1.0"}) || !got["tag"][0].CooldownSatisfiedAt.Equal(t0.Add(72*time.Hour)) {
		t.Fatalf("github-tag after the cooldown: %+v, %v", got, err)
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

func (f failTx) DeletePendingRelease(context.Context, string, string, string) error {
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
