package storetest

import (
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

func testUpdateVersionAndBaseline(t *testing.T, open opener) {
	s := open(t)
	seed(t, s)
	ctx := t.Context()
	must(t, s.CreateWatch(ctx, store.Watch{ID: "w0", TargetID: "t1", BaselineDone: true}))
	v := store.Version{TargetID: "t1", Raw: "1.0.0", Normalized: "1.0.0", FirstSeenAt: t0}
	if _, err := s.AddVersion(ctx, v); err != nil {
		t.Fatal(err)
	}

	v.Yanked, v.RemovedAt = true, t0.Add(time.Minute)
	v.Normalized, v.Prerelease = "ignored", true // immutable fields
	must(t, s.UpdateVersion(ctx, v))
	vs, err := s.ListVersions(ctx, "t1")
	must(t, err)
	if len(vs) != 1 || !vs[0].Yanked || !vs[0].RemovedAt.Equal(t0.Add(time.Minute)) || vs[0].Normalized != "1.0.0" || vs[0].Prerelease {
		t.Fatalf("versions after update = %+v", vs)
	}
	v.Yanked, v.RemovedAt = false, time.Time{}
	must(t, s.UpdateVersion(ctx, v))
	vs, _ = s.ListVersions(ctx, "t1")
	if vs[0].Yanked || !vs[0].RemovedAt.IsZero() {
		t.Fatalf("versions after reset = %+v", vs)
	}
	wantErr(t, s.UpdateVersion(ctx, store.Version{TargetID: "t1", Raw: "9"}), store.ErrNotFound, "update unknown version")

	ws, err := s.ListWatches(ctx, "t1")
	must(t, err)
	if len(ws) != 2 || ws[0].ID != "w0" || !ws[0].BaselineDone || ws[1].ID != "w1" || ws[1].BaselineDone || ws[1].Labels["team"] != "a" {
		t.Fatalf("watches = %+v", ws)
	}
	if ws, err := s.ListWatches(ctx, "nope"); err != nil || len(ws) != 0 {
		t.Fatalf("watches of unknown target = %v, %v", ws, err)
	}
	for range 2 { // idempotent
		must(t, s.MarkBaselineDone(ctx, "w1"))
	}
	if w, _ := s.GetWatch(ctx, "w1"); !w.BaselineDone {
		t.Fatal("baseline_done not set")
	}
	wantErr(t, s.MarkBaselineDone(ctx, "nope"), store.ErrNotFound, "mark unknown watch")
}
