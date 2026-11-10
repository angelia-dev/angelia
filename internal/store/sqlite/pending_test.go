package sqlite

import (
	"encoding/json"
	"testing"

	"github.com/angelia-dev/angelia/internal/store"
)

// Held versions survive a restart (the other pending-release rules are in the
// shared suite).
func TestPendingReleaseSurvivesRestart(t *testing.T) {
	s, path := openTest(t)
	seed(t, s)
	ctx := t.Context()
	for _, raw := range []string{"1.0.0", "1.1.0"} {
		_, err := s.AddVersion(ctx, store.Version{TargetID: "t1", Raw: raw, Normalized: raw, FirstSeenAt: t0})
		must(t, err)
		must(t, s.AddPendingRelease(ctx, store.PendingRelease{
			WatchID: "w1", TargetID: "t1", Raw: raw, HeldAt: t0, Release: json.RawMessage(`{"raw":"` + raw + `"}`),
		}))
	}
	must(t, s.Close())

	s2, err := Open(ctx, path, discard())
	must(t, err)
	t.Cleanup(func() { _ = s2.Close() })
	got, err := s2.ListPendingReleases(ctx, "t1")
	must(t, err)
	if len(got) != 2 || got[0].Raw != "1.0.0" || got[1].Raw != "1.1.0" {
		t.Fatalf("pending after restart = %+v", got)
	}
}
