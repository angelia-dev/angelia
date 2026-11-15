package open

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// The clock-skew limits of the start-up check (ADR 0028 §9, ADR 0031 §5).
// Each host is measured against the database, so with a per-host refusal at
// 15 s any two hosts that start differ by at most 30 s, the minimum margin of
// ADR 0017 §2.
const (
	ClockSkewWarn   = 10 * time.Second
	ClockSkewRefuse = 15 * time.Second
)

// ErrClockSkew is returned by [CheckClock] when the host clock is too far from
// the database clock to share leases with other hosts.
var ErrClockSkew = errors.New("the host clock is too far from the database clock")

// clockSkewer is the engine side of the check: the Postgres engine has it.
type clockSkewer interface {
	ClockSkew(ctx context.Context, now func() time.Time) (skew, uncertainty time.Duration, err error)
}

// CheckClock compares the host clock now with the database clock before the
// scheduler or dispatcher role starts. Lease times are host times (D22), so a
// host that is off by more than [ClockSkewRefuse] would break the lease
// fences of other hosts: CheckClock then returns an error wrapping
// [ErrClockSkew]. Above [ClockSkewWarn] it logs a warning. The measured skew
// counts with the measurement's uncertainty (half the round trip) added. On
// SQLite, one process on one host, there is nothing to check. A nil now means
// time.Now, a nil log [slog.Default].
func CheckClock(ctx context.Context, st Store, now func() time.Time, log *slog.Logger) error {
	c, ok := st.(clockSkewer)
	if !ok {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	skew, uncertainty, err := c.ClockSkew(ctx, now)
	if err != nil {
		return err //nolint:wrapcheck // prefixed by the engine
	}
	off := skew.Abs() + uncertainty
	// Positive skew: the database clock is ahead of the host.
	attrs := []any{"skew", skew.Round(time.Millisecond).String(), "uncertainty", uncertainty.Round(time.Millisecond).String()}
	switch {
	case off > ClockSkewRefuse:
		return fmt.Errorf("%w: the database clock is %s ahead of this host (±%s), more than %s; "+
			"synchronise the host clock (NTP) before starting the scheduler or dispatcher role",
			ErrClockSkew, skew.Round(time.Millisecond), uncertainty.Round(time.Millisecond), ClockSkewRefuse)
	case off > ClockSkewWarn:
		log.Warn("the host clock is off from the database clock; leases need host clocks within "+
			ClockSkewRefuse.String()+" of the database, synchronise it (NTP)", attrs...)
	}
	return nil
}
