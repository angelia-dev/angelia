package diff

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/version"
)

// maxLogged bounds how many raw strings one aggregated log record lists, and
// maxLoggedRaw how many bytes of each.
const (
	maxLogged    = 5
	maxLoggedRaw = 128
)

// Input is one successful fetch of a target.
type Input struct {
	TargetID string
	// Fetched is what the source returned. NotModified means the known set
	// is current: nothing is compared, but baselines are still marked.
	Fetched source.Result
	// Scheme is the source kind's default scheme, normally
	// schemes.Select(kind, "", logger). It fills Normalized and Prerelease
	// of new rows, so the known set never depends on a watch's override.
	Scheme version.Scheme
	// SchemeFor returns the scheme of a watch, normally
	// schemes.Select(kind, override, logger). nil means every watch uses
	// Scheme. Within one target, schemes are told apart by Name (Select
	// gives one configuration per name and source kind).
	SchemeFor func(store.Watch) (version.Scheme, error)
	// Now stamps FirstSeenAt and RemovedAt.
	Now time.Time
	// Logger receives removals, returns, baselines and the aggregated
	// warnings about unparseable or skipped versions. nil discards them.
	Logger *slog.Logger
}

// Outcome is what Apply wrote and what each watch should hear about.
type Outcome struct {
	// Result is the diff classified with the default scheme.
	Result
	// Watches lists every watch on the target, ordered by ID.
	Watches []WatchChanges
}

// WatchChanges is what one watch gets from a check.
type WatchChanges struct {
	// Watch is the watch as it was before the check.
	Watch store.Watch
	// Scheme is the name of the scheme that classified Changes; "" for a
	// baseline.
	Scheme string
	// Baseline is true when this check recorded a baseline for the watch
	// (its first check, or the first non-empty fill of an empty known set):
	// Changes is empty.
	Baseline bool
	// Changes are the changes whose Kind.Notifies(), classified with the
	// watch's scheme, in that scheme's order. Each watch has its own slice.
	Changes []Change
}

// errNoScheme is returned by Apply when the input has no scheme.
var errNoScheme = errors.New("diff: no version scheme")

// Apply runs one check of in.TargetID inside tx: it reads the known set and
// the watches, computes the changes, writes new and updated versions, and
// marks the baseline of every watch that had none. It returns the per-watch
// changes for the policy engine; the caller writes events, deliveries and
// the fetch state in the same transaction and commits. On error the caller
// must roll back (store.Store.InTx does when fn returns the error).
//
// When the known set was empty before the check and the fetch adds more
// than one version, the check is a baseline for every watch, even one whose
// baseline is already done: a target first fetched empty (a glitch, or a
// package not published yet) must not replay its whole history later. A
// single first version is reported.
func Apply(ctx context.Context, tx store.Tx, in Input) (Outcome, error) {
	if in.Scheme == nil {
		return Outcome{}, errNoScheme
	}
	logger := in.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	logger = logger.With(slog.String("target", in.TargetID))

	var out Outcome
	baselineAll := false
	if !in.Fetched.NotModified {
		known, err := tx.ListVersions(ctx, in.TargetID)
		if err != nil {
			return Outcome{}, fmt.Errorf("diff: %w", err)
		}
		out.Result = Compute(in.TargetID, known, in.Fetched.Versions, in.Scheme, in.Now)
		if out.Usable == 0 && len(known) > 0 {
			logger.WarnContext(ctx, "upstream listed no usable versions; known set kept", slog.Int("known", len(known)))
		}
		baselineAll = len(known) == 0 && len(out.Add) > 1
	}
	for _, v := range out.Add {
		added, err := tx.AddVersion(ctx, v)
		if err != nil {
			return Outcome{}, fmt.Errorf("diff: %w", err)
		}
		if !added { // a concurrent check of the same target
			return Outcome{}, fmt.Errorf("diff: version %s@%s already known: %w", v.TargetID, v.Raw, store.ErrConflict)
		}
	}
	for _, v := range out.Update {
		if err := tx.UpdateVersion(ctx, v); err != nil {
			return Outcome{}, fmt.Errorf("diff: %w", err)
		}
	}

	watches, err := tx.ListWatches(ctx, in.TargetID)
	if err != nil {
		return Outcome{}, fmt.Errorf("diff: %w", err)
	}
	byScheme := map[string][]Change{} // notifying changes per scheme name
	fallbacks := map[string][]string{in.Scheme.Name(): out.Fallbacks}
	byScheme[in.Scheme.Name()] = notifying(out.Changes)
	for _, w := range watches {
		wc := WatchChanges{Watch: w}
		if !w.BaselineDone || baselineAll {
			if !w.BaselineDone {
				if err := tx.MarkBaselineDone(ctx, w.ID); err != nil {
					return Outcome{}, fmt.Errorf("diff: %w", err)
				}
			}
			wc.Baseline = true
			logger.InfoContext(ctx, "baseline recorded", slog.String("watch", w.ID),
				slog.Bool("first_fill", baselineAll), slog.Int("versions", len(out.Add)))
			out.Watches = append(out.Watches, wc)
			continue
		}
		s := in.Scheme
		if in.SchemeFor != nil {
			if s, err = in.SchemeFor(w); err != nil {
				return Outcome{}, fmt.Errorf("diff: watch %s scheme: %w", w.ID, err)
			}
		}
		cs, ok := byScheme[s.Name()]
		if !ok {
			var fb []string
			cs, fb = out.Classify(s)
			cs = notifying(cs)
			byScheme[s.Name()], fallbacks[s.Name()] = cs, fb
		}
		wc.Scheme, wc.Changes = s.Name(), slices.Clone(cs)
		out.Watches = append(out.Watches, wc)
	}

	logResult(ctx, logger, out.Result, fallbacks)
	return out, nil
}

func notifying(cs []Change) []Change {
	var out []Change
	for _, c := range cs {
		if c.Kind.Notifies() {
			out = append(out, c)
		}
	}
	return out
}

// logResult logs removals and returns (they emit nothing in Phase 0), one
// aggregated warning per scheme for lexical fallbacks and one for skipped
// entries, as ADR 0007 asks of the fetch loop.
func logResult(ctx context.Context, logger *slog.Logger, r Result, fallbacks map[string][]string) {
	var removed, returned []string
	for _, c := range r.Changes {
		switch c.Kind {
		case KindRemoved:
			removed = append(removed, c.Raw)
		case KindReturned:
			returned = append(returned, c.Raw)
		}
	}
	if len(removed) > 0 {
		logger.InfoContext(ctx, "versions no longer listed upstream", slog.Int("count", len(removed)), slog.Any("versions", sample(removed)))
	}
	if len(returned) > 0 {
		logger.InfoContext(ctx, "removed versions listed upstream again", slog.Int("count", len(returned)), slog.Any("versions", sample(returned)))
	}
	names := make([]string, 0, len(fallbacks))
	for name := range fallbacks {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if fb := fallbacks[name]; len(fb) > 0 {
			logger.WarnContext(ctx, "new versions do not match the scheme, using lexical order",
				slog.String("scheme", name), slog.Int("count", len(fb)), slog.Any("versions", sample(fb)))
		}
	}
	if r.Skipped > 0 {
		logger.WarnContext(ctx, "skipped blank, padded or repeated upstream versions", slog.Int("count", r.Skipped))
	}
}

// sample returns at most maxLogged raw strings, each truncated for the log.
func sample(raws []string) []string {
	raws = raws[:min(len(raws), maxLogged)]
	out := make([]string, len(raws))
	for i, raw := range raws {
		out[i] = version.Truncate(raw, maxLoggedRaw)
	}
	return out
}
