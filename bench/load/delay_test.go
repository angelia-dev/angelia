package main

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseDelayHistogram(t *testing.T) {
	h, err := parseDelayHistogram(`# TYPE angelia_check_lag_seconds histogram
angelia_check_lag_seconds_bucket{le="1"} 2
angelia_check_lag_seconds_bucket{le="120"} 8
angelia_check_lag_seconds_bucket{le="+Inf"} 10
angelia_check_lag_seconds_sum 100
angelia_check_lag_seconds_count 10
other_bucket{le="1"} 77
`)
	if err != nil {
		t.Fatal(err)
	}
	if h.total() != 10 || len(h.buckets) != 3 || h.buckets[1].le != 120 || !math.IsInf(h.buckets[2].le, 1) {
		t.Errorf("histogram = %+v", h)
	}
	if _, err := parseDelayHistogram("nothing here\n"); err == nil {
		t.Error("a scrape without the metric must be an error")
	}
	if _, err := parseDelayHistogram("angelia_check_lag_seconds_bucket{le=\"x\"} 1\n"); err == nil {
		t.Error("a bad bound must be an error")
	}
}

// Quantiles interpolate linearly inside a bucket, as PromQL's
// histogram_quantile does.
func TestDelayQuantiles(t *testing.T) {
	// 100 observations: 50 in (0,60], 40 in (60,120], 9 in (120,300], 1 above.
	h := delayHist{buckets: []delayBucket{{60, 50}, {120, 90}, {300, 99}, {math.Inf(1), 100}}}
	d := h.summary()
	if d.Samples != 100 {
		t.Fatalf("samples = %d", d.Samples)
	}
	if math.Abs(d.P50Seconds-60) > 1e-9 {
		t.Errorf("P50 = %v, want 60", d.P50Seconds)
	}
	// rank 95 is 5 of the 9 in (120,300]: 120 + 180*5/9 = 220
	if math.Abs(d.P95Seconds-220) > 1e-9 {
		t.Errorf("P95 = %v, want 220", d.P95Seconds)
	}
	// rank 99 is the last of the 9: 300
	if math.Abs(d.P99Seconds-300) > 1e-9 {
		t.Errorf("P99 = %v, want 300", d.P99Seconds)
	}
	// All in the first bucket: rank 95 of 100 is 0.95 of the way to 60.
	one := delayHist{buckets: []delayBucket{{60, 100}, {math.Inf(1), 100}}}.summary()
	if math.Abs(one.P95Seconds-57) > 1e-9 {
		t.Errorf("first bucket P95 = %v, want 57", one.P95Seconds)
	}
	// A quantile in the +Inf bucket is the highest finite bound, flagged.
	tail := delayHist{buckets: []delayBucket{{300, 90}, {math.Inf(1), 100}}}.summary()
	if tail.P95Seconds != 300 || !tail.P95Open {
		t.Errorf("open tail = %+v", tail)
	}
	// P99 in +Inf while P95 is inside a bucket: only P99 is open.
	p99 := delayHist{buckets: []delayBucket{{120, 97}, {math.Inf(1), 100}}}.summary()
	if p99.P95Open || !p99.P99Open || p99.P95Seconds >= 120 {
		t.Errorf("open P99 only: %+v", p99)
	}
	// A rank exactly on a bucket boundary is that bucket's bound.
	edge := delayHist{buckets: []delayBucket{{60, 95}, {120, 100}, {math.Inf(1), 100}}}.summary()
	if math.Abs(edge.P95Seconds-60) > 1e-9 || edge.P95Open {
		t.Errorf("boundary P95 = %+v", edge)
	}
	// Only a +Inf bucket: everything is open at zero.
	inf := delayHist{buckets: []delayBucket{{math.Inf(1), 10}}}.summary()
	if !inf.P50Open || !inf.P95Open || !inf.P99Open || inf.P95Seconds != 0 {
		t.Errorf("+Inf only = %+v", inf)
	}
	if e := (delayHist{}).summary(); e.Samples != 0 {
		t.Errorf("empty = %+v", e)
	}
}

// The window is the difference of two scrapes; a restart (counters going
// down) is an error, not a negative count.
func TestDelayWindow(t *testing.T) {
	start := delayHist{buckets: []delayBucket{{60, 10}, {math.Inf(1), 10}}}
	end := delayHist{buckets: []delayBucket{{60, 25}, {math.Inf(1), 30}}}
	w, err := end.since(start)
	if err != nil {
		t.Fatal(err)
	}
	if w.total() != 20 || w.buckets[0].count != 15 {
		t.Errorf("window = %+v", w)
	}
	// A lower _count with higher buckets is a restart too.
	if _, err := (delayHist{buckets: end.buckets, count: 5}).since(delayHist{buckets: start.buckets, count: 9}); err == nil {
		t.Error("a _count that went down must be an error")
	}
	if _, err := start.since(end); err == nil {
		t.Error("counters that went down must be an error")
	}
	if _, err := end.since(delayHist{buckets: []delayBucket{{30, 1}}}); err == nil {
		t.Error("different buckets must be an error")
	}
	if w, err := end.since(delayHist{}); err != nil || w.total() != 30 {
		t.Errorf("no start scrape: %+v %v", w, err)
	}
}

func TestScrapeDelay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("angelia_check_lag_seconds_bucket{le=\"120\"} 3\nangelia_check_lag_seconds_bucket{le=\"+Inf\"} 4\n"))
	}))
	defer srv.Close()
	h, err := scrapeDelay(context.Background(), srv.URL)
	if err != nil || h.total() != 4 {
		t.Fatalf("scrape = %+v, %v", h, err)
	}
	if _, err := scrapeDelay(context.Background(), srv.URL+"/nope"); err == nil {
		t.Error("a 404 must be an error")
	}
}
