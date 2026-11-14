// Package diff compares a target's known version set with the versions a
// source just fetched and reports what changed (architecture.md §3.4): new
// versions, including backports such as 1.9.4 released after 2.0.0, yank and
// un-yank transitions, and versions that stopped being listed upstream.
//
// [Compute] is pure: it takes the known set and the fetched list and returns
// the changes and the rows to write. [Result.Classify] classifies the same
// changes under another scheme, because the scheme is chosen per watch while
// the known set belongs to the target. [Apply] runs one check inside the
// store's unit of work ([store.Tx]): it reads the known set and the target's
// watches, writes the known set, marks first-run baselines, and returns what
// each watch should hear about. The caller (fetch loop) commits it in the
// same transaction as the events and the target's fetch state.
//
// Rules (ADR 0010):
//   - The known set is keyed by the raw upstream string (ADR 0007): "1.0" and
//     "1.0.0" are two versions even when they compare equal. It is
//     scheme-neutral: Normalized and Prerelease are written with the source
//     kind's default scheme, never with a watch's override.
//   - Raw strings are used as the source gives them. Blank strings, strings
//     with leading or trailing white space and repeated strings are skipped
//     (normalisation is the source's job).
//   - The known set never shrinks. A version missing from a fetch with at
//     least one usable entry is marked removed (RemovedAt) and reported once
//     as [KindRemoved]; it emits nothing in Phase 0. If it is listed again
//     it is reported as [KindReturned], never as new. This assumes
//     Source.List returns the complete list.
//   - A fetch without usable entries changes nothing: it is far more often a
//     registry glitch than every version being deleted.
//   - Running the same check twice gives no changes the second time.
package diff

import (
	"slices"
	"strings"
	"time"
	"unicode/utf8"

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
	// Version is Raw parsed with the classifying scheme, or with the
	// lexical scheme when that scheme rejects it.
	Version version.Version
	// Upstream is the fetched entry. It is the zero value for KindRemoved.
	Upstream source.Version
	// Prerelease is, for KindNew, the upstream flag when the source gives
	// one, otherwise the classifying scheme's opinion. For known versions it
	// is the recorded value.
	Prerelease bool
	// Yanked is the version's yank state after this check.
	Yanked bool

	// The fields below are set for KindNew only, by the classifying scheme.

	// Previous is the raw string of the version this one is compared with,
	// or "" when there is none (ADR 0007 and ADR 0010):
	//   - a known version of equal precedence ("1.2.0" for "v1.2.0"), or an
	//     earlier entry of the same fetch with equal precedence; then Bump is
	//     "other";
	//   - otherwise the greatest version strictly below Version among the
	//     known and new versions parsed by the same scheme as Version.
	Previous string
	// Bump classifies Previous → Version; "" when Previous is "".
	Bump version.Bump
	// Backport is true when a known, not removed version is greater than
	// Version, both parsed by the classifying scheme (1.9.4 released after
	// 2.0.0).
	Backport bool

	order int // position of a new version in the fetch, for equal precedence
}

// Result is the outcome of Compute.
type Result struct {
	// Changes, classified with the default scheme, grouped by kind (new,
	// yanked, unyanked, removed, returned) and in ascending version order
	// within a kind.
	Changes []Change
	// Add are the new rows of the known set, in the order of Changes.
	Add []store.Version
	// Update are the known rows whose Yanked or RemovedAt changed: listed
	// ones in fetch order, then removed ones in known-set order.
	Update []store.Version
	// Fallbacks are the raw strings of new versions that the default scheme
	// could not parse and that were ordered lexically instead.
	Fallbacks []string
	// Usable counts the fetched entries that were compared; Skipped those
	// that were ignored (blank, surrounded by white space, or repeated).
	Usable, Skipped int

	known []store.Version // the known set before the check, for Classify
}

// IsZero reports whether r has no changes and nothing to write.
func (r Result) IsZero() bool {
	return len(r.Changes) == 0 && len(r.Add) == 0 && len(r.Update) == 0
}

// Compute compares known, the known set of targetID, with fetched, the
// versions a source listed. def is the source kind's default scheme: it
// classifies Changes and fills Normalized and Prerelease of the new rows.
// now stamps FirstSeenAt and RemovedAt. Compute does no I/O and does not
// modify its inputs.
func Compute(targetID string, known []store.Version, fetched []source.Version, def version.Scheme, now time.Time) Result {
	r := detect(known, fetched, now)
	r.Changes, r.Fallbacks = r.Classify(def)
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

// detect finds the changes by raw string, without any scheme.
func detect(known []store.Version, fetched []source.Version, now time.Time) Result {
	r := Result{known: known}
	byRaw := make(map[string]store.Version, len(known))
	for _, k := range known {
		byRaw[k.Raw] = k
	}
	seen := make(map[string]bool, len(fetched))
	for i, f := range fetched {
		// The first entry of a raw string wins: sources list newest first,
		// so a repeated entry, even one with a different yank flag, is
		// treated as a stale copy and skipped.
		// A raw string that is not valid UTF-8 or holds a NUL cannot be
		// stored as text on Postgres (and is malformed anyway): skipped on
		// every engine, like a blank or padded one.
		if f.Raw == "" || f.Raw != strings.TrimSpace(f.Raw) || !utf8.ValidString(f.Raw) ||
			strings.ContainsRune(f.Raw, 0) || seen[f.Raw] {
			r.Skipped++
			continue
		}
		seen[f.Raw] = true
		if k, ok := byRaw[f.Raw]; ok {
			r.knownListed(k, f)
			continue
		}
		r.Changes = append(r.Changes, Change{Kind: KindNew, Raw: f.Raw, Upstream: f, Yanked: f.Yanked, order: i})
	}
	r.Usable = len(seen)
	if r.Usable == 0 { // no usable entry: remove nothing
		return r
	}
	for _, k := range known {
		if seen[k.Raw] || !k.RemovedAt.IsZero() {
			continue
		}
		r.Changes = append(r.Changes, Change{Kind: KindRemoved, Raw: k.Raw, Prerelease: k.Prerelease, Yanked: k.Yanked})
		k.RemovedAt = now
		r.Update = append(r.Update, k)
	}
	return r
}

// knownListed records the changes of a known version k that upstream lists as f.
func (r *Result) knownListed(k store.Version, f source.Version) {
	upd, dirty := k, false
	if !k.RemovedAt.IsZero() {
		upd.RemovedAt, dirty = time.Time{}, true
		r.Changes = append(r.Changes, Change{Kind: KindReturned, Raw: k.Raw, Upstream: f, Prerelease: k.Prerelease, Yanked: f.Yanked})
	}
	if k.Yanked != f.Yanked {
		upd.Yanked, dirty = f.Yanked, true
		kind := KindYanked
		if !f.Yanked {
			kind = KindUnyanked
		}
		r.Changes = append(r.Changes, Change{Kind: kind, Raw: k.Raw, Upstream: f, Prerelease: k.Prerelease, Yanked: f.Yanked})
	}
	if dirty {
		r.Update = append(r.Update, upd)
	}
}

// entry is a parsed version of the union of the known and new sets.
type entry struct {
	raw     string
	v       version.Version
	known   bool
	removed bool
	order   int // fetch position of a new version
}

// Classify returns the changes of r classified with scheme: every change's
// Version, and for new versions Prerelease, Previous, Bump and Backport, in
// scheme order. It also returns the new versions that scheme parsed with
// the lexical fallback. Removed, yanked, unyanked and returned changes are
// the same for every scheme. r is not modified.
func (r Result) Classify(scheme version.Scheme) ([]Change, []string) {
	primary := func(v version.Version) bool { return v.Scheme() == scheme.Name() }
	all := make([]entry, 0, len(r.known)+len(r.Changes))
	var maxKnown version.Version
	for _, k := range r.known {
		v := parse(scheme, k.Raw)
		e := entry{raw: k.Raw, v: v, known: true, removed: !k.RemovedAt.IsZero()}
		all = append(all, e)
		if !e.removed && primary(v) && (maxKnown == nil || scheme.Compare(v, maxKnown) > 0) {
			maxKnown = v
		}
	}

	out := slices.Clone(r.Changes)
	var fallbacks []string
	var news []int
	for i := range out {
		c := &out[i]
		c.Version = parse(scheme, c.Raw)
		if c.Kind != KindNew {
			continue
		}
		if !primary(c.Version) && scheme.Name() != lexical.Name {
			fallbacks = append(fallbacks, c.Raw)
		}
		c.Prerelease = scheme.IsPrerelease(c.Version)
		if c.Upstream.Prerelease != nil {
			c.Prerelease = *c.Upstream.Prerelease
		}
		c.Backport = maxKnown != nil && primary(c.Version) && scheme.Compare(maxKnown, c.Version) > 0
		c.Previous, c.Bump = "", ""
		news = append(news, i)
		all = append(all, entry{raw: c.Raw, v: c.Version, order: c.order})
	}
	if len(news) > 0 {
		setPrevious(out, news, all, scheme)
	}
	slices.SortStableFunc(out, func(a, b Change) int {
		if c := kindOrder[a.Kind] - kindOrder[b.Kind]; c != 0 {
			return c
		}
		return compareEntries(scheme, a.Version, b.Version, a.Raw, b.Raw)
	})
	return out, fallbacks
}

// parse parses raw with scheme, falling back to the lexical scheme so that a
// scheme without fallback still orders every non-blank string.
func parse(scheme version.Scheme, raw string) version.Version {
	if v, err := scheme.Parse(raw); err == nil {
		return v
	}
	v, _ := lexical.Parse(raw) // raw is never blank here
	return v
}

// setPrevious fills Previous and Bump of the new changes at idx (see
// Change.Previous).
func setPrevious(changes []Change, idx []int, all []entry, scheme version.Scheme) {
	slices.SortStableFunc(all, func(a, b entry) int { return compareEntries(scheme, a.v, b.v, a.raw, b.raw) })
	pos := make(map[string]int, len(all))
	for i, e := range all {
		pos[e.raw] = i
	}
	same := func(e entry, v version.Version) bool { return e.v.Scheme() == v.Scheme() }
	for _, ci := range idx {
		c := &changes[ci]
		i := pos[c.Raw]
		me := all[i]
		lo, hi := i, i
		for lo > 0 && same(all[lo-1], me.v) && scheme.Compare(all[lo-1].v, me.v) == 0 {
			lo--
		}
		for hi < len(all)-1 && same(all[hi+1], me.v) && scheme.Compare(all[hi+1].v, me.v) == 0 {
			hi++
		}
		// An alias: a known entry of equal precedence, else the earliest
		// fetched one before this one.
		alias := -1
		for j := lo; j <= hi; j++ {
			e := all[j]
			switch {
			case j == i || (!e.known && e.order > me.order):
			case alias < 0,
				e.known && !all[alias].known,
				!e.known && !all[alias].known && e.order < all[alias].order:
				alias = j
			}
		}
		if alias < 0 {
			for j := lo - 1; j >= 0; j-- {
				if same(all[j], me.v) {
					alias = j
					break
				}
			}
		}
		if alias >= 0 {
			c.Previous = all[alias].raw
			c.Bump = scheme.Bump(all[alias].v, c.Version)
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
