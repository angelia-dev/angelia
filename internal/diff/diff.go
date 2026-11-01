// Package diff compares a target's known version set with the versions a
// source just fetched and reports what changed (architecture.md §3.4): new
// versions, including backports such as 1.9.4 released after 2.0.0, yank and
// un-yank transitions, and versions that stopped being listed upstream.
//
// [Compute] is pure: it takes the known set and the fetched list and returns
// the changes and the rows to write. [Apply] runs one check inside the
// store's unit of work ([store.Tx]): it reads the known set and the target's
// watches, writes the known set, marks first-run baselines, and returns what
// each watch should hear about. The caller (fetch loop) commits it in the
// same transaction as the events and the target's fetch state.
//
// Rules:
//   - The known set is keyed by the raw upstream string (ADR 0007): "1.0" and
//     "1.0.0" are two versions even when they compare equal.
//   - The known set never shrinks. A version missing from a non-empty fetch
//     is marked removed (RemovedAt) and reported once as [KindRemoved]; it
//     emits nothing in Phase 0. If it is listed again it is reported as
//     [KindReturned], never as new.
//   - An empty fetch (no versions, NotModified false) changes nothing: it is
//     far more often a registry glitch than every version being deleted.
//   - Baseline: the first successful check of a watch records the known set
//     and marks the watch's baseline done, but gives the watch no changes.
//   - Running the same check twice gives no changes the second time.
package diff

import (
	"slices"
	"strings"
	"time"

	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/version"
	"github.com/angelia-dev/angelia/internal/version/lexical"
)

// Kind is the kind of a change.
type Kind string

// Change kinds, in the order Compute reports them.
const (
	// KindNew is a version that is not in the known set.
	KindNew Kind = "new"
	// KindYanked is a known version that upstream now marks yanked.
	KindYanked Kind = "yanked"
	// KindUnyanked is a known yanked version that upstream no longer marks yanked.
	KindUnyanked Kind = "unyanked"
	// KindRemoved is a known version that upstream stopped listing. Logged only.
	KindRemoved Kind = "removed"
	// KindReturned is a removed version that upstream lists again. Logged only.
	KindReturned Kind = "returned"
)

var kindOrder = map[Kind]int{KindNew: 0, KindYanked: 1, KindUnyanked: 2, KindRemoved: 3, KindReturned: 4}

// Notifies reports whether changes of kind k are passed on to watches (and
// so may become events). Removals and returns are only logged in Phase 0.
func (k Kind) Notifies() bool { return k == KindNew || k == KindYanked || k == KindUnyanked }

// Change is one difference between the known set and the fetched versions.
type Change struct {
	Kind Kind
	// Raw is the upstream version string, the key of the known set.
	Raw string
	// Version is Raw parsed with the watch's scheme (possibly the lexical
	// fallback).
	Version version.Version
	// Upstream is the fetched entry. It is the zero value for KindRemoved.
	Upstream source.Version
	// Prerelease is the upstream flag when the source gives one, otherwise
	// the scheme's opinion. For known versions it is the recorded value.
	Prerelease bool
	// Yanked is the version's yank state after this check.
	Yanked bool

	// The fields below are set for KindNew only.

	// Previous is the raw string of the greatest version strictly below
	// Version among the known and new versions (ADR 0007), or "" when there
	// is none.
	Previous string
	// Bump classifies Previous → Version; "" when Previous is "".
	Bump version.Bump
	// Backport is true when a version known before this check is greater
	// than Version (1.9.4 released after 2.0.0).
	Backport bool
}

// Result is the outcome of Compute.
type Result struct {
	// Changes, grouped by kind (new, yanked, unyanked, removed, returned)
	// and in ascending version order within a kind.
	Changes []Change
	// Add are the new rows of the known set, in the order of Changes.
	Add []store.Version
	// Update are the known rows whose Yanked or RemovedAt changed: listed
	// ones in fetch order, then removed ones in known-set order.
	Update []store.Version
	// Fallbacks are the raw strings of new versions that the scheme could
	// not parse and that were ordered lexically instead.
	Fallbacks []string
	// Skipped counts fetched entries that were ignored: blank or duplicate
	// raw strings.
	Skipped int
}

// IsZero reports whether r has no changes and nothing to write.
func (r Result) IsZero() bool {
	return len(r.Changes) == 0 && len(r.Add) == 0 && len(r.Update) == 0
}

// entry is a parsed version of the union of the known and new sets.
type entry struct {
	raw string
	v   version.Version
}

// Compute compares known, the known set of targetID, with fetched, the
// versions a source listed, using scheme (which should be wrapped with
// schemes.WithFallback so that odd tags still parse). now stamps
// FirstSeenAt and RemovedAt. It does no I/O and does not modify its inputs.
func Compute(targetID string, known []store.Version, fetched []source.Version, scheme version.Scheme, now time.Time) Result {
	var r Result
	byRaw := make(map[string]store.Version, len(known))
	for _, k := range known {
		byRaw[k.Raw] = k
	}

	var all []entry
	var maxKnown version.Version
	for _, k := range known {
		v, err := scheme.Parse(k.Raw)
		if err != nil {
			continue // only a blank string fails, and the store never holds one from us
		}
		all = append(all, entry{raw: k.Raw, v: v})
		if maxKnown == nil || scheme.Compare(v, maxKnown) > 0 {
			maxKnown = v
		}
	}

	seen := make(map[string]bool, len(fetched))
	var news []int // indexes into r.Changes
	for _, f := range fetched {
		if strings.TrimSpace(f.Raw) == "" || seen[f.Raw] {
			r.Skipped++
			continue
		}
		if k, ok := byRaw[f.Raw]; ok {
			seen[f.Raw] = true
			r.knownListed(k, f, scheme)
			continue
		}
		v, err := scheme.Parse(f.Raw)
		if err != nil {
			r.Skipped++
			continue
		}
		seen[f.Raw] = true
		if v.Scheme() == lexical.Name && scheme.Name() != lexical.Name {
			r.Fallbacks = append(r.Fallbacks, f.Raw)
		}
		pre := scheme.IsPrerelease(v)
		if f.Prerelease != nil {
			pre = *f.Prerelease
		}
		news = append(news, len(r.Changes))
		r.Changes = append(r.Changes, Change{
			Kind: KindNew, Raw: f.Raw, Version: v, Upstream: f, Prerelease: pre, Yanked: f.Yanked,
			Backport: maxKnown != nil && scheme.Compare(maxKnown, v) > 0,
		})
		all = append(all, entry{raw: f.Raw, v: v})
	}

	if len(seen) > 0 { // an empty fetch removes nothing
		for _, k := range known {
			if seen[k.Raw] || !k.RemovedAt.IsZero() {
				continue
			}
			v, err := scheme.Parse(k.Raw)
			if err != nil {
				continue
			}
			r.Changes = append(r.Changes, Change{Kind: KindRemoved, Raw: k.Raw, Version: v, Prerelease: k.Prerelease, Yanked: k.Yanked})
			k.RemovedAt = now
			r.Update = append(r.Update, k)
		}
	}

	if len(news) > 0 {
		setPrevious(r.Changes, news, all, scheme)
	}
	slices.SortStableFunc(r.Changes, func(a, b Change) int {
		if c := kindOrder[a.Kind] - kindOrder[b.Kind]; c != 0 {
			return c
		}
		return compareEntries(scheme, a.Version, b.Version, a.Raw, b.Raw)
	})
	for _, c := range r.Changes {
		if c.Kind != KindNew {
			continue
		}
		r.Add = append(r.Add, store.Version{
			TargetID: targetID, Raw: c.Raw, Normalized: c.Version.String(), PublishedAt: c.Upstream.PublishedAt.UTC(),
			Prerelease: c.Prerelease, Yanked: c.Yanked, FirstSeenAt: now,
		})
	}
	return r
}

// knownListed records the changes of a known version k that upstream lists as f.
func (r *Result) knownListed(k store.Version, f source.Version, scheme version.Scheme) {
	v, err := scheme.Parse(k.Raw)
	if err != nil {
		return
	}
	upd, dirty := k, false
	if !k.RemovedAt.IsZero() {
		upd.RemovedAt, dirty = time.Time{}, true
		r.Changes = append(r.Changes, Change{Kind: KindReturned, Raw: k.Raw, Version: v, Upstream: f, Prerelease: k.Prerelease, Yanked: f.Yanked})
	}
	if k.Yanked != f.Yanked {
		upd.Yanked, dirty = f.Yanked, true
		kind := KindYanked
		if !f.Yanked {
			kind = KindUnyanked
		}
		r.Changes = append(r.Changes, Change{Kind: kind, Raw: k.Raw, Version: v, Upstream: f, Prerelease: k.Prerelease, Yanked: f.Yanked})
	}
	if dirty {
		r.Update = append(r.Update, upd)
	}
}

// setPrevious fills Previous and Bump of the new changes at idx: the
// greatest entry of all strictly below each one (ADR 0007: the caller of
// Bump passes the greatest known version below next, not the latest).
func setPrevious(changes []Change, idx []int, all []entry, scheme version.Scheme) {
	slices.SortStableFunc(all, func(a, b entry) int { return compareEntries(scheme, a.v, b.v, a.raw, b.raw) })
	pos := make(map[string]int, len(all))
	for i, e := range all {
		pos[e.raw] = i
	}
	for _, ci := range idx {
		c := &changes[ci]
		for j := pos[c.Raw] - 1; j >= 0; j-- {
			if scheme.Compare(all[j].v, c.Version) < 0 {
				c.Previous = all[j].raw
				c.Bump = scheme.Bump(all[j].v, c.Version)
				break
			}
		}
	}
}

// compareEntries orders by scheme precedence, then by raw string so that
// versions of equal precedence ("1.0", "1.0.0") have a stable order.
func compareEntries(scheme version.Scheme, a, b version.Version, rawA, rawB string) int {
	if c := scheme.Compare(a, b); c != 0 {
		return c
	}
	return strings.Compare(rawA, rawB)
}
