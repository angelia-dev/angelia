package dispatch

import "time"

// RetryCeiling is the upper bound of the wait after the failed-th failed
// attempt (failed >= 1): base·2^(failed−1), capped at maxDelay. failed <= 0
// returns base.
func RetryCeiling(failed int64, base, maxDelay time.Duration) time.Duration {
	if base <= 0 {
		return max(maxDelay, 0)
	}
	d := base
	for i := int64(1); i < failed; i++ {
		if d >= maxDelay/2 {
			return maxDelay
		}
		d *= 2
	}
	return min(d, maxDelay)
}

// RetryDelay is the wait before the next attempt after the failed-th failed
// attempt, with full jitter: r in [0, 1) maps linearly onto
// [0, RetryCeiling). Full jitter spreads the retries of deliveries that
// failed together (a sink outage) over the whole window instead of sending
// them back in waves. r is clamped to [0, 1].
//
// With the defaults (base 12 min, cap 12 h, 8 attempts) the seven waits
// have ceilings of 12 min, 24 min, 48 min, 1 h 36, 3 h 12, 6 h 24 and 12 h:
// the last attempt comes at most 24 h 36 min after the first, about 12 h
// on average.
func RetryDelay(failed int64, base, maxDelay time.Duration, r float64) time.Duration {
	r = min(max(r, 0), 1)
	return time.Duration(r * float64(RetryCeiling(failed, base, maxDelay)))
}
