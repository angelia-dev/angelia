package netguard

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// defaultMaxPause caps how long a server-sent reset or Retry-After may pause
// a host, so that a bogus header cannot block it for days.
const defaultMaxPause = time.Hour

// Limit is a token bucket: Burst requests at once, refilled at Rate requests
// per second. Over any window of length T it admits at most
// Burst + Rate*T requests.
type Limit struct {
	Rate  float64
	Burst int
}

func (l Limit) validate() error {
	if !(l.Rate > 0) || math.IsInf(l.Rate, 0) {
		return fmt.Errorf("rate must be positive, got %v", l.Rate)
	}
	if l.Burst < 1 {
		return fmt.Errorf("burst must be at least 1, got %d", l.Burst)
	}
	return nil
}

// Budget returns a Limit that admits at most n requests in any window of
// length per, with the given burst: the refill rate is (n-burst)/per.
func Budget(n int, per time.Duration, burst int) (Limit, error) {
	if per <= 0 || burst < 1 || n <= burst {
		return Limit{}, fmt.Errorf("netguard: invalid budget %d per %v with burst %d", n, per, burst)
	}
	return Limit{Rate: float64(n-burst) / per.Seconds(), Burst: burst}, nil
}

// HostLimit holds the limits for one host. A request carrying an
// Authorization header uses Authenticated; others use Anonymous. The two
// budgets are tracked in separate buckets.
type HostLimit struct {
	Authenticated Limit
	Anonymous     Limit
}

func (h HostLimit) validate() error {
	if err := h.Authenticated.validate(); err != nil {
		return fmt.Errorf("authenticated: %w", err)
	}
	if err := h.Anonymous.validate(); err != nil {
		return fmt.Errorf("anonymous: %w", err)
	}
	return nil
}

// DefaultFallbackLimit applies to hosts without an entry in the host limits:
// 5 requests per second with a burst of 5.
func DefaultFallbackLimit() HostLimit {
	l := Limit{Rate: 5, Burst: 5}
	return HostLimit{Authenticated: l, Anonymous: l}
}

// DefaultHostLimits returns the built-in limits for known hosts, keyed by
// lower-case hostname. It returns a new map on every call.
//
// api.github.com allows 5,000 requests per hour with a token and 60 without.
// Burst + Rate*1h equals those budgets (see Budget), so the buckets never
// exceed them over any one-hour window.
//
// crates.io's crawler policy allows one request per second to its API, so
// crates.io gets a rate of 1 per second with no burst beyond a single
// request. The sparse index (index.crates.io) is a CDN meant for bulk reads
// and keeps the fallback limit.
func DefaultHostLimits() map[string]HostLimit {
	crates := Limit{Rate: 1, Burst: 1}
	return map[string]HostLimit{
		"crates.io": {Authenticated: crates, Anonymous: crates},
		"api.github.com": {
			Authenticated: Limit{Rate: (5000 - 10) / 3600.0, Burst: 10},
			Anonymous:     Limit{Rate: (60 - 1) / 3600.0, Burst: 1},
		},
	}
}

// normHost returns the key used for a host: lower case, without a trailing dot.
func normHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// limiter is a set of per-host token buckets. It is safe for concurrent use.
type limiter struct {
	clock    Clock
	fallback HostLimit
	limits   map[string]HostLimit
	maxPause time.Duration

	mu      sync.Mutex
	buckets map[bucketKey]*bucket
}

type bucketKey struct {
	host   string
	authed bool
}

// bucket tracks tokens as of time last. Tokens may go negative: each
// negative token is a slot already promised to a waiting caller. shift is
// the total time pauses have pushed the bucket back; waiters compare it
// before and after sleeping to pick up a pause that arrived meanwhile.
type bucket struct {
	limit  Limit
	tokens float64
	last   time.Time
	shift  time.Duration
}

// newLimiter returns a limiter that uses limits for the listed hosts and
// fallback for every other host. Limits are validated by New.
func newLimiter(clock Clock, fallback HostLimit, limits map[string]HostLimit) *limiter {
	if clock == nil {
		clock = realClock{}
	}
	norm := make(map[string]HostLimit, len(limits))
	for h, l := range limits {
		norm[normHost(h)] = l
	}
	return &limiter{
		clock:    clock,
		fallback: fallback,
		limits:   norm,
		maxPause: defaultMaxPause,
		buckets:  make(map[bucketKey]*bucket),
	}
}

// bucketLocked returns the bucket for host, creating it full. l.mu must be held.
func (l *limiter) bucketLocked(host string, authed bool, now time.Time) *bucket {
	k := bucketKey{host: normHost(host), authed: authed}
	b, ok := l.buckets[k]
	if !ok {
		hl, ok := l.limits[k.host]
		if !ok {
			hl = l.fallback
		}
		lim := hl.Anonymous
		if authed {
			lim = hl.Authenticated
		}
		b = &bucket{limit: lim, tokens: float64(lim.Burst), last: now}
		l.buckets[k] = b
	}
	return b
}

// advance refills the bucket up to now. A bucket paused into the future is
// left alone.
func (b *bucket) advance(now time.Time) {
	if !now.After(b.last) {
		return
	}
	b.tokens = math.Min(float64(b.limit.Burst), b.tokens+now.Sub(b.last).Seconds()*b.limit.Rate)
	b.last = now
}

// reserve takes one token and returns the time at which the caller may go.
func (b *bucket) reserve(now time.Time) time.Time {
	b.advance(now)
	b.tokens--
	start := b.last // later than now while the bucket is paused
	if b.tokens >= 0 {
		return start
	}
	wait := math.Ceil(-b.tokens / b.limit.Rate * float64(time.Second))
	return start.Add(time.Duration(wait))
}

// refund gives back a token that a caller reserved but did not use.
func (l *limiter) refund(b *bucket) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b.advance(l.clock.Now())
	b.tokens = math.Min(b.tokens+1, float64(b.limit.Burst))
}

// wait blocks until a request to host may be sent. A caller whose context
// ends, or whose deadline comes before its slot, gets an error and its
// token back.
func (l *limiter) wait(ctx context.Context, host string, authed bool) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("netguard: rate limit wait for %s: %w", host, err)
	}
	l.mu.Lock()
	now := l.clock.Now()
	b := l.bucketLocked(host, authed, now)
	ready := b.reserve(now)
	shift := b.shift
	l.mu.Unlock()

	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < ready.Sub(now) {
		l.refund(b)
		return fmt.Errorf("netguard: rate limit wait for %s would pass the deadline: %w", host, context.DeadlineExceeded)
	}
	for {
		if err := l.clock.Sleep(ctx, ready.Sub(now)); err != nil {
			l.refund(b)
			return fmt.Errorf("netguard: rate limit wait for %s: %w", host, err)
		}
		l.mu.Lock()
		extra := b.shift - shift
		shift = b.shift
		now = l.clock.Now()
		l.mu.Unlock()
		if extra <= 0 {
			return nil
		}
		ready = ready.Add(extra) // a pause arrived while we slept
	}
}

// pause stops requests to host until the given time and empties the bucket
// at that point. Slots already promised, including those of callers
// sleeping in wait, are pushed back by the same amount. A time in the past
// is ignored; one further away than maxPause is capped.
func (l *limiter) pause(host string, authed bool, until time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	if !until.After(now) {
		return
	}
	if limit := now.Add(l.maxPause); until.After(limit) {
		until = limit
	}
	b := l.bucketLocked(host, authed, now)
	b.advance(now)
	if delta := until.Sub(b.last); delta > 0 {
		b.tokens = math.Min(b.tokens, 0)
		b.last = until
		b.shift += delta
	}
}
