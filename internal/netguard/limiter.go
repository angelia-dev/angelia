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
// api.github.com allows 5,000 requests per hour with a token and 60 without;
// the buckets stay within those budgets over any one-hour window.
func DefaultHostLimits() map[string]HostLimit {
	// The arguments are constants that satisfy Budget's checks.
	ghAuth, _ := Budget(5000, time.Hour, 10)
	ghAnon, _ := Budget(60, time.Hour, 1)
	return map[string]HostLimit{
		"api.github.com": {Authenticated: ghAuth, Anonymous: ghAnon},
	}
}

// Limiter is a set of per-host token buckets. It is safe for concurrent use.
type Limiter struct {
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
// negative token is a slot already promised to a waiting caller.
type bucket struct {
	limit  Limit
	tokens float64
	last   time.Time
}

// NewLimiter returns a limiter that uses limits for the listed hosts (keys
// are matched case-insensitively) and fallback for every other host.
// Limits are not validated here; [New] validates them.
func NewLimiter(clock Clock, fallback HostLimit, limits map[string]HostLimit) *Limiter {
	if clock == nil {
		clock = realClock{}
	}
	lower := make(map[string]HostLimit, len(limits))
	for h, l := range limits {
		lower[strings.ToLower(h)] = l
	}
	return &Limiter{
		clock:    clock,
		fallback: fallback,
		limits:   lower,
		maxPause: defaultMaxPause,
		buckets:  make(map[bucketKey]*bucket),
	}
}

// bucketLocked returns the bucket for host, creating it full. l.mu must be held.
func (l *Limiter) bucketLocked(host string, authed bool, now time.Time) *bucket {
	k := bucketKey{host: strings.ToLower(host), authed: authed}
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

// Wait blocks until a request to host may be sent. A token taken by a
// caller whose context ends is not given back, so the budget is never
// exceeded.
func (l *Limiter) Wait(ctx context.Context, host string, authed bool) error {
	l.mu.Lock()
	now := l.clock.Now()
	ready := l.bucketLocked(host, authed, now).reserve(now)
	l.mu.Unlock()
	if err := l.clock.Sleep(ctx, ready.Sub(now)); err != nil {
		return fmt.Errorf("netguard: rate limit wait for %s: %w", host, err)
	}
	return nil
}

// Pause stops requests to host until the given time and empties the bucket
// at that point. Slots already promised are pushed back as well. A time in
// the past is ignored; one further away than an hour is capped.
func (l *Limiter) Pause(host string, authed bool, until time.Time) {
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
	if until.After(b.last) {
		b.tokens = math.Min(b.tokens, 0)
		b.last = until
	}
}
