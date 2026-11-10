package storetest

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

func testPendingRelease(t *testing.T, open opener) {
	s := open(t)
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

	wantErr(t, s.DeletePendingRelease(ctx, "w1", "t2", "1.0.0"), store.ErrNotFound, "delete on another target")
	must(t, s.DeletePendingRelease(ctx, "w1", "t1", "1.0.0"))
	wantErr(t, s.DeletePendingRelease(ctx, "w1", "t1", "1.0.0"), store.ErrNotFound, "delete twice")
	// Deleting a watch deletes what it holds.
	must(t, s.DeleteWatch(ctx, "w2"))
	got, err = s.ListPendingReleases(ctx, "t1")
	must(t, err)
	if len(got) != 1 || got[0].WatchID != "w1" || got[0].Raw != "1.1.0" {
		t.Fatalf("pending after deletes = %+v", got)
	}

	// A watch that points at another target no longer lists its rows.
	must(t, s.CreateTarget(ctx, store.Target{ID: "t2", Kind: "pypi", Coordinates: map[string]string{"name": "x"}, NextCheckAt: t0}))
	must(t, s.InConfigTx(ctx, func(tx store.ConfigTx) error {
		return tx.UpdateWatch(ctx, store.Watch{ID: "w1", TargetID: "t2", Policy: json.RawMessage(`{}`)})
	}))
	got, err = s.ListPendingReleases(ctx, "t1")
	must(t, err)
	if len(got) != 0 {
		t.Fatalf("pending of a moved watch = %+v", got)
	}

	// The same raw version on the new target is held without a conflict
	// with the row left on the old one, and deleted by its own key.
	if _, err := s.AddVersion(ctx, store.Version{TargetID: "t2", Raw: "1.1.0", Normalized: "1.1.0", FirstSeenAt: t0}); err != nil {
		t.Fatal(err)
	}
	moved := p("w1", "1.1.0", t0.Add(2*time.Hour))
	moved.TargetID = "t2"
	must(t, s.AddPendingRelease(ctx, moved))
	wantErr(t, s.AddPendingRelease(ctx, moved), store.ErrConflict, "held twice on the new target")
	got, err = s.ListPendingReleases(ctx, "t2")
	must(t, err)
	if len(got) != 1 || got[0].TargetID != "t2" || got[0].Raw != "1.1.0" {
		t.Fatalf("pending on the new target = %+v", got)
	}

	// DeletePendingReleasesOfWatch forgets the rows of every target.
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w3", TargetID: "t2"}))
	must(t, s.AddPendingRelease(ctx, store.PendingRelease{WatchID: "w3", TargetID: "t2", Raw: "1.1.0", HeldAt: t0, Release: rel("1.1.0")}))
	var n int64
	must(t, s.InTx(ctx, func(tx store.Tx) error {
		var err error
		n, err = tx.DeletePendingReleasesOfWatch(ctx, "w1")
		return err
	}))
	if n != 2 {
		t.Fatalf("DeletePendingReleasesOfWatch deleted %d rows, want 2 (old and new target)", n)
	}
	if n, err := s.DeletePendingReleasesOfWatch(ctx, "w1"); n != 0 || err != nil {
		t.Fatalf("second DeletePendingReleasesOfWatch = %d, %v", n, err)
	}
	got, err = s.ListPendingReleases(ctx, "t2")
	must(t, err)
	if len(got) != 1 || got[0].WatchID != "w3" {
		t.Fatalf("other watches' rows must stay: %+v", got)
	}
}
