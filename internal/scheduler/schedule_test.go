package scheduler

import (
	"testing"
	"time"
)

func TestJittered(t *testing.T) {
	tests := []struct {
		frac, r float64
		want    time.Duration
	}{
		{0.1, 0.5, time.Hour},
		{0.1, 0, 54 * time.Minute},
		{0.1, 1, 66 * time.Minute},
		{0.1, 0.25, 57 * time.Minute},
		{0, 0.99, time.Hour},
		{-1, 0, time.Hour},          // negative fraction: no jitter
		{2, 0, 0},                   // fraction clamped to 1
		{0.1, -5, 54 * time.Minute}, // r clamped to [0, 1]
	}
	for _, tc := range tests {
		if got := Jittered(time.Hour, tc.frac, tc.r); got != tc.want {
			t.Errorf("Jittered(1h, %v, %v) = %v, want %v", tc.frac, tc.r, got, tc.want)
		}
	}
}

func TestBackoff(t *testing.T) {
	const base, maxDelay = time.Minute, 24 * time.Hour
	want := []time.Duration{
		time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute,
		32 * time.Minute, 64 * time.Minute, 128 * time.Minute, 256 * time.Minute, 512 * time.Minute,
		1024 * time.Minute, maxDelay, maxDelay,
	}
	for i, w := range want {
		if got := Backoff(int64(i+1), base, maxDelay); got != w {
			t.Errorf("Backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
	if got := Backoff(1<<62, base, maxDelay); got != maxDelay {
		t.Errorf("huge error count: %v", got)
	}
	if got := Backoff(0, base, maxDelay); got != base {
		t.Errorf("Backoff(0) = %v", got)
	}
	if got := Backoff(3, 0, maxDelay); got != maxDelay {
		t.Errorf("zero base = %v", got)
	}
	if got := Backoff(1, 2*maxDelay, maxDelay); got != maxDelay {
		t.Errorf("base above cap = %v", got)
	}
}
