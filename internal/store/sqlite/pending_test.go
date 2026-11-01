package sqlite

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

func TestPendingRelease(t *testing.T) {
	s, path := openTest(t)
	seed(t, s)
	ctx := t.Context()
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w2", TargetID: "t1"}))
	for _, raw := range []string{"1.0.0", "1.1.0", "2.0.0"} {
		if _, err := s.AddVersion(ctx, store.Version{TargetID: "t1", Raw: raw, Normalized: raw, FirstSeenAt: t0}); err != nil {
			t.Fatal(err)
		}
	}
	rel := func(raw string) json.RawMessage { return json.RawMessage(`{"raw":"` + raw + `"}`) }
	p := func(w, raw string, at time.Time) store.PendingRelease {
		return store.PendingRelease{WatchID: w, TargetID: "t1", Raw: raw, HeldAt: at, Release: rel(raw)}
	}
	must(t, s.AddPendingRelease(ctx, p("w1", "1.1.0", t0.Add(time.Hour))))
	must(t, s.AddPendingRelease(ctx, p("w1", "1.0.0", t0)))
	must(t, s.InTx(ctx, func(tx store.Tx) error { return tx.AddPendingRelease(ctx, p("w2", "1.0.0", t0)) }))

	wantErr(t, s.AddPendingRelease(ctx, p("w1", "1.0.0", t0)), store.ErrConflict, "held twice")
	wantErr(t, s.AddPendingRelease(ctx, p("nope", "1.0.0", t0)), store.ErrInvalidRef, "missing watch")
	wantErr(t, s.AddPendingRelease(ctx, p("w1", "9.9.9", t0)), store.ErrInvalidRef, "unknown version")
	wantErr(t, s.AddPendingRelease(ctx, p("w1", "2.0.0", time.Time{})), store.ErrInvalid, "zero held_at")
	bad := p("w1", "2.0.0", t0)
	bad.Release = nil
	wantErr(t, s.AddPendingRelease(ctx, bad), store.ErrInvalid, "empty release")

	// The list reflects the known set's current state.
	must(t, s.UpdateVersion(ctx, store.Version{TargetID: "t1", Raw: "1.0.0", Yanked: true}))
	must(t, s.UpdateVersion(ctx, store.Version{TargetID: "t1", Raw: "1.1.0", RemovedAt: t0}))
	got, err := s.ListPendingReleases(ctx, "t1")
	must(t, err)
	want := []store.PendingRelease{
		{WatchID: "w1", TargetID: "t1", Raw: "1.0.0", HeldAt: t0, Release: rel("1.0.0"), Yanked: true},
		{WatchID: "w1", TargetID: "t1", Raw: "1.1.0", HeldAt: t0.Add(time.Hour), Release: rel("1.1.0"), Removed: true},
		{WatchID: "w2", TargetID: "t1", Raw: "1.0.0", HeldAt: t0, Release: rel("1.0.0"), Yanked: true},
	}
	if len(got) != len(want) {
		t.Fatalf("pending = %+v", got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.WatchID != w.WatchID || g.Raw != w.Raw || !g.HeldAt.Equal(w.HeldAt) || string(g.Release) != string(w.Release) ||
			g.Yanked != w.Yanked || g.Removed != w.Removed || g.TargetID != w.TargetID {
			t.Errorf("pending[%d] = %+v, want %+v", i, g, w)
		}
	}

	// Held versions survive a restart.
	must(t, s.Close())
	s2, err := Open(ctx, path, discard())
	must(t, err)
	t.Cleanup(func() { _ = s2.Close() })
	got, err = s2.ListPendingReleases(ctx, "t1")
	must(t, err)
	if len(got) != 3 {
		t.Fatalf("pending after restart = %+v", got)
	}

	must(t, s2.DeletePendingRelease(ctx, "w1", "1.0.0"))
	wantErr(t, s2.DeletePendingRelease(ctx, "w1", "1.0.0"), store.ErrNotFound, "delete twice")
	// Deleting a watch deletes what it holds.
	must(t, s2.DeleteWatch(ctx, "w2"))
	got, err = s2.ListPendingReleases(ctx, "t1")
	must(t, err)
	if len(got) != 1 || got[0].WatchID != "w1" || got[0].Raw != "1.1.0" {
		t.Fatalf("pending after deletes = %+v", got)
	}

	// A watch that points at another target no longer lists its rows.
	must(t, s2.CreateTarget(ctx, store.Target{ID: "t2", Kind: "pypi", Coordinates: map[string]string{"name": "x"}, NextCheckAt: t0}))
	if _, err := s2.db.ExecContext(ctx, `UPDATE watch SET target_id = 't2' WHERE id = 'w1'`); err != nil {
		t.Fatal(err)
	}
	got, err = s2.ListPendingReleases(ctx, "t1")
	must(t, err)
	if len(got) != 0 {
		t.Fatalf("pending of a moved watch = %+v", got)
	}
}
