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
//
// MaxBodyBytes caps the response body for responses served by this host,
// overriding Config.MaxBodyBytes. Zero means the built-in cap for the host if
// there is one (64 MiB for registry.npmjs.org, pypi.org and index.crates.io), else the
// global cap; so an entry that only tunes rates keeps the built-in cap. The
// body is counted after the transport's transparent gzip decoding; a caller
// that sets Accept-Encoding itself receives, and is capped on, wire bytes.
// The cap follows the host that actually serves the body: a redirect from a
// host with a large cap to one without does not carry the large cap along
// (and the reverse). It lives here, not in a context option, so that one
// table configures everything known about a host, a caller cannot raise a cap
// per request, and the cap stays tied to the host the SSRF guard approved.
// A non-zero value in Config.FallbackLimit is rejected: the fallback only
// describes rates.
type HostLimit struct {
	Authenticated Limit
	Anonymous     Limit
	MaxBodyBytes  int64
	// Timeout bounds one attempt against this host, including reading the
	// body, overriding Config.Timeout. Zero means the built-in timeout for
	// the host if there is one (2 minutes for the large registries), else
	// the global one. Like the body cap it is chosen from the hop's host.
	Timeout time.Duration
}

func (h HostLimit) validate() error {
	if h.MaxBodyBytes < 0 {
		return fmt.Errorf("max body bytes must not be negative, got %d", h.MaxBodyBytes)
	}
	if h.Timeout < 0 {
		return fmt.Errorf("timeout must not be negative, got %v", h.Timeout)
	}
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

// Built-in allowances for registry hosts that serve single documents above
// the global 10 MiB cap: npm packuments reach 31.5 MB (next), the swc_core
// sparse index file is 16.0 MB, and PyPI JSON responses can be large too
// (npm and crates measured 2026-11-01). Big bodies also take longer than the
// global 30 s attempt timeout on a slow link.
const (
	largeRegistryMaxBodyBytes = 64 << 20 // 64 MiB
	largeRegistryTimeout      = 2 * time.Minute
)

// hostAllowance is the built-in body cap and per-attempt timeout of a host.
type hostAllowance struct {
	maxBodyBytes int64
	timeout      time.Duration
}

// defaultHostAllowances are kept apart from the rate limits so that a custom
// FallbackLimit still sets those hosts' rates.
var defaultHostAllowances = map[string]hostAllowance{
	"registry.npmjs.org": {largeRegistryMaxBodyBytes, largeRegistryTimeout},
	"pypi.org":           {largeRegistryMaxBodyBytes, largeRegistryTimeout},
	"index.crates.io":    {largeRegistryMaxBodyBytes, largeRegistryTimeout},
}

// maxBodyBytes returns the body cap for host: the operator's HostLimit
// MaxBodyBytes if above zero, else the built-in cap for the host, else global.
func (l *limiter) maxBodyBytes(host string, global int64) int64 {
	h := normHost(host)
	if hl, ok := l.limits[h]; ok && hl.MaxBodyBytes > 0 {
		return hl.MaxBodyBytes
	}
	if a, ok := defaultHostAllowances[h]; ok {
		return a.maxBodyBytes
	}
	return global
}

// timeout returns the per-attempt timeout for host, resolved like
// maxBodyBytes: operator HostLimit.Timeout, built-in, then global.
func (l *limiter) timeout(host string, global time.Duration) time.Duration {
	h := normHost(host)
	if hl, ok := l.limits[h]; ok && hl.Timeout > 0 {
		return hl.Timeout
	}
	if a, ok := defaultHostAllowances[h]; ok {
		return a.timeout
	}
	return global
}

// OtherHost is the host reported to an Observer for hosts without an entry
// in the limits table.
const OtherHost = "other"

// observedHost returns host if it has an entry in the limits table, else
// OtherHost, so that metrics never carry the host of a sink or a mirror.
func (l *limiter) observedHost(host string) string {
	h := normHost(host)
	if _, ok := l.limits[h]; ok {
		return h
	}
	return OtherHost
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
