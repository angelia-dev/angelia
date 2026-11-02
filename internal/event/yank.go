package event

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"time"

	"github.com/angelia-dev/angelia/internal/diff"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/subscription"
	"github.com/angelia-dev/angelia/internal/ulid"
	"github.com/angelia-dev/angelia/internal/version"
)

// YankInput is what BuildYank needs for one release.yanked event.
type YankInput struct {
	// ID is the event id, a ULID; empty means a new one for Time.
	ID string
	// Instance is the configuration's instance.name.
	Instance string
	// Watch is the watch as it is now: its name and labels go in data.watch,
	// so subscription filters see the current labels.
	Watch store.Watch
	// Original is the stored payload (store.Event.Payload) of the event the
	// watch emitted for the version: a release.published or
	// release.prerelease event of the same watch.
	Original json.RawMessage
	// Time is when the yank was seen, the end of the check: both detected_at
	// and the event time.
	Time time.Time
}

// BuildYank builds the release.yanked event of a version the watch emitted
// (spec docs/release-event-v1.md, "Rules that depend on the type"). It
// repeats the original payload, so the classification (version,
// normalized_version, previous_version, scheme, bump, backport, prerelease)
// and the URLs, dates and purl are what the subscriber received, and sets:
// a new id, type release.yanked, time and detected_at to in.Time, yanked
// true, cooldown_satisfied_at null, and source and data.watch from the
// current instance name and watch. It does no I/O. An input from which no
// event valid under the spec can be built, or an original that is not a
// published or pre-release event of this watch, is an error wrapping
// ErrInvalid.
func BuildYank(in YankInput) (Envelope, error) {
	var orig Envelope
	if err := json.Unmarshal(in.Original, &orig); err != nil {
		return Envelope{}, fmt.Errorf("%w: original event: %w", ErrInvalid, err)
	}
	switch {
	case !instanceRE.MatchString(in.Instance):
		return Envelope{}, fmt.Errorf("%w: instance name %q", ErrInvalid, in.Instance)
	case !ulid.Valid(in.Watch.ID):
		return Envelope{}, fmt.Errorf("%w: watch id %q is not a ULID", ErrInvalid, in.Watch.ID)
	case in.Time.IsZero():
		return Envelope{}, fmt.Errorf("%w: yank without detection time", ErrInvalid)
	case orig.Type != TypePublished && orig.Type != TypePrerelease:
		return Envelope{}, fmt.Errorf("%w: original event %s has type %q", ErrInvalid, orig.ID, orig.Type)
	case orig.Data.Watch.ID != in.Watch.ID:
		return Envelope{}, fmt.Errorf("%w: original event %s belongs to watch %q", ErrInvalid, orig.ID, orig.Data.Watch.ID)
	case orig.Data.Version == "" || orig.Subject == "":
		return Envelope{}, fmt.Errorf("%w: original event %s has no version or subject", ErrInvalid, orig.ID)
	}
	if err := checkLabels(in.Watch.Labels); err != nil {
		return Envelope{}, err
	}
	id := in.ID
	if id == "" {
		id = ulid.New(in.Time)
	}
	var name *string
	if in.Watch.Name != "" {
		name = &in.Watch.Name
	}
	labels := maps.Clone(in.Watch.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	env := orig
	env.SpecVersion, env.DataContentType, env.DataSchema = SpecVersion, DataContentType, DataSchema
	env.Type = TypeYanked
	env.ID = id
	env.Source = "angelia://" + in.Instance + "/watches/" + in.Watch.ID
	env.Time = timestamp(in.Time)
	env.Data.Yanked = true
	env.Data.DetectedAt = timestamp(in.Time)
	env.Data.CooldownSatisfiedAt = nil
	env.Data.Watch = Watch{ID: in.Watch.ID, Name: name, Labels: labels}
	return env, nil
}

// EmitYanks is the yank step of a check, inside its transaction, after
// diff.Apply (ADR 0018). For each version that a watch's changes report as
// yanked (diff.KindYanked) it:
//
//  1. cancels the deliveries of that version the watch's events still hold
//     for a subscription cooldown (store.Tx.CancelHeldDeliveries, ADR 0014);
//  2. finds the release.published or release.prerelease event the watch
//     emitted for it (same subject). When there is none (a version known
//     from the baseline, dropped by the policy, yanked while the watch held
//     it, or emitted before the watch was moved to another target) it writes
//     nothing more and logs at Info: release.yanked is only for versions the
//     watch emitted;
//  3. otherwise writes one release.yanked event (BuildYank) whose
//     store.Event.RefEventID is that event's id, with one delivery per
//     subscription whose filter matches it. Subscription policies do not
//     apply to it (subscription.Policy.Evaluate admits it); a subscription
//     that does not decode gets a dead delivery, as in Emit.
//
// Un-yanks (diff.KindUnyanked) are logged at Info: v1 has no event for them.
// Errors are returned and the caller must roll back.
func EmitYanks(ctx context.Context, tx store.Tx, watches []diff.WatchChanges, in EmitInput) (Emitted, error) {
	var n Emitted
	logger := in.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	newID := in.NewID
	if newID == nil {
		newID = ulid.New
	}
	kind := in.Target.Kind
	pkg := Package(kind, in.Target.Coordinates)
	var subs *compiled
	var ids []string
	for _, wc := range watches {
		w := wc.Watch
		for _, c := range wc.Changes {
			raw := version.Truncate(c.Raw, 128)
			switch c.Kind {
			case diff.KindUnyanked:
				logger.InfoContext(ctx, "version un-yanked upstream; no event in v1",
					slog.String("watch", w.ID), slog.String("version", raw))
				continue
			case diff.KindYanked:
			default:
				continue
			}
			cancelled, err := tx.CancelHeldDeliveries(ctx, w.ID, c.Raw, in.Now)
			if err != nil {
				return Emitted{}, fmt.Errorf("event: %w", err)
			}
			n.Cancelled += int(cancelled)
			if cancelled > 0 {
				logger.InfoContext(ctx, "held deliveries of a yanked version cancelled",
					slog.String("watch", w.ID), slog.String("version", raw), slog.Int64("count", cancelled))
			}
			orig, err := tx.GetLatestEventBySubject(ctx, w.ID, kind+":"+pkg+"@"+c.Raw, TypePublished, TypePrerelease)
			if errors.Is(err, store.ErrNotFound) {
				logger.InfoContext(ctx, "version yanked upstream that this watch never emitted; no event",
					slog.String("watch", w.ID), slog.String("version", raw))
				continue
			}
			if err != nil {
				return Emitted{}, fmt.Errorf("event: %w", err)
			}
			env, err := BuildYank(YankInput{ID: newID(in.Now), Instance: in.Instance, Watch: w, Original: orig.Payload, Time: in.Now})
			if err != nil {
				return Emitted{}, fmt.Errorf("event: watch %s yank of %q: %w", w.ID, raw, err)
			}
			payload, err := json.Marshal(env)
			if err != nil {
				return Emitted{}, fmt.Errorf("event: encode %s: %w", env.ID, err)
			}
			if subs == nil {
				if subs, err = in.Matcher.current(ctx, tx, logger); err != nil {
					return Emitted{}, err
				}
			}
			bump := ""
			if env.Data.Bump != nil {
				bump = *env.Data.Bump
			}
			short := ShortType(env.Type)
			match := subscription.Input{
				Source: env.Data.Source, Package: env.Data.Package, Labels: env.Data.Watch.Labels, Bump: bump, Type: short,
			}
			ev := subscription.Event{Type: short, Prerelease: env.Data.Prerelease}
			var ds []store.Delivery
			ds, ids, err = subs.route(match, ev, hold{watchID: w.ID, raw: c.Raw}, in.Now, newID, &n, ids)
			if err != nil {
				return Emitted{}, err
			}
			if err := tx.AppendEvent(ctx, store.Event{
				ID: env.ID, WatchID: w.ID, Type: env.Type, Payload: payload, CreatedAt: in.Now, RefEventID: orig.ID,
			}, ds); err != nil {
				return Emitted{}, fmt.Errorf("event: %w", err)
			}
			n.Events++
			n.Deliveries += len(ds)
		}
	}
	return n, nil
}
