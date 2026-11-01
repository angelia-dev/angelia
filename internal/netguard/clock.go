package netguard

import (
	"context"
	"time"
)

// Clock is the time source used by the limiter and the retry loop. Tests
// replace it with a fake.
type Clock interface {
	Now() time.Time
	// Sleep blocks for d or until ctx is done, and returns ctx.Err() in the
	// latter case.
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
