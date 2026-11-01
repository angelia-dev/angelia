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
	Window         Window              `json:"window"` // of Published, Seen and Lag
	Ecosystems     map[string]EcoStats `json:"ecosystems"`
	Total          EcoStats            `json:"total"`
}

// EcoStats counts the responses of one ecosystem (or all of them) and
// summarizes check lag.
type EcoStats struct {
	Requests    int64 `json:"requests"`
	OK          int64 `json:"ok"`
	NotModified int64 `json:"not_modified"`
	Throttled   int64 `json:"throttled"` // 429
	Forbidden   int64 `json:"forbidden"` // 403, primary limit
	NotFound    int64 `json:"not_found"`
	// Cancelled counts requests the client gave up on during the latency.
	Cancelled int64 `json:"cancelled"`
	// Published counts the releases published in the window.
	Published int `json:"published"`
	// Seen counts those that have been sent to a client at least once.
	Seen int      `json:"seen"`
	Lag  LagStats `json:"lag"`
}

// LagStats summarizes check lag: for a seen release, the time from its
// publication to the first 200 that contained it; for an unseen release,
// the time since its publication, a lower bound of its eventual lag. Both
// count, so a missed release counts against the percentiles; how much
// depends on how long before the snapshot it was published (see StatsIn).
type LagStats struct {
	Samples     int     `json:"samples"`
	P50Seconds  float64 `json:"p50_seconds"`
	P95Seconds  float64 `json:"p95_seconds"`
	MaxSeconds  float64 `json:"max_seconds"`
	MeanSeconds float64 `json:"mean_seconds"`
}

// Window limits the release counts and lag statistics to releases
// published in [From, To). A zero bound is open.
type Window struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

func (w Window) contains(t time.Time) bool {
	return (w.From.IsZero() || !t.Before(w.From)) && (w.To.IsZero() || t.Before(w.To))
}

// seenRelease is a release sent for the first time.
type seenRelease struct {
	pub time.Time
	lag time.Duration
}

// Stats returns a snapshot covering every release since the start.
func (s *Server) Stats() Stats { return s.StatsIn(Window{}) }

// StatsIn returns a snapshot whose release counts and lag cover only the
// releases published in w. A caller that stops at time T should end the
// window well before T: an unseen release counts with its age, so one
// published just before T would count with a near-zero lag.
func (s *Server) StatsIn(w Window) Stats {
	now := s.now()
	out := Stats{
		StartedAt: s.start, ElapsedSeconds: now.Sub(s.start).Seconds(), Window: w, Ecosystems: map[string]EcoStats{},
	}
	var all []time.Duration
	for _, name := range Ecosystems() {
		st, lags := s.ecos[name].stats(now, w)
		out.Ecosystems[name] = st
		all = append(all, lags...)
		t := &out.Total
		t.Requests += st.Requests
		t.OK += st.OK
		t.NotModified += st.NotModified
		t.Throttled += st.Throttled
		t.NotFound += st.NotFound
		t.Forbidden += st.Forbidden
		t.Cancelled += st.Cancelled
		t.Published += st.Published
		t.Seen += st.Seen
	}
	out.Total.Lag = summarize(all)
	return out
}

func (e *eco) stats(now time.Time, w Window) (EcoStats, []time.Duration) {
	st := EcoStats{
		Requests: e.requests.Load(), OK: e.ok.Load(), NotModified: e.notModified.Load(),
		Throttled: e.throttled.Load(), Forbidden: e.forbidden.Load(), NotFound: e.notFound.Load(),
		Cancelled: e.cancelled.Load(),
	}
	e.mu.Lock()
	var lags []time.Duration
	for _, r := range e.seen {
		if w.contains(r.pub) {
			lags = append(lags, r.lag)
		}
	}
	st.Seen = len(lags)
	for i := range e.served {
		for k, n := e.served[i], e.count(i, now); k < n; k++ {
			if pub := e.publishedAt(i, k); w.contains(pub) {
				lags = append(lags, now.Sub(pub))
			}
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
