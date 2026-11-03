package dispatch

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/angelia-dev/angelia/internal/sink"
)

type outcomes struct {
	mu  sync.Mutex
	got []string
}

func (o *outcomes) AttemptFinished(outcome string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.got = append(o.got, outcome)
}

func (o *outcomes) list() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.got...)
}

// Metrics see delivered, retry and dead outcomes, each once and only after
// the outcome is recorded.
func TestMetricsOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		max  int64
		want string
	}{
		{"delivered", nil, 0, OutcomeDelivered},
		{"retry", sink.HTTPStatus(500, 0), 0, OutcomeRetry},
		{"permanent", sink.Permanent(errors.New("bad config")), 0, OutcomeDead},
		{"exhausted", errors.New("boom"), 1, OutcomeDead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := newStore(t)
			addEvents(t, st, 1)
			m := &outcomes{}
			ts := &testSink{fn: func(context.Context, sink.Request) error { return tc.err }}
			d := newDispatcher(t, st, registry(t, ts), Options{Clock: &fakeClock{now: t0}, Metrics: m, MaxAttempts: tc.max})
			if n := pollWait(t, d); n != 1 {
				t.Fatalf("started %d attempts, want 1", n)
			}
			if got := m.list(); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("outcomes = %v, want [%s]", got, tc.want)
			}
		})
	}
}

// An attempt whose lease was lost is not counted: its result was dropped.
func TestMetricsSkipLostLease(t *testing.T) {
	st, _ := newStore(t)
	addEvents(t, st, 1)
	m := &outcomes{}
	clock := &fakeClock{now: t0}
	ts := &testSink{fn: func(ctx context.Context, _ sink.Request) error {
		// Another holder takes the delivery over while this one sends.
		clock.Set(t0.Add(DefaultLeaseTTL + 1))
		leased, err := st.LeaseDueDeliveries(ctx, "other", clock.Now(), clock.Now().Add(DefaultLeaseTTL), 1)
		if err != nil || len(leased) != 1 {
			t.Errorf("takeover: %v, %v", leased, err)
		}
		return nil
	}}
	d := newDispatcher(t, st, registry(t, ts), Options{Clock: clock, Metrics: m})
	pollWait(t, d)
	if got := m.list(); len(got) != 0 {
		t.Fatalf("outcomes = %v, want none for a fenced-off attempt", got)
	}
}
