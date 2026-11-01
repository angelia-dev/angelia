package scheduler

import "time"

// Jittered spreads d by up to ±frac of itself: r in [0, 1) maps linearly to
// [d·(1−frac), d·(1+frac)), so r = 0.5 gives d exactly. frac is clamped to
// [0, 1]. Jitter keeps targets created together from staying in lockstep
// and hitting a registry in bursts (architecture.md §3.2).
func Jittered(d time.Duration, frac, r float64) time.Duration {
	frac = min(max(frac, 0), 1)
	r = min(max(r, 0), 1)
	return time.Duration(float64(d) * (1 + frac*(2*r-1)))
}

// Backoff is the delay before the next check of a target that has failed
// errorCount times in a row (errorCount >= 1): base·2^(errorCount−1),
// capped at maxDelay. errorCount <= 0 returns base.
func Backoff(errorCount int64, base, maxDelay time.Duration) time.Duration {
	if base <= 0 {
		return maxDelay
	}
	d := base
	for i := int64(1); i < errorCount; i++ {
		if d >= maxDelay/2 {
			return maxDelay
		}
		d *= 2
	}
	return min(d, maxDelay)
}
