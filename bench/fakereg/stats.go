package fakereg

import (
	"math"
	"slices"
	"time"
)

// Stats is what the server observed since it started.
type Stats struct {
	StartedAt      time.Time           `json:"started_at"`
	ElapsedSeconds float64             `json:"elapsed_seconds"`
	Ecosystems     map[string]EcoStats `json:"ecosystems"`
	Total          EcoStats            `json:"total"`
}

// EcoStats counts the responses of one ecosystem (or all of them) and
// summarizes check lag.
type EcoStats struct {
	Requests    int64 `json:"requests"`
	OK          int64 `json:"ok"`
	NotModified int64 `json:"not_modified"`
	Throttled   int64 `json:"throttled"`
	NotFound    int64 `json:"not_found"`
	// Published counts the releases published since the start.
	Published int `json:"published"`
	// Seen counts those that have been sent to a client at least once.
	Seen int      `json:"seen"`
	Lag  LagStats `json:"lag"`
}

// LagStats summarizes check lag: for a seen release, the time from its
// publication to the first 200 that contained it; for an unseen release,
// the time since its publication, a lower bound of its eventual lag. Both
// count, so missed releases make the percentiles worse, never better.
type LagStats struct {
	Samples     int     `json:"samples"`
	P50Seconds  float64 `json:"p50_seconds"`
	P95Seconds  float64 `json:"p95_seconds"`
	MaxSeconds  float64 `json:"max_seconds"`
	MeanSeconds float64 `json:"mean_seconds"`
}

// Stats returns a snapshot.
func (s *Server) Stats() Stats {
	now := s.now()
	out := Stats{StartedAt: s.start, ElapsedSeconds: now.Sub(s.start).Seconds(), Ecosystems: map[string]EcoStats{}}
	var all []time.Duration
	for _, name := range Ecosystems {
		st, lags := s.ecos[name].stats(now)
		out.Ecosystems[name] = st
		all = append(all, lags...)
		t := &out.Total
		t.Requests += st.Requests
		t.OK += st.OK
		t.NotModified += st.NotModified
		t.Throttled += st.Throttled
		t.NotFound += st.NotFound
		t.Published += st.Published
		t.Seen += st.Seen
	}
	out.Total.Lag = summarize(all)
	return out
}

func (e *eco) stats(now time.Time) (EcoStats, []time.Duration) {
	st := EcoStats{
		Requests: e.requests.Load(), OK: e.ok.Load(), NotModified: e.notModified.Load(),
		Throttled: e.throttled.Load(), NotFound: e.notFound.Load(),
	}
	e.mu.Lock()
	lags := slices.Clone(e.lags)
	st.Seen = len(lags)
	for i := range e.served {
		for k, n := e.served[i], e.count(i, now); k < n; k++ {
			lags = append(lags, now.Sub(e.publishedAt(i, k)))
		}
	}
	e.mu.Unlock()
	st.Published = len(lags)
	st.Lag = summarize(lags)
	return st, lags
}

func summarize(lags []time.Duration) LagStats {
	if len(lags) == 0 {
		return LagStats{}
	}
	slices.Sort(lags)
	var sum time.Duration
	for _, l := range lags {
		sum += l
	}
	return LagStats{
		Samples:     len(lags),
		P50Seconds:  percentile(lags, 0.50).Seconds(),
		P95Seconds:  percentile(lags, 0.95).Seconds(),
		MaxSeconds:  lags[len(lags)-1].Seconds(),
		MeanSeconds: (sum / time.Duration(len(lags))).Seconds(),
	}
}

// percentile uses the nearest-rank method on sorted values.
func percentile(sorted []time.Duration, p float64) time.Duration {
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[min(max(i, 0), len(sorted)-1)]
}
