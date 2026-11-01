//go:build bench

// Benchmark 4 (architecture.md section 6): WASM source plugin round-trip
// overhead per List call vs native, target < 5 ms.
//
//	go test -tags bench -run TestBenchmark4 -v ./bench/wasm
//
// Calls alternate native/wasm on the same netguard client and in-memory
// fake upstream, so drift affects both equally. Overhead p50/p95 is the
// difference of the two latency percentiles.

package wasm

import (
	"context"
	"os"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/source"
)

const targetOverhead = 5 * time.Millisecond

func TestBenchmark4(t *testing.T) {
	n := 2000
	if v, err := strconv.Atoi(os.Getenv("BENCH4_CALLS")); err == nil && v > 0 {
		n = v
	}
	t.Logf("GOOS=%s GOARCH=%s GOMAXPROCS=%d NumCPU=%d calls=%d", runtime.GOOS, runtime.GOARCH, runtime.GOMAXPROCS(0), runtime.NumCPU(), n)
	t.Logf("%-20s %10s %10s %10s %10s %12s %12s", "workload", "native p50", "native p95", "wasm p50", "wasm p95", "overhead p50", "overhead p95")
	for _, w := range workloads {
		ns, ws, st := env(t, w)
		// Warm up: instance pool, caches, JIT-compiled code paths.
		for range 50 {
			mustList(t, ns, st)
			mustList(t, ws, st)
		}
		nat := make([]time.Duration, n)
		was := make([]time.Duration, n)
		for i := range n {
			nat[i] = timeList(t, ns, st)
			was[i] = timeList(t, ws, st)
		}
		np50, np95 := pct(nat, 50), pct(nat, 95)
		wp50, wp95 := pct(was, 50), pct(was, 95)
		o50, o95 := wp50-np50, wp95-np95
		t.Logf("%-20s %10s %10s %10s %10s %12s %12s", w.name, r(np50), r(np95), r(wp50), r(wp95), r(o50), r(o95))
		if o95 > targetOverhead {
			t.Errorf("%s: overhead p95 %v exceeds the %v target", w.name, o95, targetOverhead)
		}
	}
}

func mustList(t *testing.T, s source.Source, st source.FetchState) {
	t.Helper()
	if _, err := s.List(context.Background(), target, st); err != nil {
		t.Fatal(err)
	}
}

func timeList(t *testing.T, s source.Source, st source.FetchState) time.Duration {
	t.Helper()
	start := time.Now()
	mustList(t, s, st)
	return time.Since(start)
}

func pct(d []time.Duration, p int) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	return s[(len(s)-1)*p/100]
}

func r(d time.Duration) string { return d.Round(time.Microsecond).String() }
