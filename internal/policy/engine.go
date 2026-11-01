package policy

import (
	"cmp"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/angelia-dev/angelia/internal/diff"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/version"
)

// Release is a version a watch emits (or holds during its cooldown), with
// what the event mapping (#106) needs. Held releases are stored as JSON in
// store.PendingRelease.Release: they keep the classification of the check
// that detected them.
type Release struct {
	Raw        string `json:"raw"`
	Normalized string `json:"normalized"`
	// Scheme names the scheme that classified the version.
	Scheme string `json:"scheme"`
	// Previous and Bump are diff.Change's; both are empty for a version
	// with nothing to compare with.
	Previous   string       `json:"previous,omitempty"`
	Bump       version.Bump `json:"bump,omitempty"`
	Prerelease bool         `json:"prerelease"`
	Backport   bool         `json:"backport,omitempty"`
	// PublishedAt is the upstream date, zero when the source gives none.
	PublishedAt time.Time `json:"published_at,omitzero"`
	// DetectedAt is the time of the check that first saw the version.
	DetectedAt time.Time      `json:"detected_at"`
	URL        string         `json:"url,omitempty"`
	NotesURL   string         `json:"notes_url,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	// CooldownSatisfiedAt is when the version became older than the
	// cooldown (see Release.CooldownFrom); zero without a cooldown. Set on
	// emitted releases only.
	CooldownSatisfiedAt time.Time `json:"cooldown_satisfied_at,omitzero"`
}

// CooldownFrom is the time a cooldown counts from: PublishedAt when the
// source gives it and it is not after DetectedAt, otherwise DetectedAt. An
// upstream date in the future cannot hold a version longer than if it had
// no date.
func (r Release) CooldownFrom() time.Time {
	if !r.PublishedAt.IsZero() && r.PublishedAt.Before(r.DetectedAt) {
		return r.PublishedAt
	}
	return r.DetectedAt
}

// Reason says why a version was dropped.
type Reason string

// Reasons, in evaluation order (docs/policy.md).
const (
	ReasonYanked     Reason = "yanked"     // yanked when detected, or while held
	ReasonAlias      Reason = "alias"      // same precedence as its Previous ("v1.2.0" after "1.2.0")
	ReasonInclude    Reason = "include"    // matches no include pattern
	ReasonExclude    Reason = "exclude"    // matches an exclude pattern
	ReasonPrerelease Reason = "prerelease" // ruled out by the pre-release mode
	ReasonConstraint Reason = "constraint" // outside the constraint
	ReasonBump       Reason = "bump"       // bump not listed
	ReasonCorrupt    Reason = "corrupt"    // a held row that does not decode
)

// Drop is a version the policy dropped.
type Drop struct {
	Raw    string
	Reason Reason
	// Held is true for a version that was held for its cooldown.
	Held bool
}

// Decision is the outcome of one evaluation for one watch.
type Decision struct {
	// Emit are the releases to emit: formerly held ones (by detection time,
	// then version order), then new ones (in the order of the changes).
	Emit []Release
	// Hold are new releases to hold until their cooldown ends.
	Hold []Release
	// Unhold are the raw strings of held versions to forget: emitted or
	// dropped.
	Unhold []string
	// Dropped lists every dropped version, with the reason.
	Dropped []Drop
}

// Engine applies one watch's policy. It is immutable and safe for
// concurrent use.
type Engine struct {
	policy           Policy
	scheme           version.Scheme
	include, exclude []*regexp.Regexp
	rng              Range
	bump             map[version.Bump]bool
}

// Compile validates p and binds it to the watch's scheme s, which parses
// the constraint operands. s is normally schemes.Select(kind, p.Scheme, …).
func Compile(p Policy, s version.Scheme) (*Engine, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	e := &Engine{policy: p, scheme: s}
	for _, pat := range p.Include {
		e.include = append(e.include, regexp.MustCompile(pat)) // Validate compiled it
	}
	for _, pat := range p.Exclude {
		e.exclude = append(e.exclude, regexp.MustCompile(pat))
	}
	c, _ := ParseConstraint(p.Constraint) // Validate parsed it
	rng, err := c.Bind(s)
	if err != nil {
		return nil, fmt.Errorf("constraint: %w", err)
	}
	e.rng = rng
	if len(p.Bump) > 0 {
		e.bump = make(map[version.Bump]bool, len(p.Bump))
		for _, b := range p.Bump {
			e.bump[b] = true
		}
	}
	return e, nil
}

// Policy returns the compiled policy.
func (e *Engine) Policy() Policy { return e.policy }

// filter applies the filters, in order, to a version that is neither
// yanked nor an alias. It returns "" when the version passes.
func (e *Engine) filter(raw string, v version.Version, prerelease bool, bump version.Bump) Reason {
	if len(e.include) > 0 && !slices.ContainsFunc(e.include, func(re *regexp.Regexp) bool { return re.MatchString(raw) }) {
		return ReasonInclude
	}
	if slices.ContainsFunc(e.exclude, func(re *regexp.Regexp) bool { return re.MatchString(raw) }) {
		return ReasonExclude
	}
	switch e.policy.Prerelease {
	case PrereleaseNone:
		if prerelease {
			return ReasonPrerelease
		}
	case PrereleaseOnly:
		if !prerelease {
			return ReasonPrerelease
		}
	}
	if !e.rng.Match(v) {
		return ReasonConstraint
	}
	if e.bump != nil && !e.bump[bump] {
		return ReasonBump
	}
	return ""
}

// isAlias reports whether c has the same precedence as its Previous (diff
// sets Previous to an equal version when there is one, ADR 0010 §8).
func (e *Engine) isAlias(c diff.Change) bool {
	if c.Previous == "" || c.Bump != version.BumpOther || c.Version == nil {
		return false
	}
	prev, err := e.scheme.Parse(c.Previous)
	return err == nil && e.scheme.Compare(prev, c.Version) == 0
}

// due returns when r's cooldown ends.
func (e *Engine) due(r Release) time.Time {
	return r.CooldownFrom().Add(time.Duration(e.policy.Cooldown))
}

// emit stamps r for emission at now, or reports false while it is cooling down.
func (e *Engine) emit(r Release, now time.Time) (Release, bool) {
	if e.policy.Cooldown == 0 {
		return r, true
	}
	due := e.due(r)
	if due.After(now) {
		return r, false
	}
	r.CooldownSatisfiedAt = due
	return r, true
}

// Evaluate decides, at now, what the watch emits. changes are the watch's
// changes from diff.Apply (diff.WatchChanges.Changes, classified with the
// engine's scheme); only diff.KindNew changes are candidates. held are the
// versions the watch holds (store.Tx.ListPendingReleases, this watch's rows
// only): each is re-checked on every evaluation with the current policy and
// the version's current state:
//
//   - yanked: dropped, it never emits (even if un-yanked later);
//   - no longer listed upstream (removed): kept, not emitted;
//   - filtered out by the current policy: dropped;
//   - cooldown over (from CooldownFrom plus the current cooldown): emitted.
//
// A new version is dropped when it is yanked, an alias of its Previous or
// filtered out; otherwise it is emitted, or held while its cooldown runs.
// Evaluate is pure.
func (e *Engine) Evaluate(changes []diff.Change, held []store.PendingRelease, now time.Time) Decision {
	var d Decision
	var fromHeld []Release
	for _, p := range held {
		r, reason := e.recheck(p)
		if reason == "" && p.Removed {
			continue // keep holding: it may be listed again
		}
		if reason == "" {
			var ok bool
			if r, ok = e.emit(r, now); !ok {
				continue
			}
			fromHeld = append(fromHeld, r)
		} else {
			d.Dropped = append(d.Dropped, Drop{Raw: p.Raw, Reason: reason, Held: true})
		}
		d.Unhold = append(d.Unhold, p.Raw)
	}
	slices.SortStableFunc(fromHeld, func(a, b Release) int {
		if c := a.DetectedAt.Compare(b.DetectedAt); c != 0 {
			return c
		}
		return e.compareRaw(a.Raw, b.Raw)
	})
	d.Emit = fromHeld

	for _, c := range changes {
		if c.Kind != diff.KindNew {
			continue
		}
		reason := Reason("")
		switch {
		case c.Yanked:
			reason = ReasonYanked
		case e.isAlias(c):
			reason = ReasonAlias
		default:
			reason = e.filter(c.Raw, c.Version, c.Prerelease, c.Bump)
		}
		if reason != "" {
			d.Dropped = append(d.Dropped, Drop{Raw: c.Raw, Reason: reason})
			continue
		}
		r, ok := e.emit(e.release(c, now), now)
		if ok {
			d.Emit = append(d.Emit, r)
		} else {
			d.Hold = append(d.Hold, r)
		}
	}
	return d
}

// recheck decodes a held row and applies the yank state and the current
// filters. It returns "" when the version may still be emitted.
func (e *Engine) recheck(p store.PendingRelease) (Release, Reason) {
	var r Release
	if err := json.Unmarshal(p.Release, &r); err != nil || r.Raw != p.Raw {
		return Release{}, ReasonCorrupt
	}
	if p.Yanked {
		return r, ReasonYanked
	}
	v, err := e.scheme.Parse(r.Raw)
	if err != nil {
		v = nil // never matches a constraint
	}
	return r, e.filter(r.Raw, v, r.Prerelease, r.Bump)
}

func (e *Engine) compareRaw(a, b string) int {
	va, errA := e.scheme.Parse(a)
	vb, errB := e.scheme.Parse(b)
	if errA != nil || errB != nil {
		return cmp.Compare(a, b)
	}
	return e.scheme.Compare(va, vb)
}

// release snapshots a new version detected at now.
func (e *Engine) release(c diff.Change, now time.Time) Release {
	r := Release{
		Raw: c.Raw, Scheme: e.scheme.Name(), Previous: c.Previous, Bump: c.Bump,
		Prerelease: c.Prerelease, Backport: c.Backport, DetectedAt: now.UTC(),
		URL: c.Upstream.URL, NotesURL: c.Upstream.NotesURL, Metadata: c.Upstream.Metadata,
	}
	if c.Version != nil {
		r.Normalized = c.Version.String()
	}
	if !c.Upstream.PublishedAt.IsZero() {
		r.PublishedAt = c.Upstream.PublishedAt.UTC()
	}
	return r
}
