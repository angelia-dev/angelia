package scheduler

import "time"

// Clock is the scheduler's time source. Tests replace it with a fake so
// that jitter, backoff and lease expiry are deterministic without sleeps.
type Clock interface {
	Now() time.Time
	// Timer returns a channel that receives once d has elapsed, and a
	// function that stops the timer. The channel never receives after stop.
	Timer(d time.Duration) (<-chan time.Time, func())
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Timer(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}
