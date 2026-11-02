package event

import (
	"context"
	"encoding/json"
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
	// Matcher caches the compiled subscriptions between calls; nil loads
	// them from tx on every call. The caller owns it (see MatcherCache).
	Matcher *MatcherCache
}

// Emitted counts what Emit and EmitYanks wrote.
type Emitted struct {
	// Deliveries counts every delivery written; Held and Dead are subsets.
	Events, Deliveries, Held, Dead int
	// Cancelled counts the held deliveries that yanks cancelled (deleted).
	Cancelled int
}

func (e Emitted) add(o Emitted) Emitted {
	return Emitted{
		Events: e.Events + o.Events, Deliveries: e.Deliveries + o.Deliveries, Held: e.Held + o.Held,
		Dead: e.Dead + o.Dead, Cancelled: e.Cancelled + o.Cancelled,
	}
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
// have been rejected when stored) cannot be matched; it does not block the
// others. Each event gets a dead delivery for it, with LastError
// InvalidSubscription and the decode error, so the problem shows wherever
// deliveries are listed, and an Error log. Every other error is returned and
// the caller must roll back.
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
	subs, err := in.Matcher.current(ctx, tx, logger)
	if err != nil {
		return n, err
	}
	kind := in.Target.Kind
	trust := policy.TrustsPublishedAt(kind)
	var ids []string
	for _, wr := range rel {
		wp := wr.Policy
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
			ds, next, err := subs.route(match, ev, hold{watchID: wr.Watch.ID, raw: r.Raw}, in.Now, newID, &n, ids)
			if err != nil {
				return n, err
			}
			ids = next
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

// hold identifies the version of an event, for the store.Hold of a delivery
// that a subscription cooldown holds.
type hold struct{ watchID, raw string }

// route returns the deliveries of one event: one for every subscription whose
// filter accepts match and whose policy does not drop ev, due at now or held
// as the policy says, and a dead one for every subscription that does not
// decode. It adds the held and dead ones to n. ids is scratch space, returned
// for reuse.
func (c *compiled) route(
	match subscription.Input, ev subscription.Event, h hold, now time.Time, newID func(time.Time) string, n *Emitted, ids []string,
) ([]store.Delivery, []string, error) {
	var ds []store.Delivery
	ids = c.matcher.Match(match, ids[:0])
	for _, subID := range ids {
		d := store.Delivery{ID: newID(now), SubscriptionID: subID, Status: store.DeliveryPending, NextAttemptAt: now}
		dec := c.policies[subID].Evaluate(ev, now)
		switch dec.Verdict {
		case subscription.Drop:
			continue
		case subscription.Hold:
			input, err := json.Marshal(match)
			if err != nil {
				return nil, ids, fmt.Errorf("event: encode filter input: %w", err)
			}
			d.NextAttemptAt = dec.Until
			d.Hold = &store.Hold{
				Until: dec.Until, StartedAt: ev.CooldownStart, WatchCooldown: ev.WatchCooldown,
				Prerelease: ev.Prerelease, WatchID: h.watchID, Raw: h.raw, Input: input,
			}
			n.Held++
		}
		ds = append(ds, d)
	}
	for _, bad := range c.invalid {
		ds = append(ds, store.Delivery{
			ID: newID(now), SubscriptionID: bad.id, Status: store.DeliveryDead, LastError: bad.reason,
		})
		n.Dead++
	}
	return ds, ids, nil
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
	// Matcher is passed to Emit. When it is set and the committer also
	// implements SubscriptionReader (the SQLite store does), Commit brings it
	// up to date before the transaction starts, so the write lock is not held
	// while subscriptions are loaded.
	Matcher *MatcherCache
}

// Commit writes everything one check found in a single transaction (the
// transactional outbox, architecture.md §2 decision 4): the known set and
// baselines (diff.Apply), the held versions (policy.Apply), the events and
// deliveries of new releases (Emit), and those of yanks (EmitYanks). Any error rolls all of it back, so a failure at any
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
	if r, ok := st.(SubscriptionReader); ok && in.Matcher != nil {
		if err := in.Matcher.Warm(ctx, r, logger); err != nil {
			return Emitted{}, err
		}
	}
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
		ein := EmitInput{
			Target: in.Target, Instance: in.Instance, RegistryURL: in.RegistryURL, Now: in.Now,
			NewID: in.NewID, Logger: logger, Matcher: in.Matcher,
		}
		if n, err = Emit(ctx, tx, rel, ein); err != nil {
			return err
		}
		y, err := EmitYanks(ctx, tx, out.Watches, ein)
		n = n.add(y)
		return err
	})
	if err != nil {
		return Emitted{}, err
	}
	return n, nil
}
