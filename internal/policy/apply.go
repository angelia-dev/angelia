package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/angelia-dev/angelia/internal/diff"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/version"
	"github.com/angelia-dev/angelia/internal/version/schemes"
)

// Input is what Apply needs besides the diff outcome.
type Input struct {
	TargetID string
	// SchemeFor returns the scheme of a watch; it must agree with the one
	// given to diff.Apply. Normally SchemeFor(kind, logger).
	SchemeFor func(store.Watch) (version.Scheme, error)
	// Now is the time of the check, the same as diff.Input.Now.
	Now time.Time
	// TrustPublishedAt lets cooldowns count from the upstream publish date
	// (Release.CooldownFrom). Set it to TrustsPublishedAt(target.Kind); the
	// zero value counts from detection, which is always safe.
	TrustPublishedAt bool
	// Logger receives drops (Debug, Info for held versions) and invalid
	// policies (Error). nil discards them.
	Logger *slog.Logger
}

// WatchReleases is what one watch emits from a check.
type WatchReleases struct {
	Watch store.Watch
	// Policy is the watch's decoded policy, as evaluated.
	Policy Policy
	// Releases to emit, in order (see Decision.Emit).
	Releases []Release
}

var errNoSchemeFor = errors.New("policy: no SchemeFor")

// SchemeFor returns a diff.Input.SchemeFor / Input.SchemeFor for watches of
// a source kind: schemes.Select with the `scheme` key of the watch's
// policy_json.
func SchemeFor(kind string, logger *slog.Logger) func(store.Watch) (version.Scheme, error) {
	return func(w store.Watch) (version.Scheme, error) {
		var p struct {
			Scheme string `json:"scheme"`
		}
		if len(w.Policy) > 0 {
			if err := json.Unmarshal(w.Policy, &p); err != nil {
				return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
			}
		}
		return schemes.Select(kind, p.Scheme, logger)
	}
}

// Apply runs every watch's policy on the outcome of diff.Apply, inside the
// same unit of work, after it: it re-checks the versions each watch holds,
// holds new versions whose cooldown runs, forgets the ones it emits or
// drops, and returns what each watch emits. The caller turns the releases
// into events and deliveries in the same transaction (#106).
//
// A watch whose policy is invalid (it should have been rejected when it was
// stored) is skipped with an Error log: it emits nothing from this check and
// its held versions are kept. Store errors are returned; the caller must
// roll back.
func Apply(ctx context.Context, tx store.Tx, out diff.Outcome, in Input) ([]WatchReleases, error) {
	if in.SchemeFor == nil {
		return nil, errNoSchemeFor
	}
	logger := in.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	logger = logger.With(slog.String("target", in.TargetID))

	pending, err := tx.ListPendingReleases(ctx, in.TargetID)
	if err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	held := map[string][]store.PendingRelease{}
	for _, p := range pending {
		held[p.WatchID] = append(held[p.WatchID], p)
	}

	var res []WatchReleases
	for _, wc := range out.Watches {
		w := wc.Watch
		wlog := logger.With(slog.String("watch", w.ID))
		e, err := compileFor(w, in.SchemeFor, in.TrustPublishedAt)
		if err != nil {
			wlog.ErrorContext(ctx, "invalid watch policy; watch skipped for this check",
				slog.String("error", err.Error()), slog.Int("changes", len(wc.Changes)))
			continue
		}
		d := e.Evaluate(wc.Changes, held[w.ID], in.Now)
		for _, raw := range d.Unhold {
			if err := tx.DeletePendingRelease(ctx, w.ID, in.TargetID, raw); err != nil {
				return nil, fmt.Errorf("policy: %w", err)
			}
		}
		for _, r := range d.Hold {
			b, err := json.Marshal(r)
			if err != nil {
				return nil, fmt.Errorf("policy: watch %s: encode %s: %w", w.ID, version.Truncate(r.Raw, 64), err)
			}
			if err := tx.AddPendingRelease(ctx, store.PendingRelease{
				WatchID: w.ID, TargetID: in.TargetID, Raw: r.Raw, HeldAt: in.Now, Release: b,
			}); err != nil {
				return nil, fmt.Errorf("policy: %w", err)
			}
		}
		logDecision(ctx, wlog, d)
		if len(d.Emit) > 0 {
			res = append(res, WatchReleases{Watch: w, Policy: e.Policy(), Releases: d.Emit})
		}
	}
	return res, nil
}

func compileFor(w store.Watch, schemeFor func(store.Watch) (version.Scheme, error), trust bool) (*Engine, error) {
	p, err := Decode(w.Policy)
	if err != nil {
		return nil, err
	}
	s, err := schemeFor(w)
	if err != nil {
		return nil, err
	}
	e, err := Compile(p, s)
	if err != nil {
		return nil, err
	}
	return e.WithPublishedAt(trust), nil
}

func logDecision(ctx context.Context, logger *slog.Logger, d Decision) {
	for _, dr := range d.Dropped {
		level := slog.LevelDebug
		if dr.Held {
			level = slog.LevelInfo
		}
		if dr.Reason == ReasonCorrupt {
			level = slog.LevelError
		}
		logger.Log(ctx, level, "version dropped by policy", slog.String("version", version.Truncate(dr.Raw, 128)),
			slog.String("reason", string(dr.Reason)), slog.Bool("held", dr.Held))
	}
	if len(d.Hold) > 0 {
		logger.InfoContext(ctx, "versions held for cooldown", slog.Int("count", len(d.Hold)))
	}
}
