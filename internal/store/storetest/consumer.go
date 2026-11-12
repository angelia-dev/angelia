package storetest

import (
	"encoding/json"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

func consumer(id, name string) store.Consumer {
	return store.Consumer{ID: id, Name: name, Filter: json.RawMessage(`{"type":["release"]}`), CreatedAt: t0, UpdatedAt: t0}
}

func createConsumer(t *testing.T, s store.APIStore, c store.Consumer) {
	t.Helper()
	ctx := t.Context()
	must(t, s.InAPITx(ctx, func(tx store.APITx) error { return tx.CreateConsumer(ctx, c) }))
}

// Story #79-B: create, read, list paging, update (filter only), delete and
// the ErrNotFound / ErrConflict / ErrInvalid mapping.
func testConsumerCRUD(t *testing.T, open opener) {
	s := open(t)
	ctx := t.Context()

	for _, n := range []string{"b", "c", "a"} {
		createConsumer(t, s, consumer("id-"+n, n))
	}
	// Names that sort alike are told apart by id: the name is unique, so a
	// shared name is a conflict.
	err := s.InAPITx(ctx, func(tx store.APITx) error { return tx.CreateConsumer(ctx, consumer("id-other", "a")) })
	wantErr(t, err, store.ErrConflict, "duplicate name")
	err = s.InAPITx(ctx, func(tx store.APITx) error { return tx.CreateConsumer(ctx, consumer("id-a", "a2")) })
	wantErr(t, err, store.ErrConflict, "duplicate id")
	for _, bad := range []store.Consumer{
		{Name: "n", CreatedAt: t0},
		{ID: "i", CreatedAt: t0},
		{ID: "i", Name: "n"},
		{ID: "i", Name: "n", CreatedAt: t0, AckedSeq: -1},
		{ID: "i", Name: "n", CreatedAt: t0, Filter: json.RawMessage(`{bad`)},
	} {
		err = s.InAPITx(ctx, func(tx store.APITx) error { return tx.CreateConsumer(ctx, bad) })
		wantErr(t, err, store.ErrInvalid, "invalid create")
	}

	got, err := s.ConsumerByName(ctx, "b")
	must(t, err)
	want := consumer("id-b", "b")
	if got.ID != want.ID || got.Name != "b" || string(got.Filter) != string(want.Filter) || got.AckedSeq != 0 ||
		!got.AckedAt.IsZero() || !got.CreatedAt.Equal(t0) || !got.UpdatedAt.Equal(t0) {
		t.Fatalf("ConsumerByName = %+v", got)
	}
	_, err = s.ConsumerByName(ctx, "missing")
	wantErr(t, err, store.ErrNotFound, "ConsumerByName")

	// An empty filter reads back as {}; a start position is stored as given.
	empty := store.Consumer{ID: "id-e", Name: "e", AckedSeq: 7, CreatedAt: t0}
	createConsumer(t, s, empty)
	got, err = s.ConsumerByName(ctx, "e")
	must(t, err)
	if string(got.Filter) != "{}" || got.AckedSeq != 7 || !got.AckedAt.IsZero() {
		t.Fatalf("empty consumer = %+v", got)
	}

	// Paging in (name, id) order.
	var names []string
	afterName, afterID := "", ""
	for {
		page, err := s.ListConsumers(ctx, afterName, afterID, 2)
		must(t, err)
		for _, c := range page {
			names = append(names, c.Name)
		}
		if len(page) < 2 {
			break
		}
		last := page[len(page)-1]
		afterName, afterID = last.Name, last.ID
	}
	if !equalStrings(names, []string{"a", "b", "c", "e"}) {
		t.Fatalf("paged names = %v", names)
	}

	// Update replaces the filter only.
	later := t0.Add(time.Hour)
	must(t, s.InAPITx(ctx, func(tx store.APITx) error {
		if _, err := tx.AckConsumer(ctx, "b", 5, t0.Add(time.Minute)); err != nil {
			return err
		}
		u := consumer("id-b", "renamed-is-ignored")
		u.Filter, u.UpdatedAt, u.AckedSeq = json.RawMessage(`{"watch":["w"]}`), later, 99
		return tx.UpdateConsumer(ctx, u)
	}))
	got, err = s.ConsumerByName(ctx, "b")
	must(t, err)
	if string(got.Filter) != `{"watch":["w"]}` || got.AckedSeq != 5 || got.Name != "b" ||
		!got.UpdatedAt.Equal(later) || !got.CreatedAt.Equal(t0) {
		t.Fatalf("after update = %+v", got)
	}
	err = s.InAPITx(ctx, func(tx store.APITx) error { return tx.UpdateConsumer(ctx, consumer("nope", "x")) })
	wantErr(t, err, store.ErrNotFound, "update unknown")

	// Delete.
	must(t, s.InAPITx(ctx, func(tx store.APITx) error { return tx.DeleteConsumer(ctx, "id-b") }))
	_, err = s.ConsumerByName(ctx, "b")
	wantErr(t, err, store.ErrNotFound, "after delete")
	err = s.InAPITx(ctx, func(tx store.APITx) error { return tx.DeleteConsumer(ctx, "id-b") })
	wantErr(t, err, store.ErrNotFound, "second delete")
}

// Story #79-B: an ack of a lower or equal seq is a no-op that keeps acked_at
// (and updated_at); a higher one moves both.
func testAckConsumer(t *testing.T, open opener) {
	s := open(t)
	ctx := t.Context()
	createConsumer(t, s, consumer("c1", "agent"))
	ack := func(seq int64, at time.Time) store.Consumer {
		t.Helper()
		var c store.Consumer
		must(t, s.InAPITx(ctx, func(tx store.APITx) (err error) { c, err = tx.AckConsumer(ctx, "agent", seq, at); return err }))
		return c
	}
	t1, t2, t3 := t0.Add(time.Minute), t0.Add(2*time.Minute), t0.Add(3*time.Minute)

	// An ack of 0 on a fresh consumer moves nothing.
	if c := ack(0, t1); c.AckedSeq != 0 || !c.AckedAt.IsZero() || !c.UpdatedAt.Equal(t0) {
		t.Fatalf("ack 0 = %+v", c)
	}
	if c := ack(10, t1); c.AckedSeq != 10 || !c.AckedAt.Equal(t1) || !c.UpdatedAt.Equal(t1) || string(c.Filter) != `{"type":["release"]}` {
		t.Fatalf("ack 10 = %+v", c)
	}
	for _, seq := range []int64{3, 10} {
		if c := ack(seq, t2); c.AckedSeq != 10 || !c.AckedAt.Equal(t1) || !c.UpdatedAt.Equal(t1) {
			t.Fatalf("ack %d = %+v, want the consumer unchanged", seq, c)
		}
	}
	if c := ack(11, t3); c.AckedSeq != 11 || !c.AckedAt.Equal(t3) {
		t.Fatalf("ack 11 = %+v", c)
	}
	// The result is what a read returns.
	got, err := s.ConsumerByName(ctx, "agent")
	must(t, err)
	if got.AckedSeq != 11 || !got.AckedAt.Equal(t3) {
		t.Fatalf("read after ack = %+v", got)
	}

	err = s.InAPITx(ctx, func(tx store.APITx) error { _, err := tx.AckConsumer(ctx, "nope", 1, t3); return err })
	wantErr(t, err, store.ErrNotFound, "ack unknown")
	err = s.InAPITx(ctx, func(tx store.APITx) error { _, err := tx.AckConsumer(ctx, "agent", -1, t3); return err })
	wantErr(t, err, store.ErrInvalid, "ack negative")
}

// Story #79-B: 50 concurrent acks with random seqs leave acked_seq at their
// maximum, whatever the order they commit in.
func testAckConsumerConcurrent(t *testing.T, open opener) {
	s := open(t)
	ctx := t.Context()
	createConsumer(t, s, consumer("c1", "agent"))
	const n = 50
	var highest int64
	seqs := make([]int64, n)
	for i := range seqs {
		seqs[i] = 1 + rand.Int64N(1000) //nolint:gosec // a test input, not a secret
		highest = max(highest, seqs[i])
	}
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for _, seq := range seqs {
		wg.Go(func() {
			errs <- s.InAPITx(ctx, func(tx store.APITx) error { _, err := tx.AckConsumer(ctx, "agent", seq, t0); return err })
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		must(t, err)
	}
	got, err := s.ConsumerByName(ctx, "agent")
	must(t, err)
	if got.AckedSeq != highest {
		t.Fatalf("acked_seq = %d, want the maximum %d of %v", got.AckedSeq, highest, seqs)
	}
}

// Story #79-B: deleting a watch does not touch consumers (a filter names
// watches loosely, the cursor is a log position).
func testDeleteWatchKeepsConsumers(t *testing.T, open opener) {
	s := open(t)
	seed(t, s) // t1, w1
	ctx := t.Context()
	c := consumer("c1", "agent")
	c.Filter = json.RawMessage(`{"watch":["w1"]}`)
	c.AckedSeq, c.AckedAt = 4, t0
	createConsumer(t, s, c)
	must(t, s.InConfigTx(ctx, func(tx store.ConfigTx) error { return tx.DeleteWatch(ctx, "w1") }))
	got, err := s.ConsumerByName(ctx, "agent")
	must(t, err)
	if string(got.Filter) != `{"watch":["w1"]}` || got.AckedSeq != 4 || !got.UpdatedAt.Equal(t0) {
		t.Fatalf("consumer after watch delete = %+v", got)
	}
}

// MaxEventSeq is the head of the log, pruned events included: the highest
// seq ever committed, also when pruning removed every event.
func testMaxEventSeq(t *testing.T, open opener) {
	s := open(t)
	seed(t, s)
	ctx := t.Context()
	head := func() int64 {
		t.Helper()
		n, err := s.MaxEventSeq(ctx)
		must(t, err)
		return n
	}
	if n := head(); n != 0 {
		t.Fatalf("empty log = %d, want 0", n)
	}
	must(t, s.AppendEvent(ctx, eventAt("e1", pruneOld), nil))
	must(t, s.AppendEvent(ctx, eventAt("e2", pruneOld), nil))
	must(t, s.AppendEvent(ctx, eventAt("e3", pruneOld), nil))
	if n := head(); n != 3 {
		t.Fatalf("three events = %d, want 3", n)
	}
	// Fully pruned: the log is empty but the head stays at the last seq.
	res, err := s.PruneHistory(ctx, pruneBefore, 0)
	must(t, err)
	if left := listEventIDs(t, s); res.Events != 3 || len(left) != 0 {
		t.Fatalf("prune = %+v, retained %v", res, left)
	}
	if n := head(); n != 3 {
		t.Fatalf("fully pruned log = %d, want 3 (HighestPrunedSeq)", n)
	}
	if n, err := s.HighestPrunedSeq(ctx); err != nil || n != 3 {
		t.Fatalf("HighestPrunedSeq = %d, %v", n, err)
	}
	// A new event continues after the pruned ones.
	must(t, s.AppendEvent(ctx, eventAt("e4", t0), nil))
	if n := head(); n != 4 {
		t.Fatalf("after e4 = %d, want 4", n)
	}
}
