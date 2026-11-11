package storetest

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// Reads by name and by page: the lookups of the REST API (store.APIReader).
func testNamedListsAndLookups(t *testing.T, open opener) {
	s := open(t)
	seed(t, s) // t1, w1, k1 (unnamed), s1, s2 (unnamed)
	ctx := t.Context()
	for _, n := range []string{"b", "c", "a"} {
		must(t, s.CreateWatch(ctx, store.Watch{ID: "w-" + n, TargetID: "t1", Name: n, ManagedBy: store.ManagedByAPI}))
		must(t, s.CreateSink(ctx, store.Sink{ID: "k-" + n, Kind: "webhook", ConfigEncrypted: []byte(n), Name: n, ManagedBy: store.ManagedByAPI}))
	}
	// Two sinks with a name that sorts alike are told apart by id.
	must(t, s.CreateSink(ctx, store.Sink{ID: "k-b2", Kind: "webhook", Name: "b2", ManagedBy: store.ManagedByFile}))
	for _, n := range []string{"y", "x"} {
		must(t, s.CreateSubscription(ctx, store.Subscription{ID: "s-" + n, SinkID: "k-a", Name: n, ManagedBy: store.ManagedByAPI}))
	}

	// Pages follow (Name, ID) and skip rows without a name.
	var names []string
	afterName, afterID := "", ""
	for {
		page, err := s.ListNamedWatches(ctx, afterName, afterID, 2)
		must(t, err)
		if len(page) == 0 {
			break
		}
		for _, w := range page {
			names = append(names, w.Name)
		}
		last := page[len(page)-1]
		afterName, afterID = last.Name, last.ID
	}
	if fmt.Sprint(names) != "[a b c]" {
		t.Fatalf("named watches = %v, want [a b c]", names)
	}
	sinks, err := s.ListNamedSinks(ctx, "a", "k-a", 10)
	must(t, err)
	if len(sinks) != 3 || sinks[0].ID != "k-b" || sinks[1].ID != "k-b2" || sinks[2].ID != "k-c" || string(sinks[0].ConfigEncrypted) != "b" {
		t.Fatalf("named sinks after a = %+v", sinks)
	}
	subs, err := s.ListNamedSubscriptions(ctx, "", "", 1)
	must(t, err)
	if len(subs) != 1 || subs[0].Name != "x" || subs[0].ManagedBy != store.ManagedByAPI {
		t.Fatalf("named subscriptions = %+v", subs)
	}

	w, err := s.WatchByName(ctx, "b")
	must(t, err)
	k, err := s.SinkByName(ctx, "b2")
	must(t, err)
	sub, err := s.SubscriptionByName(ctx, "y")
	must(t, err)
	if w.ID != "w-b" || k.ID != "k-b2" || k.ManagedBy != store.ManagedByFile || sub.ID != "s-y" {
		t.Fatalf("by name: %+v %+v %+v", w, k, sub)
	}
	_, err = s.WatchByName(ctx, "nope")
	wantErr(t, err, store.ErrNotFound, "watch by name")
	_, err = s.SinkByName(ctx, "")
	wantErr(t, err, store.ErrNotFound, "sink by empty name")
	_, err = s.SubscriptionByName(ctx, "nope")
	wantErr(t, err, store.ErrNotFound, "subscription by name")

	bySink, err := s.SubscriptionsBySink(ctx, "k-a")
	must(t, err)
	if len(bySink) != 2 || bySink[0].ID != "s-x" || bySink[1].ID != "s-y" {
		t.Fatalf("subscriptions by sink = %+v, want ordered by name", bySink)
	}
	if none, err := s.SubscriptionsBySink(ctx, "k-c"); err != nil || len(none) != 0 {
		t.Fatalf("sink without subscriptions: %+v %v", none, err)
	}
	gs, err := s.GetSink(ctx, "k-c")
	must(t, err)
	gsub, err := s.GetSubscription(ctx, "s-x")
	must(t, err)
	if gs.Name != "c" || gsub.Name != "x" || gsub.SinkID != "k-a" {
		t.Fatalf("get: %+v %+v", gs, gsub)
	}
	_, err = s.GetSink(ctx, "nope")
	wantErr(t, err, store.ErrNotFound, "get sink")
	_, err = s.GetSubscription(ctx, "nope")
	wantErr(t, err, store.ErrNotFound, "get subscription")
}

// InAPITx offers the same lookups as the store, and the sink writes of a
// file apply.
func testInAPITxLookupsAndSinkUpdate(t *testing.T, open opener) {
	s := open(t)
	seed(t, s)
	ctx := t.Context()
	must(t, s.CreateSink(ctx, store.Sink{ID: "k2", Kind: "webhook", ConfigEncrypted: []byte{1}, Name: "hook", ManagedBy: store.ManagedByFile}))
	must(t, s.CreateSubscription(ctx, store.Subscription{ID: "s3", SinkID: "k2", Name: "sub", ManagedBy: store.ManagedByFile}))
	must(t, s.InAPITx(ctx, func(tx store.APITx) error {
		k, err := tx.SinkByName(ctx, "hook")
		if err != nil {
			return err
		}
		sub, err := tx.SubscriptionByName(ctx, "sub")
		if err != nil {
			return err
		}
		if k.ID != "k2" || sub.ID != "s3" {
			t.Errorf("by name in tx: %+v %+v", k, sub)
		}
		if _, err := tx.WatchByName(ctx, "nope"); err == nil {
			t.Error("watch by name in tx: want ErrNotFound")
		}
		if got, err := tx.SubscriptionsBySink(ctx, "k2"); err != nil || len(got) != 1 {
			t.Errorf("subscriptions by sink in tx: %+v %v", got, err)
		}
		if got, err := tx.GetSink(ctx, "k2"); err != nil || got.ID != "k2" {
			t.Errorf("get sink in tx: %+v %v", got, err)
		}
		if got, err := tx.GetSubscription(ctx, "s3"); err != nil || got.ID != "s3" {
			t.Errorf("get subscription in tx: %+v %v", got, err)
		}
		return nil
	}))

	must(t, s.InConfigTx(ctx, func(tx store.ConfigTx) error {
		ks, err := tx.ListManagedSinks(ctx, store.ManagedByFile)
		if err != nil {
			return err
		}
		if len(ks) != 1 || ks[0].ID != "k2" || string(ks[0].ConfigEncrypted) != "\x01" {
			return fmt.Errorf("managed sinks = %+v", ks)
		}
		if err := tx.UpdateSink(ctx, store.Sink{ID: "k2", Kind: "github-dispatch", ConfigEncrypted: []byte{9, 9}}); err != nil {
			return err
		}
		if err := tx.UpdateSink(ctx, store.Sink{ID: "nope", Kind: "webhook"}); !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("update missing sink  failed, want ErrNotFound: %w", err)
		}
		return nil
	}))
	k, err := s.GetSink(ctx, "k2")
	must(t, err)
	if k.Kind != "github-dispatch" || string(k.ConfigEncrypted) != "\x09\x09" || k.Name != "hook" || k.ManagedBy != store.ManagedByFile {
		t.Fatalf("updated sink = %+v (kind and config change, name and owner stay)", k)
	}
}

func idsOf(rows []store.DeliveryRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// Events and deliveries by id and by page, and the replay of a finished
// delivery.
func testEventAndDeliveryReads(t *testing.T, open opener) {
	s := open(t)
	seed(t, s)
	ctx := t.Context()
	d := func(id, sub string) store.Delivery {
		return store.Delivery{ID: id, SubscriptionID: sub, Status: store.DeliveryPending}
	}
	must(t, s.AppendEvent(ctx, event("e1"), []store.Delivery{d("d1a", "s1"), d("d1b", "s2")}))
	must(t, s.AppendEvent(ctx, event("e2"), []store.Delivery{d("d2a", "s1")}))
	evs, err := s.ListEventsAfter(ctx, 0, 10)
	must(t, err)
	e1, e2 := evs[0], evs[1]

	got, err := s.GetEvent(ctx, "e1")
	must(t, err)
	if got.Seq != e1.Seq || got.ID != "e1" || got.WatchID != "w1" || got.Type != "release.published" ||
		string(got.Payload) != `{"v":1}` || !got.CreatedAt.Equal(t0) {
		t.Fatalf("GetEvent = %+v", got)
	}
	_, err = s.GetEvent(ctx, "nope")
	wantErr(t, err, store.ErrNotFound, "get event")
	for after, want := range map[int64]bool{0: true, e1.Seq: true, e2.Seq: false, e2.Seq + 100: false} {
		if has, err := s.HasEventsAfter(ctx, after); err != nil || has != want {
			t.Errorf("HasEventsAfter(%d) = %v, %v; want %v", after, has, err, want)
		}
	}
	dl, err := s.GetDelivery(ctx, "d1b")
	must(t, err)
	if dl.EventID != "e1" || dl.SubscriptionID != "s2" || dl.Status != store.DeliveryPending {
		t.Fatalf("GetDelivery = %+v", dl)
	}
	_, err = s.GetDelivery(ctx, "nope")
	wantErr(t, err, store.ErrNotFound, "get delivery")

	// Newest event first, then by delivery id.
	page := func(q store.DeliveryQuery) []string {
		t.Helper()
		rows, err := s.ListDeliveryPage(ctx, q)
		must(t, err)
		return idsOf(rows)
	}
	for _, tc := range []struct {
		name string
		q    store.DeliveryQuery
		want string
	}{
		{"all", store.DeliveryQuery{Limit: 10}, "[d2a d1a d1b]"},
		{"limit", store.DeliveryQuery{Limit: 2}, "[d2a d1a]"},
		{"after a row of the same event", store.DeliveryQuery{BeforeSeq: e1.Seq, BeforeID: "d1a", Limit: 10}, "[d1b]"},
		{"after the first event", store.DeliveryQuery{BeforeSeq: e1.Seq, BeforeID: "d1b", Limit: 10}, "[]"},
		{"status", store.DeliveryQuery{Statuses: []string{store.DeliveryDead}, Limit: 10}, "[]"},
		{"subscription", store.DeliveryQuery{SubscriptionIDs: []string{"s1"}, Limit: 10}, "[d2a d1a]"},
		{"no subscription", store.DeliveryQuery{SubscriptionIDs: []string{}, Limit: 10}, "[]"},
		{"event", store.DeliveryQuery{EventID: "e1", Limit: 10}, "[d1a d1b]"},
	} {
		if got := fmt.Sprint(page(tc.q)); got != tc.want {
			t.Errorf("%s: page = %s, want %s", tc.name, got, tc.want)
		}
	}
	rows, err := s.ListDeliveryPage(ctx, store.DeliveryQuery{Limit: 1})
	must(t, err)
	if len(rows) != 1 || rows[0].EventSeq != e2.Seq || rows[0].SubscriptionID != "s1" {
		t.Fatalf("row = %+v, want the event's seq", rows)
	}

	// Kill d1a on its first attempt and deliver d2a; both can be replayed.
	leased, err := s.LeaseDueDeliveries(ctx, "h", t0, t0.Add(time.Minute), 10)
	must(t, err)
	if len(leased) != 3 {
		t.Fatalf("leased %d deliveries, want 3", len(leased))
	}
	for _, ld := range leased {
		status, lastErr := store.DeliveryDelivered, ""
		if ld.Delivery.ID == "d1a" {
			status, lastErr = store.DeliveryDead, "boom"
		}
		if ld.Delivery.ID == "d1b" {
			continue // stays in flight
		}
		ok, err := s.FinishDelivery(ctx, store.DeliveryFinish{ID: ld.Delivery.ID, Attempt: ld.Delivery.Attempts, Status: status, LastError: lastErr}, "h")
		must(t, err)
		if !ok {
			t.Fatalf("FinishDelivery(%s) fenced out", ld.Delivery.ID)
		}
	}
	at := t0.Add(time.Hour)
	must(t, s.InAPITx(ctx, func(tx store.APITx) error {
		if got, err := tx.GetDelivery(ctx, "d1a"); err != nil || got.Status != store.DeliveryDead {
			t.Errorf("GetDelivery in tx = %+v %v", got, err)
		}
		cands, err := tx.ListReplayCandidates(ctx, store.ReplayQuery{Statuses: []string{store.DeliveryDead, store.DeliveryDelivered}})
		if err != nil {
			return err
		}
		if fmt.Sprint(idsOf(cands)) != "[d1a d2a]" || cands[0].EventSeq != e1.Seq || string(cands[0].EventPayload) != `{"v":1}` {
			t.Errorf("candidates = %+v, want d1a then d2a oldest event first", cands)
		}
		if one, err := tx.ListReplayCandidates(ctx, store.ReplayQuery{EventID: "e2", SubscriptionID: "s1"}); err != nil || fmt.Sprint(idsOf(one)) != "[d2a]" {
			t.Errorf("candidates of e2/s1 = %+v %v", one, err)
		}
		if err := tx.ReplayDelivery(ctx, "d1a", at); err != nil {
			return err
		}
		for _, id := range []string{"d1b", "nope"} { // in flight, missing
			if err := tx.ReplayDelivery(ctx, id, at); !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("replay of %s  failed, want ErrNotFound: %w", id, err)
			}
		}
		return nil
	}))
	r, err := s.GetDelivery(ctx, "d1a")
	must(t, err)
	if r.Status != store.DeliveryPending || !r.NextAttemptAt.Equal(at) || r.Attempts != 1 || r.ReplayBaseAttempts != 1 || r.LastError != "boom" || r.Hold != nil {
		t.Fatalf("replayed delivery = %+v", r)
	}
}

// Close ends the use of the store: nothing works afterwards.
func testClose(t *testing.T, open opener) {
	s := open(t)
	seed(t, s)
	must(t, s.Close())
	if _, err := s.GetTarget(t.Context(), "t1"); err == nil {
		t.Fatal("GetTarget on a closed store succeeded")
	}
}
