package event

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/angelia-dev/angelia/internal/diff"
	"github.com/angelia-dev/angelia/internal/policy"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/subscription"
	"github.com/angelia-dev/angelia/internal/ulid"
	"github.com/angelia-dev/angelia/internal/version/schemes"
)

// EmitInput is what Emit needs besides the releases.
type EmitInput struct {
	// Target is the checked target.
	Target store.Target
	// Instance is the configuration's instance.name.
	Instance string
	// RegistryURL is the target kind's configured base_url when it is not
	// the public registry, otherwise "" (see Input.RegistryURL).
	RegistryURL string
	// Now is the emission time: event time, delivery due time, and the
	// instant subscription cooldowns are measured at.
	Now time.Time
	// NewID returns a new ULID; nil means ulid.New. Tests inject it.
	NewID func(time.Time) string
	// Logger receives skipped subscriptions (Error). nil discards them.
	Logger *slog.Logger
}

// Emitted counts what Emit wrote.
type Emitted struct {
	Events, Deliveries, Held int
}

// Emit is the outbox step of a check, inside its transaction, after
// policy.Apply: it writes one immutable event per release a watch emits,
// and one delivery for every subscription whose filter matches the event
// and whose policy does not drop it (subscription.Policy.Evaluate). A
// delivery is due at in.Now, or held until the subscription cooldown ends.
// An event that no subscription takes is still written: pull consumers read
// it.
//
// A subscription whose stored filter or policy does not decode (it should
// have been rejected when stored) is skipped with an Error log. Every other
// error is returned and the caller must roll back.
func Emit(ctx context.Context, tx store.Tx, rel []policy.WatchReleases, in EmitInput) (Emitted, error) {
	var n Emitted
	if len(rel) == 0 {
		return n, nil
	}
	logger := in.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	newID := in.NewID
	if newID == nil {
		newID = ulid.New
	}
	matcher, policies, err := loadSubscriptions(ctx, tx, logger)
	if err != nil {
		return n, err
	}
	kind := in.Target.Kind
	trust := policy.TrustsPublishedAt(kind)
	var ids []string
	for _, wr := range rel {
		wp, err := policy.Decode(wr.Watch.Policy)
		if err != nil { // policy.Apply skips such a watch, so it emits nothing
			return n, fmt.Errorf("event: watch %s policy: %w", wr.Watch.ID, err)
		}
		for _, r := range wr.Releases {
			env, err := Build(Input{
				ID: newID(in.Now), Instance: in.Instance, Target: in.Target, RegistryURL: in.RegistryURL,
				Watch: wr.Watch, Release: r, Time: in.Now,
			})
			if err != nil {
				return n, fmt.Errorf("event: watch %s version %q: %w", wr.Watch.ID, r.Raw, err)
			}
			payload, err := json.Marshal(env)
			if err != nil {
				return n, fmt.Errorf("event: encode %s: %w", env.ID, err)
			}
			short := ShortType(env.Type)
			match := subscription.Input{
				Source: kind, Package: env.Data.Package, Labels: env.Data.Watch.Labels, Bump: string(r.Bump), Type: short,
			}
			ev := subscription.Event{
				Type: short, Prerelease: r.Prerelease, WatchPrerelease: string(wp.Prerelease),
				CooldownStart: r.CooldownFrom(trust), WatchCooldown: time.Duration(wp.Cooldown),
			}
			var ds []store.Delivery
			ids = matcher.Match(match, ids[:0])
			for _, subID := range ids {
				d := store.Delivery{ID: newID(in.Now), SubscriptionID: subID, Status: store.DeliveryPending, NextAttemptAt: in.Now}
				dec := policies[subID].Evaluate(ev, in.Now)
				switch dec.Verdict {
				case subscription.Drop:
					continue
				case subscription.Hold:
					input, err := json.Marshal(match)
					if err != nil {
						return n, fmt.Errorf("event: encode filter input: %w", err)
					}
					d.NextAttemptAt = dec.Until
					d.Hold = &store.Hold{
						Until: dec.Until, StartedAt: ev.CooldownStart, WatchCooldown: ev.WatchCooldown,
						Prerelease: r.Prerelease, WatchID: wr.Watch.ID, Raw: r.Raw, Input: input,
					}
					n.Held++
				}
				ds = append(ds, d)
			}
			if err := tx.AppendEvent(ctx, store.Event{
				ID: env.ID, WatchID: wr.Watch.ID, Type: env.Type, Payload: payload, CreatedAt: in.Now,
			}, ds); err != nil {
				return n, fmt.Errorf("event: %w", err)
			}
			n.Events++
			n.Deliveries += len(ds)
		}
	}
	return n, nil
}

// loadSubscriptions builds the matcher and the policies of every
// subscription that decodes.
func loadSubscriptions(ctx context.Context, tx store.Tx, logger *slog.Logger) (*subscription.Matcher, map[string]subscription.Policy, error) {
	subs, err := tx.ListSubscriptions(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("event: %w", err)
	}
	var valid []subscription.Subscription
	policies := make(map[string]subscription.Policy, len(subs))
	for _, s := range subs {
		f, ferr := subscription.FromJSON(s.Filter)
		p, perr := subscription.PolicyFromJSON(s.Policy)
		if err := errors.Join(ferr, perr); err != nil {
			logger.ErrorContext(ctx, "invalid subscription; skipped for this check",
				slog.String("subscription", s.ID), slog.String("error", err.Error()))
			continue
		}
		valid = append(valid, subscription.Subscription{ID: s.ID, Filter: f})
		policies[s.ID] = p
	}
	return subscription.NewMatcher(valid), policies, nil
}

// Committer is the part of store.Store a check commits through.
type Committer interface {
	InTx(ctx context.Context, fn func(tx store.Tx) error) error
}

// CheckInput is one successful fetch of a target, to commit.
type CheckInput struct {
	Target  store.Target
	Fetched source.Result
	// Instance and RegistryURL are as in EmitInput.
	Instance    string
	RegistryURL string
	// Now is the time of the check; events are emitted at it.
	Now    time.Time
	NewID  func(time.Time) string // nil means ulid.New
	Logger *slog.Logger
}

// Commit writes everything one check found in a single transaction (the
// transactional outbox, architecture.md §2 decision 4): the known set and
// baselines (diff.Apply), the held versions (policy.Apply), and the events
// and deliveries (Emit). Any error rolls all of it back, so a failure at any
// step leaves either everything or nothing. The target's fetch state and
// schedule are the scheduler's to write.
func Commit(ctx context.Context, st Committer, in CheckInput) (Emitted, error) {
	logger := in.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	kind := in.Target.Kind
	def, err := schemes.Select(kind, "", logger)
	if err != nil {
		return Emitted{}, fmt.Errorf("event: target %s scheme: %w", in.Target.ID, err)
	}
	schemeFor := policy.SchemeFor(kind, logger)
	var n Emitted
	err = st.InTx(ctx, func(tx store.Tx) error {
		out, err := diff.Apply(ctx, tx, diff.Input{
			TargetID: in.Target.ID, Fetched: in.Fetched, Scheme: def, SchemeFor: schemeFor, Now: in.Now, Logger: logger,
		})
		if err != nil {
			return err
		}
		rel, err := policy.Apply(ctx, tx, out, policy.Input{
			TargetID: in.Target.ID, SchemeFor: schemeFor, Now: in.Now,
			TrustPublishedAt: policy.TrustsPublishedAt(kind), Logger: logger,
		})
		if err != nil {
			return err
		}
		n, err = Emit(ctx, tx, rel, EmitInput{
			Target: in.Target, Instance: in.Instance, RegistryURL: in.RegistryURL, Now: in.Now,
			NewID: in.NewID, Logger: logger,
		})
		return err
	})
	if err != nil {
		return Emitted{}, err
	}
	return n, nil
}
