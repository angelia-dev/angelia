package diff

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	// Scheme orders and classifies the versions, normally
	// schemes.Select(kind, override, logger).
	Scheme version.Scheme
	// Now stamps FirstSeenAt and RemovedAt.
	Now time.Time
	// Logger receives removals, returns and the aggregated warnings about
	// unparseable or skipped versions. nil discards them.
	Logger *slog.Logger
}

// Outcome is what Apply wrote and what each watch should hear about.
type Outcome struct {
	Result
	// Watches lists every watch on the target, ordered by ID.
	Watches []WatchChanges
}

// WatchChanges is what one watch gets from a check.
type WatchChanges struct {
	// Watch is the watch as it was before the check.
	Watch store.Watch
	// Baseline is true when this check was the watch's first: the known set
	// was recorded and Changes is empty.
	Baseline bool
	// Changes are the changes whose Kind.Notifies(), in Result order.
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
	if !in.Fetched.NotModified {
		known, err := tx.ListVersions(ctx, in.TargetID)
		if err != nil {
			return Outcome{}, fmt.Errorf("diff: %w", err)
		}
		out.Result = Compute(in.TargetID, known, in.Fetched.Versions, in.Scheme, in.Now)
		if len(in.Fetched.Versions) == 0 && len(known) > 0 {
			logger.WarnContext(ctx, "upstream listed no versions; known set kept", slog.Int("known", len(known)))
		}
	}
	for _, v := range out.Add {
		if _, err := tx.AddVersion(ctx, v); err != nil {
			return Outcome{}, fmt.Errorf("diff: %w", err)
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
	var notify []Change
	for _, c := range out.Changes {
		if c.Kind.Notifies() {
			notify = append(notify, c)
		}
	}
	for _, w := range watches {
		wc := WatchChanges{Watch: w}
		if w.BaselineDone {
			wc.Changes = notify
		} else {
			if err := tx.MarkBaselineDone(ctx, w.ID); err != nil {
				return Outcome{}, fmt.Errorf("diff: %w", err)
			}
			wc.Baseline = true
			logger.InfoContext(ctx, "baseline recorded", slog.String("watch", w.ID), slog.Int("skipped_changes", len(notify)))
		}
		out.Watches = append(out.Watches, wc)
	}

	logResult(ctx, logger, in.Scheme.Name(), out.Result)
	return out, nil
}

// logResult logs removals and returns (they emit nothing in Phase 0) and one
// aggregated warning for lexical fallbacks and one for skipped entries, as
// ADR 0007 asks of the fetch loop.
func logResult(ctx context.Context, logger *slog.Logger, scheme string, r Result) {
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
	if len(r.Fallbacks) > 0 {
		logger.WarnContext(ctx, "new versions do not match the scheme, using lexical order",
			slog.String("scheme", scheme), slog.Int("count", len(r.Fallbacks)), slog.Any("versions", sample(r.Fallbacks)))
	}
	if r.Skipped > 0 {
		logger.WarnContext(ctx, "skipped blank or duplicate upstream versions", slog.Int("count", r.Skipped))
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
