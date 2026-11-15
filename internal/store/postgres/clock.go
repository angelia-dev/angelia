package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// clockSamples is how many round trips ClockSkew measures (ADR 0028 §9).
const clockSamples = 5

// ClockSkew measures the host clock now against the database clock (ADR 0028
// §9, ADR 0031 §5). It reads clock_timestamp(), the time the server answers
// (now() is the start of the transaction), clockSamples times, noting now()
// before and after each read, and keeps the sample with the shortest round
// trip. skew is the database time minus the midpoint of the host times;
// uncertainty is half that round trip. A nil now means time.Now.
func (s *Store) ClockSkew(ctx context.Context, now func() time.Time) (skew, uncertainty time.Duration, err error) {
	if now == nil {
		now = time.Now
	}
	best := time.Duration(-1)
	for range clockSamples {
		before := now()
		var db time.Time
		if err := s.db.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&db); err != nil {
			return 0, 0, fmt.Errorf("store: read the database clock: %w", err)
		}
		after := now()
		rtt := after.Sub(before)
		if rtt < 0 {
			return 0, 0, errors.New("store: read the database clock: the host clock went backwards")
		}
		if best < 0 || rtt < best {
			best = rtt
			skew = db.Sub(before.Add(rtt / 2))
		}
	}
	return skew, best / 2, nil
}
