package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// delayMetric is the scheduler's histogram of how late each check started:
// lease time minus the time the target was due (internal/metrics). It is the
// due-to-check delay of benchmark 1 (architecture.md section 6).
const delayMetric = "angelia_check_lag_seconds"

// delayBucket is one cumulative bucket of the histogram: observations <= le.
type delayBucket struct {
	le    float64
	count float64
}

// delayHist is a scraped Prometheus histogram, buckets in ascending order.
type delayHist struct{ buckets []delayBucket }

// DueDelay summarizes the due-to-check delay over the measuring window, in
// seconds. Open means a quantile fell above the highest finite bucket, so
// its value is that bound and the real one is higher.
type DueDelay struct {
	Samples    uint64  `json:"samples"`
	P50Seconds float64 `json:"p50_seconds"`
	P95Seconds float64 `json:"p95_seconds"`
	P99Seconds float64 `json:"p99_seconds"`
	Open       bool    `json:"open_ended,omitempty"`
}

// parseDelayHistogram reads the bucket lines of delayMetric from a
// Prometheus text exposition.
func parseDelayHistogram(body string) (delayHist, error) {
	var h delayHist
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), delayMetric+`_bucket{`)
		if !ok {
			continue
		}
		labels, value, ok := strings.Cut(rest, "} ")
		if !ok {
			return delayHist{}, fmt.Errorf("malformed bucket line %q", sc.Text())
		}
		bound, ok := strings.CutPrefix(labels, `le="`)
		if !ok {
			return delayHist{}, fmt.Errorf("bucket line without le: %q", sc.Text())
		}
		bound = strings.TrimSuffix(bound, `"`)
		le, err := strconv.ParseFloat(bound, 64) // "+Inf" parses
		if err != nil {
			return delayHist{}, fmt.Errorf("bucket bound %q: %w", bound, err)
		}
		n, err := strconv.ParseFloat(strings.Fields(value)[0], 64)
		if err != nil {
			return delayHist{}, fmt.Errorf("bucket count %q: %w", value, err)
		}
		h.buckets = append(h.buckets, delayBucket{le, n})
	}
	if err := sc.Err(); err != nil {
		return delayHist{}, err
	}
	if len(h.buckets) == 0 {
		return delayHist{}, fmt.Errorf("%s not found in the metrics", delayMetric)
	}
	return h, nil
}

// scrapeDelay reads the histogram from base+"/metrics".
func scrapeDelay(ctx context.Context, base string) (delayHist, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/metrics", nil)
	if err != nil {
		return delayHist{}, err
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // loopback URL of the instance under test
	if err != nil {
		return delayHist{}, fmt.Errorf("scrape metrics: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return delayHist{}, fmt.Errorf("scrape metrics: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return delayHist{}, fmt.Errorf("scrape metrics: %w", err)
	}
	return parseDelayHistogram(string(body))
}

func (h delayHist) total() float64 {
	if len(h.buckets) == 0 {
		return 0
	}
	return h.buckets[len(h.buckets)-1].count
}

// since returns the observations made after the earlier scrape. A zero
// earlier scrape means "from the start". Counters that went down (a restart)
// or different buckets are an error.
func (h delayHist) since(earlier delayHist) (delayHist, error) {
	if len(earlier.buckets) == 0 {
		return h, nil
	}
	if len(earlier.buckets) != len(h.buckets) {
		return delayHist{}, errors.New("the histogram buckets changed between scrapes")
	}
	out := delayHist{buckets: make([]delayBucket, len(h.buckets))}
	for i, b := range h.buckets {
		if earlier.buckets[i].le != b.le {
			return delayHist{}, errors.New("the histogram buckets changed between scrapes")
		}
		d := b.count - earlier.buckets[i].count
		if d < 0 {
			return delayHist{}, errors.New("the histogram counters went down between scrapes: did angelia restart?")
		}
		out.buckets[i] = delayBucket{b.le, d}
	}
	return out, nil
}

// quantile interpolates linearly inside the bucket holding the rank, as
// PromQL's histogram_quantile does. open reports a rank in the +Inf bucket,
// where the result is the highest finite bound.
func (h delayHist) quantile(q float64) (v float64, open bool) {
	total := h.total()
	if total == 0 {
		return 0, false
	}
	rank := q * total
	lower, prev := 0.0, 0.0
	for _, b := range h.buckets {
		if b.count >= rank {
			if math.IsInf(b.le, 1) {
				return lower, true
			}
			if b.count == prev {
				return b.le, false
			}
			return lower + (b.le-lower)*(rank-prev)/(b.count-prev), false
		}
		if !math.IsInf(b.le, 1) {
			lower = b.le
		}
		prev = b.count
	}
	return lower, true
}

func (h delayHist) summary() DueDelay {
	d := DueDelay{Samples: uint64(h.total())}
	if d.Samples == 0 {
		return d
	}
	var o1, o2, o3 bool
	d.P50Seconds, o1 = h.quantile(0.50)
	d.P95Seconds, o2 = h.quantile(0.95)
	d.P99Seconds, o3 = h.quantile(0.99)
	d.Open = o1 || o2 || o3
	return d
}
