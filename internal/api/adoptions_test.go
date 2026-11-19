package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

func adoptBody(dry bool, objs ...string) map[string]any {
	list := []any{}
	for _, o := range objs {
		kind, name, _ := strings.Cut(o, "/")
		list = append(list, map[string]any{"kind": kind, "name": name})
	}
	return map[string]any{"objects": list, "dry_run": dry}
}

// reapply applies fileConfig again, as the next startup does, and returns the plan.
func (e *env) reapply(yaml string) *watchfile.Plan {
	e.t.Helper()
	f, err := watchfile.Parse([]byte(yaml), "angelia.yaml", watchfile.Options{
		Sources: testSources(e.t), LookupEnv: func(string) (string, bool) { return "", true },
	})
	if err != nil {
		e.t.Fatal(err)
	}
	p, err := watchfile.Apply(context.Background(), e.st, f, watchfile.ApplyOptions{Now: func() time.Time { return e.now }})
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func TestAdoptResources(t *testing.T) {
	e := newEnv(t)
	e.applyFile(fileConfig)
	watchID := e.watchID("file-watch")
	subID := e.subscriptionID("file-sub")
	ev := e.appendEvent(watchID, "dev.angelia.release.published.v1", "pypi", e.now,
		store.Delivery{ID: "01JBX2A9QWERTY0123456789AB", SubscriptionID: subID, Status: store.DeliveryPending, NextAttemptAt: e.now})

	// Only an admin.
	e.must(http.StatusForbidden, e.do("POST", "/v1/adoptions", tokWrite, adoptBody(false, "watch/file-watch")))

	// Ownership must stay consistent; nothing changes on refusal.
	for _, tc := range []struct {
		objs []string
		want string
	}{
		{[]string{"subscription/file-sub"}, "adopt its sink file-hook too"},
		{[]string{"sink/file-hook"}, "adopt the subscription file-sub too"},
		{[]string{"watch/nope"}, "no watch named nope"},
		{[]string{"watch/file-watch", "sink/file-hook"}, "adopt the subscription file-sub too"},
	} {
		r := e.do("POST", "/v1/adoptions", tokAdmin, adoptBody(false, tc.objs...))
		if r.Status != http.StatusUnprocessableEntity || !strings.Contains(string(r.Body), tc.want) {
			t.Errorf("%v: %d %s", tc.objs, r.Status, r.Body)
		}
	}
	if m := e.must(200, e.do("GET", "/v1/watches/file-watch", tokRead, nil)).json(t); m["managed_by"] != "file" {
		t.Fatalf("refused adoption changed the watch: %v", m)
	}

	// A dry run says what would happen and changes nothing.
	all := []string{"subscription/file-sub", "watch/file-watch", "sink/file-hook"}
	m := e.must(200, e.do("POST", "/v1/adoptions", tokAdmin, adoptBody(true, all...))).json(t)
	if m["dry_run"] != true || len(m["adopted"].([]any)) != 3 {
		t.Fatalf("dry run: %v", m)
	}
	if m := e.must(200, e.do("GET", "/v1/sinks/file-hook", tokAdmin, nil)).json(t); m["managed_by"] != "file" {
		t.Fatalf("dry run changed the sink: %v", m)
	}

	m = e.must(200, e.do("POST", "/v1/adoptions", tokAdmin, adoptBody(false, all...))).json(t)
	got := m["adopted"].([]any)
	if len(got) != 3 || got[0].(map[string]any)["kind"] != "sink" || got[1].(map[string]any)["id"] != watchID ||
		got[2].(map[string]any)["id"] != subID {
		t.Fatalf("adopted: %v", m)
	}
	if !strings.Contains(e.log.String(), `"action":"adopt"`) {
		t.Errorf("no audit record: %s", e.log.String())
	}
	// In place: same ids, the pending delivery kept.
	if m := e.must(200, e.do("GET", "/v1/watches/file-watch", tokRead, nil)).json(t); m["managed_by"] != "api" || m["id"] != watchID {
		t.Fatalf("adopted watch: %v", m)
	}
	d := e.must(200, e.do("GET", "/v1/deliveries/01JBX2A9QWERTY0123456789AB", tokRead, nil)).json(t)
	if d["status"] != "pending" || d["event_id"] != ev.ID {
		t.Fatalf("delivery: %v", d)
	}
	// Adopting again: no longer managed by the file.
	if r := e.do("POST", "/v1/adoptions", tokAdmin, adoptBody(false, "watch/file-watch")); r.Status != http.StatusUnprocessableEntity {
		t.Errorf("second adoption: %d %s", r.Status, r.Body)
	}

	// The API can now change them.
	e.must(http.StatusOK, e.do("PUT", "/v1/subscriptions/file-sub", tokAdmin, map[string]any{"sink": "file-hook", "filter": map[string]any{"bump": []string{"major"}}}))

	// The next startup with the same file skips the adopted entries and changes nothing.
	p := e.reapply(fileConfig)
	if p.Count(watchfile.KindSink, watchfile.Skip) != 1 || p.Count(watchfile.KindWatch, watchfile.Skip) != 1 ||
		p.Count(watchfile.KindSubscription, watchfile.Skip) != 1 || len(p.Changes) != 3 {
		t.Fatalf("plan with adopted entries:\n%s", p)
	}
	if m := e.must(200, e.do("GET", "/v1/subscriptions/file-sub", tokRead, nil)).json(t); m["managed_by"] != "api" ||
		!strings.Contains(string(e.do("GET", "/v1/subscriptions/file-sub", tokRead, nil).Body), "major") {
		t.Fatalf("startup undid the API change: %v", m)
	}

	// Once the file no longer declares them, the names are forgotten: a new
	// file entry with such a name is refused again as taken.
	if p := e.reapply("version: 1\n"); len(p.Changes) != 0 {
		t.Fatalf("empty file plan:\n%s", p)
	}
	f, err := watchfile.Parse([]byte(fileConfig), "angelia.yaml", watchfile.Options{
		Sources: testSources(e.t), LookupEnv: func(string) (string, bool) { return "", true },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := watchfile.Apply(context.Background(), e.st, f, watchfile.ApplyOptions{}); err == nil || !strings.Contains(err.Error(), "name taken") {
		t.Fatalf("after forgetting: %v", err)
	}
}

// A file subscription whose sink was adopted is skipped too: a file row may
// not reference an API row.
func TestAdoptedSinkSkipsNewFileSubscription(t *testing.T) {
	e := newEnv(t)
	e.applyFile(fileConfig)
	e.must(200, e.do("POST", "/v1/adoptions", tokAdmin, adoptBody(false, "sink/file-hook", "subscription/file-sub")))
	p := e.reapply(fileConfig + "  - {name: file-sub-2, sink: file-hook}\n")
	if p.Count(watchfile.KindSubscription, watchfile.Skip) != 2 || p.Count(watchfile.KindSubscription, watchfile.Create) != 0 {
		t.Fatalf("plan:\n%s", p)
	}
	e.must(http.StatusNotFound, e.do("GET", "/v1/subscriptions/file-sub-2", tokRead, nil))
}
