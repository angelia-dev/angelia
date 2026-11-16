package dispatch

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/sink"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/pgtest"
)

// meanSinkDelay is the mean of the benchmark sink's uniform [0, 250 ms)
// delay, as the bench/crash receiver's.
const meanSinkDelay = 125 * time.Millisecond

// BenchmarkDispatchThroughput measures how close Dispatcher.Run gets to its
// worker ceiling (workers × dispatchers ÷ mean sink delay) with the real
// clock and the default options (8 workers, 1 s poll interval), story #258.
// b.N is the number of deliveries per dispatcher, all due at the start, one
// per event. Run it with a fixed count, for example:
//
//	go test -run '^$' -bench DispatchThroughput -benchtime 800x ./internal/dispatch/
//
// It is not a CI gate: timing on shared runners is noisy. See
// bench/results/dispatch-throughput-2026-11-16.md.
func BenchmarkDispatchThroughput(b *testing.B) {
	b.Run("engine=sqlite", func(b *testing.B) {
		b.Run("dispatchers=1", func(b *testing.B) {
			st, _ := newStore(b)
			benchThroughput(b, st, []Store{st})
		})
	})
	b.Run("engine=postgres", func(b *testing.B) {
		for _, n := range []int{1, 2} {
			b.Run(fmt.Sprintf("dispatchers=%d", n), func(b *testing.B) {
				dsn := pgtest.DSN(b)
				st := openPostgres(b, dsn)
				seedSink(b, st)
				// One store instance per dispatcher, as one per process.
				stores := []Store{st}
				for len(stores) < n {
					stores = append(stores, openPostgres(b, dsn))
				}
				benchThroughput(b, st, stores)
			})
		}
	})
}

// benchThroughput writes b.N deliveries per dispatcher to st, runs one
// dispatcher on each of stores until every outcome is recorded, and reports
// the rate and the lease statistics.
func benchThroughput(b *testing.B, st store.Store, stores []Store) {
	b.Helper()
	total := b.N * len(stores)
	addEvents(b, st, total)
	// Uniform [0, 250 ms) from a fixed seed. Workers draw concurrently, so
	// which delivery gets which delay varies; the sequence does not.
	var rmu sync.Mutex
	rng := rand.New(rand.NewPCG(258, 0)) //nolint:gosec // G404: a benchmark delay
	ts := &testSink{fn: func(ctx context.Context, _ sink.Request) error {
		rmu.Lock()
		delay := time.Duration(rng.Float64() * float64(2*meanSinkDelay))
		rmu.Unlock()
		select {
		case <-time.After(delay):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	sinks := registry(b, ts)
	var finished atomic.Int32
	allFinished := make(chan struct{})
	stats := &leaseStats{}
	ds := make([]*Dispatcher, len(stores))
	for i, s := range stores {
		ds[i] = newDispatcher(b, &measuredStore{
			Store: &finishCounter{Store: s, count: &finished, want: int32(total), done: allFinished}, //nolint:gosec // G115: b.N is small
			stats: stats,
		}, sinks, Options{Holder: fmt.Sprintf("d%d", i)})
	}
	ctx, cancel := context.WithCancel(b.Context())
	defer cancel()
	done := make(chan error, len(ds))
	b.ResetTimer()
	start := time.Now()
	for _, d := range ds {
		go func() { done <- d.Run(ctx) }()
	}
	select {
	case <-allFinished:
	case <-time.After(30*time.Second + time.Duration(total)*200*time.Millisecond):
		b.Errorf("timed out: %d of %d outcomes recorded", finished.Load(), total)
	}
	elapsed := time.Since(start)
	b.StopTimer()
	cancel()
	for range ds {
		must(b, <-done)
	}

	workers := float64(DefaultWorkers * len(ds))
	rate := float64(total) / elapsed.Seconds()
	ceiling := workers / meanSinkDelay.Seconds()
	b.ReportMetric(rate, "deliveries/s")
	b.ReportMetric(100*rate/ceiling, "%ceiling")
	polls, rows, leaseTime := stats.snapshot()
	b.ReportMetric(float64(polls), "polls")
	if polls > 0 {
		b.ReportMetric(float64(rows)/float64(polls), "rows/poll")
		b.ReportMetric(float64(leaseTime.Microseconds())/1000/float64(polls), "lease-ms")
	}
}

// leaseStats counts the lease calls (polls that found something due), the
// rows they leased and the time they took, across dispatchers.
type leaseStats struct {
	mu    sync.Mutex
	calls int
	rows  int
	time  time.Duration
}

func (s *leaseStats) snapshot() (calls, rows int, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.rows, s.time
}

// measuredStore times LeaseDueDeliveries, so that the benchmark needs no
// metrics in production code.
type measuredStore struct {
	Store
	stats *leaseStats
}

func (m *measuredStore) LeaseDueDeliveries(ctx context.Context, holder string, now, until time.Time, limit int) ([]store.LeasedDelivery, error) {
	start := time.Now()
	leased, err := m.Store.LeaseDueDeliveries(ctx, holder, now, until, limit)
	took := time.Since(start)
	m.stats.mu.Lock()
	m.stats.calls++
	m.stats.rows += len(leased)
	m.stats.time += took
	m.stats.mu.Unlock()
	return leased, err
}
