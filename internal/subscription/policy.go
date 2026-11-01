package subscription

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/angelia-dev/angelia/internal/policy"
)

// Pre-release modes, the same words as the watch policy's prerelease key.
const (
	PrereleaseInherit = ""        // subscription only: no opinion, keep what the watch admitted
	PrereleaseNone    = "none"    // final versions only
	PrereleaseOnly    = "only"    // pre-releases only
	PrereleaseInclude = "include" // both
)

// MaxCooldown is the longest cooldown, the watch's limit.
const MaxCooldown = policy.MaxCooldown

// eventYanked is the type of events a Policy never delays or drops.
const eventYanked = "release.yanked"

// Policy is the narrowing policy of a subscription (ADR 0014). It can only
// receive less, later, than the watch policy admitted: Evaluate intersects it
// with the watch's result, so no value widens. The bump key is not here; it
// is Filter.Bump.
type Policy struct {
	// Prerelease is "", "none", "only" or "include". "" and "include"
	// narrow nothing.
	Prerelease string `json:"prerelease"`
	// Cooldown is the minimum age of a version, from the same start as the
	// watch's cooldown: the effective hold is max(watch, subscription).
	Cooldown time.Duration `json:"-"`
}

type policyJSON struct {
	Prerelease string `json:"prerelease"`
	Cooldown   string `json:"cooldown"`
}

// Clamp returns the policy with useless values cut to the nearest useful one:
// a negative cooldown becomes 0 and one above MaxCooldown becomes MaxCooldown.
// It never fails (ADR 0014 decision 2).
func (p Policy) Clamp() Policy {
	p.Cooldown = min(max(p.Cooldown, 0), MaxCooldown)
	return p
}

// MarshalJSON writes the stored form, defaults filled.
func (p Policy) MarshalJSON() ([]byte, error) {
	return json.Marshal(policyJSON{Prerelease: p.Prerelease, Cooldown: p.Cooldown.String()})
}

// PolicyFromJSON decodes subscription.policy_json. Empty, "null" and "{}"
// give the zero policy (inherit everything). Unknown keys are errors. The
// result is clamped.
func PolicyFromJSON(b []byte) (Policy, error) {
	var p Policy
	if t := bytes.TrimSpace(b); len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return p, nil
	}
	var j policyJSON
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&j); err != nil {
		return Policy{}, fmt.Errorf("subscription: decode policy: %w", err)
	}
	switch j.Prerelease {
	case PrereleaseInherit, PrereleaseNone, PrereleaseOnly, PrereleaseInclude:
	default:
		return Policy{}, fmt.Errorf("subscription: unknown prerelease mode %q", j.Prerelease)
	}
	p.Prerelease = j.Prerelease
	if j.Cooldown != "" {
		d, err := time.ParseDuration(j.Cooldown)
		if err != nil {
			return Policy{}, fmt.Errorf("subscription: policy cooldown: %w", err)
		}
		p.Cooldown = d
	}
	return p.Clamp(), nil
}

// Verdict is what a subscription does with an event.
type Verdict int

// The verdicts of Evaluate.
const (
	Admit Verdict = iota // create the delivery, due now
	Hold                 // create the delivery, due at Decision.Until
	Drop                 // create no delivery
)

func (v Verdict) String() string {
	switch v {
	case Admit:
		return "admit"
	case Hold:
		return "hold"
	case Drop:
		return "drop"
	}
	return fmt.Sprintf("verdict(%d)", int(v))
}

// Decision is the result of Evaluate. Until is set only for Hold.
type Decision struct {
	Verdict Verdict
	Until   time.Time
}

// Event is what Evaluate needs of the event and of the watch policy's result
// for it. None of it needs I/O.
type Event struct {
	// Type is the event type; "release.yanked" is not subject to Policy.
	Type string
	// Prerelease reports whether the version is a pre-release.
	Prerelease bool
	// WatchPrerelease is the watch policy's prerelease mode ("none", "only"
	// or "include"); empty means unknown and is not checked.
	WatchPrerelease string
	// CooldownStart is the instant the watch policy's cooldown counts from.
	// The #106 match step must fill it from
	// Release.CooldownFrom(policy.TrustsPublishedAt(kind)). Zero (unknown)
	// fails closed: Evaluate counts from now, holding for the full
	// subscription cooldown.
	CooldownStart time.Time
	// WatchCooldown is the cooldown the watch policy applied.
	WatchCooldown time.Duration
}

// modeAdmits reports whether a prerelease mode lets a version through.
func modeAdmits(mode string, prerelease bool) bool {
	switch mode {
	case PrereleaseNone:
		return !prerelease
	case PrereleaseOnly:
		return prerelease
	}
	return true
}

// Evaluate decides what a subscription with policy p does with an event at
// time now: Drop when the pre-release mode of the watch or of the
// subscription excludes it, Hold until CooldownStart + max(WatchCooldown,
// p.Cooldown) when that is still ahead, Admit otherwise. The result is
// always a subset of what the watch admitted: the watch's mode is applied
// first, a cooldown shorter than the watch's changes nothing, and a yank
// notice ignores p. It is pure; the match step of #106 calls it once per
// matching subscription.
func (p Policy) Evaluate(ev Event, now time.Time) Decision {
	if ev.Type == eventYanked {
		return Decision{Verdict: Admit}
	}
	if !modeAdmits(ev.WatchPrerelease, ev.Prerelease) || !modeAdmits(p.Prerelease, ev.Prerelease) {
		return Decision{Verdict: Drop}
	}
	p = p.Clamp()
	if p.Cooldown <= ev.WatchCooldown {
		return Decision{Verdict: Admit}
	}
	start := ev.CooldownStart
	if start.IsZero() {
		start = now
	}
	if until := start.Add(p.Cooldown); until.After(now) {
		return Decision{Verdict: Hold, Until: until}
	}
	return Decision{Verdict: Admit}
}
