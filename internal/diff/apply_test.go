package diff

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
	"github.com/angelia-dev/angelia/internal/version"
	"github.com/angelia-dev/angelia/internal/version/schemes"
	"github.com/angelia-dev/angelia/internal/version/semver"
)

func openStore(t *testing.T, watches ...store.Watch) *sqlite.Store {
	t.Helper()
	ctx := t.Context()
	s, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "angelia.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.CreateTarget(ctx, store.Target{ID: "t", Kind: "npm", Coordinates: map[string]string{"name": "x"}, NextCheckAt: t0}); err != nil {
		t.Fatal(err)
	}
	for _, w := range watches {
		w.TargetID = "t"
		if err := s.CreateWatch(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// check runs Apply in its own committed transaction.
func check(t *testing.T, s *sqlite.Store, res source.Result, logger *slog.Logger) Outcome {
	t.Helper()
	var out Outcome
	err := s.InTx(t.Context(), func(tx store.Tx) error {
		var err error
		out, err = Apply(t.Context(), tx, Input{
			TargetID: "t", Fetched: res, Scheme: schemes.WithFallback(semver.Scheme{}, nil), Now: t1, Logger: logger,
		})
		return err
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return out
}

func list(vs ...string) source.Result { return source.Result{Versions: fetched(vs...)} }

func rawSet(t *testing.T, s *sqlite.Store) []string {
	t.Helper()
	vs, err := s.ListVersions(t.Context(), "t")
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Raw
		if v.Yanked {
			out[i] += "!"
		}
		if !v.RemovedAt.IsZero() {
			out[i] += "~"
		}
	}
	slices.Sort(out)
	return out
}

func TestApplyBaseline(t *testing.T) {
	s := openStore(t, store.Watch{ID: "w1"}, store.Watch{ID: "w2"})
	ctx := t.Context()

	out := check(t, s, list("2.0.0", "1.9.3", "1.0.0"), nil)
	if len(out.Add) != 3 || len(out.Watches) != 2 {
		t.Fatalf("outcome = %+v", out)
	}
	for _, wc := range out.Watches {
		if !wc.Baseline || len(wc.Changes) != 0 || wc.Watch.BaselineDone {
			t.Errorf("baseline watch %s: %+v", wc.Watch.ID, wc)
		}
		if w, _ := s.GetWatch(ctx, wc.Watch.ID); !w.BaselineDone {
			t.Errorf("watch %s: baseline_done not set", wc.Watch.ID)
		}
	}
	if got := rawSet(t, s); !slices.Equal(got, []string{"1.0.0", "1.9.3", "2.0.0"}) {
		t.Errorf("known set = %q", got)
	}
	if evs, _ := s.ListEventsAfter(ctx, 0, 10); len(evs) != 0 {
		t.Errorf("baseline wrote events: %v", evs)
	}

	// The next check reports changes to both watches; a watch added in
	// between gets its own baseline instead.
	if err := s.CreateWatch(ctx, store.Watch{ID: "w3", TargetID: "t"}); err != nil {
		t.Fatal(err)
	}
	out = check(t, s, list("2.0.0", "1.9.4", "1.9.3!", "1.0.0"), nil)
	var ids []string
	for _, wc := range out.Watches {
		ids = append(ids, wc.Watch.ID)
		switch wc.Watch.ID {
		case "w3":
			if !wc.Baseline || len(wc.Changes) != 0 {
				t.Errorf("w3: %+v", wc)
			}
		default:
			if wc.Baseline || !slices.Equal(summaries(wc.Changes), []string{"new 1.9.4 prev=1.9.3 bump=patch backport", "yanked 1.9.3 yanked"}) {
				t.Errorf("%s: baseline=%v changes=%q", wc.Watch.ID, wc.Baseline, summaries(wc.Changes))
			}
		}
	}
	if !slices.Equal(ids, []string{"w1", "w2", "w3"}) {
		t.Errorf("watches = %q", ids)
	}
}

func TestApplyBaselineOnEmptyAndNotModified(t *testing.T) {
	s := openStore(t, store.Watch{ID: "w1"})
	// A package with no release yet: the baseline is the empty set, so its
	// first release is reported.
	if out := check(t, s, source.Result{}, nil); !out.Watches[0].Baseline {
		t.Fatalf("empty first fetch: %+v", out)
	}
	out := check(t, s, list("0.1.0"), nil)
	if got := summaries(out.Watches[0].Changes); !slices.Equal(got, []string{"new 0.1.0 prev= bump="}) {
		t.Fatalf("first release: %q", got)
	}

	// NotModified: nothing compared, baseline still marked.
	ctx := t.Context()
	if err := s.CreateWatch(ctx, store.Watch{ID: "w2", TargetID: "t"}); err != nil {
		t.Fatal(err)
	}
	out = check(t, s, source.Result{NotModified: true}, nil)
	if !out.IsZero() || len(out.Watches) != 2 || out.Watches[0].Baseline || !out.Watches[1].Baseline {
		t.Fatalf("not modified: %+v", out)
	}
	if w, _ := s.GetWatch(ctx, "w2"); !w.BaselineDone {
		t.Error("w2 baseline_done not set")
	}
}

func TestApplyIdempotent(t *testing.T) {
	s := openStore(t, store.Watch{ID: "w1", BaselineDone: true})
	inputs := []source.Result{
		list("1.0.0", "1.1.0"),
		list("2.0.0", "1.0.1", "1.0.0", "1.1.0"),
		list("2.0.0!", "1.0.1", "1.1.0"),
		list("2.0.0", "1.0.1", "1.1.0", "1.0.0"),
		{},
	}
	for i, in := range inputs {
		first := check(t, s, in, nil)
		before := rawSet(t, s)
		again := check(t, s, in, nil)
		if !again.IsZero() || len(again.Watches[0].Changes) != 0 {
			t.Fatalf("input %d: second run (after %q) changed %q", i, summaries(first.Changes), summaries(again.Changes))
		}
		if after := rawSet(t, s); !slices.Equal(before, after) {
			t.Fatalf("input %d: known set %q became %q", i, before, after)
		}
	}
	if got := rawSet(t, s); !slices.Equal(got, []string{"1.0.0", "1.0.1", "1.1.0", "2.0.0"}) {
		t.Errorf("final known set = %q", got)
	}
}

func TestApplyRollsBackWithTheTransaction(t *testing.T) {
	s := openStore(t, store.Watch{ID: "w1"})
	boom := errors.New("boom")
	err := s.InTx(t.Context(), func(tx store.Tx) error {
		if _, err := Apply(t.Context(), tx, Input{TargetID: "t", Fetched: list("1.0.0"), Scheme: semver.Scheme{}, Now: t1}); err != nil {
			return err
		}
		return boom // e.g. writing the events failed
	})
	if !errors.Is(err, boom) {
		t.Fatalf("InTx = %v", err)
	}
	if got := rawSet(t, s); len(got) != 0 {
		t.Errorf("known set after rollback = %q", got)
	}
	if w, _ := s.GetWatch(t.Context(), "w1"); w.BaselineDone {
		t.Error("baseline survived the rollback")
	}
}

func TestApplyLogs(t *testing.T) {
	s := openStore(t, store.Watch{ID: "w1", BaselineDone: true})
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	check(t, s, list("1.0.0", "1.0.1", "1.0.2"), logger)
	buf.Reset()

	check(t, s, list("1.0.0", "nightly", "edge", "", "1.0.0"), logger)
	logs := buf.String()
	for _, want := range []string{
		`level=INFO msg="versions no longer listed upstream" target=t count=2 versions="[1.0.1 1.0.2]"`,
		`level=WARN msg="new versions do not match the scheme, using lexical order" target=t scheme=semver count=2 versions="[nightly edge]"`,
		`level=WARN msg="skipped blank, padded or repeated upstream versions" target=t count=2`,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("missing %s\nin\n%s", want, logs)
		}
	}
	if strings.Count(logs, "level=WARN") != 2 {
		t.Errorf("want two aggregated warnings:\n%s", logs)
	}

	buf.Reset()
	check(t, s, list("1.0.1", "1.0.0"), logger)
	check(t, s, list(" ", ""), logger) // no usable entry
	logs = buf.String()
	for _, want := range []string{
		`msg="removed versions listed upstream again" target=t count=1 versions=[1.0.1]`,
		`msg="upstream listed no usable versions; known set kept" target=t known=5`,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("missing %s\nin\n%s", want, logs)
		}
	}

	long := strings.Repeat("x", 300)
	if got := sample([]string{"a", "b", "c", "d", "e", "f", long}); len(got) != maxLogged {
		t.Errorf("sample = %q", got)
	}
	if got := sample([]string{long}); len(got[0]) != maxLoggedRaw+len("...") {
		t.Errorf("sample did not truncate: %d", len(got[0]))
	}
}

// failTx fails the method named in fail and otherwise behaves like a
// target with one known version and one watch without a baseline.
type failTx struct {
	fail string
	done bool // the watch's baseline is done
}

var errTx = errors.New("tx failed")

func (f failTx) err(m string) error {
	if f.fail == m {
		return errTx
	}
	return nil
}

func (f failTx) ListVersions(context.Context, string) ([]store.Version, error) {
	return known("1.0.0"), f.err("ListVersions")
}

func (f failTx) ListWatches(context.Context, string) ([]store.Watch, error) {
	return []store.Watch{{ID: "w", BaselineDone: f.done}}, f.err("ListWatches")
}

func (f failTx) AddVersion(context.Context, store.Version) (bool, error) {
	return f.fail != "dup", f.err("AddVersion")
}

func (f failTx) UpdateVersion(context.Context, store.Version) error { return f.err("UpdateVersion") }

func (f failTx) MarkBaselineDone(context.Context, string) error { return f.err("MarkBaselineDone") }

func (f failTx) ListPendingReleases(context.Context, string) ([]store.PendingRelease, error) {
	return nil, f.err("ListPendingReleases")
}

func (f failTx) AddPendingRelease(context.Context, store.PendingRelease) error {
	return f.err("AddPendingRelease")
}

func (f failTx) DeletePendingRelease(context.Context, string, string, string) error {
	return f.err("DeletePendingRelease")
}

func (f failTx) ListSubscriptions(context.Context) ([]store.Subscription, error) {
	return nil, f.err("ListSubscriptions")
}

func (f failTx) SubscriptionGeneration(context.Context) (int64, error) {
	return 0, f.err("SubscriptionGeneration")
}

func (f failTx) DeletePendingReleasesOfWatch(context.Context, string) (int64, error) {
	return 0, f.err("DeletePendingReleasesOfWatch")
}

func (f failTx) AppendEvent(context.Context, store.Event, []store.Delivery) error {
	return f.err("AppendEvent")
}
func (f failTx) UpdateTarget(context.Context, store.Target) error { return f.err("UpdateTarget") }

func TestApplyErrors(t *testing.T) {
	in := Input{TargetID: "t", Fetched: list("1.1.0"), Scheme: semver.Scheme{}, Now: t1}
	in.Fetched.Versions = append(in.Fetched.Versions, source.Version{Raw: "1.0.0", Yanked: true})
	for _, m := range []string{"ListVersions", "AddVersion", "UpdateVersion", "ListWatches", "MarkBaselineDone"} {
		if _, err := Apply(t.Context(), failTx{fail: m}, in); !errors.Is(err, errTx) {
			t.Errorf("%s failing: Apply = %v", m, err)
		}
	}
	if _, err := Apply(t.Context(), failTx{}, in); err != nil {
		t.Errorf("Apply = %v", err)
	}
	if _, err := Apply(t.Context(), failTx{fail: "dup"}, in); !errors.Is(err, store.ErrConflict) {
		t.Errorf("version already known: Apply = %v", err)
	}
	in.SchemeFor = func(store.Watch) (version.Scheme, error) { return nil, errTx }
	if _, err := Apply(t.Context(), failTx{done: true}, in); !errors.Is(err, errTx) {
		t.Errorf("SchemeFor failing: Apply = %v", err)
	}
	in.Scheme = nil
	if _, err := Apply(t.Context(), failTx{}, in); !errors.Is(err, errNoScheme) {
		t.Errorf("nil scheme: %v", err)
	}
}
