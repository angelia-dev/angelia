//go:build bench

// Benchmark 4 (architecture.md section 6): source plugin round-trip
// overhead per List call vs native, target < 5 ms. Measured for the WASM
// plugin (ADR 0011) and the go-plugin subprocess (ADR 0013).
//
//	go test -tags bench -run TestBenchmark4 -v ./bench/wasm
//
// Calls rotate native/wasm/goplugin on the same netguard client and
// in-memory fake upstream, so drift affects all equally. Overhead p50/p95
// is the difference of the two latency percentiles.

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
	t.Logf("%-10s %-20s %10s %10s %10s %10s %12s %12s", "runtime", "workload", "native p50", "native p95", "plugin p50", "plugin p95", "overhead p50", "overhead p95")
	for _, w := range workloads {
		srcs, st := env(t, w)
		// Warm up: instance pool, caches, JIT-compiled code paths, gRPC connections.
		for range 50 {
			for _, im := range srcs {
				mustList(t, im.src, st)
			}
		}
		lat := make([][]time.Duration, len(srcs))
		for i := range lat {
			lat[i] = make([]time.Duration, n)
		}
		for i := range n {
			for j, im := range srcs {
				lat[j][i] = timeList(t, im.src, st)
			}
		}
		np50, np95 := pct(lat[0], 50), pct(lat[0], 95)
		for j, im := range srcs[1:] {
			p50, p95 := pct(lat[j+1], 50), pct(lat[j+1], 95)
			o50, o95 := p50-np50, p95-np95
			t.Logf("%-10s %-20s %10s %10s %10s %10s %12s %12s", im.name, w.name, r(np50), r(np95), r(p50), r(p95), r(o50), r(o95))
			if o95 > targetOverhead {
				t.Errorf("%s %s: overhead p95 %v exceeds the %v target", im.name, w.name, o95, targetOverhead)
			}
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
