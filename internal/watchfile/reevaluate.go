package watchfile

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/angelia-dev/angelia/internal/subscription"
)

// DeadOnFilter is the LastError of a held delivery that an edited
// subscription filter no longer matches.
const DeadOnFilter = "subscription filter changed while held: no longer matches"

// DeadOnPolicy is the LastError of a held delivery that an edited
// subscription policy no longer admits.
const DeadOnPolicy = "subscription policy changed while held: pre-release excluded"

// reevaluateHeld applies an edited subscription filter and policy to the
// deliveries of the subscription that are held at r.now (ADR 0014 section 3).
// Deliveries that are merely queued are not touched. Each held delivery is
// first matched again against the new filter, from the input its hold
// recorded, and set dead (DeadOnFilter) when it no longer matches. Then it is
// decided again with subscription.Policy.Evaluate from what its hold
// recorded:
//   - released now when start + max(watch cooldown, new cooldown) has passed;
//   - moved to that instant when it is still ahead;
//   - dead, with DeadOnPolicy, when the new prerelease mode excludes it.
//
// It writes nothing on a dry run, and returns a summary for the plan. It is
// self-contained so that it can be dropped without touching the rest of apply.
func (r *reconciler) reevaluateHeld(subID string, f subscription.Filter, p subscription.Policy) (string, error) {
	held, err := r.tx.ListHeldDeliveries(r.ctx, subID, r.now)
	if err != nil {
		return "", err
	}
	var released, moved, dead int
	for _, d := range held {
		h := d.Hold
		if len(h.Input) > 0 {
			var in subscription.Input
			if json.Unmarshal(h.Input, &in) == nil && !f.Match(in) {
				dead++
				if err := r.write(func() error { return r.tx.FailHeldDelivery(r.ctx, d.ID, DeadOnFilter, r.now) }); err != nil {
					return "", err
				}
				continue
			}
		}
		dec := p.Evaluate(subscription.Event{
			Type: "release.published", Prerelease: h.Prerelease, CooldownStart: h.StartedAt, WatchCooldown: h.WatchCooldown,
		}, r.now)
		switch dec.Verdict {
		case subscription.Drop:
			dead++
			err = r.write(func() error { return r.tx.FailHeldDelivery(r.ctx, d.ID, DeadOnPolicy, r.now) })
		case subscription.Admit:
			released++
			err = r.write(func() error { return r.tx.ReleaseHeldDelivery(r.ctx, d.ID, r.now) })
		case subscription.Hold:
			if dec.Until.Equal(h.Until) {
				continue
			}
			moved++
			err = r.write(func() error { return r.tx.RescheduleHeldDelivery(r.ctx, d.ID, dec.Until, r.now) })
		}
		if err != nil {
			return "", err
		}
	}
	var parts []string
	for _, c := range []struct {
		n    int
		what string
	}{{released, "released"}, {moved, "rescheduled"}, {dead, "dead"}} {
		if c.n > 0 {
			parts = append(parts, fmt.Sprintf("%d held %s", c.n, c.what))
		}
	}
	return strings.Join(parts, ", "), nil
}
