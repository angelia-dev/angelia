// Package detect is the scheduler's Checker: it fetches a target through
// the source registered for its kind and commits what it found (known set,
// held versions, events, deliveries) through event.Commit, the
// transactional outbox. It is the glue of detect -> normalize that the
// scheduler leaves out so as not to import the pipeline (scheduler.Checker).
package detect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"time"

	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/scheduler"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/store"
)

// Sources finds the Source registered under a name, which is a source kind or
// the name of an instance of one; *source.Registry implements it. The
// Source's Kind is the base kind.
type Sources interface {
	Get(kind string) (source.Source, error)
}

// Checker implements scheduler.Checker. Its fields are read-only after the
// scheduler starts, so one Checker serves every worker.
type Checker struct {
	// Store commits a check in one transaction; the SQLite store implements it.
	Store event.Committer
	// Sources lists the upstream versions of a target. Production passes a
	// registry whose sources fetch through the netguard client.
	Sources Sources
	// Instance is instance.name, the <instance> of every event's source.
	Instance string
	// RegistryURL returns the configured base URL of a source (its name: a
	// kind, or an instance) when it is not the public registry, otherwise ""
	// (event.CheckInput.RegistryURL). Nil means "" for every source.
	RegistryURL func(name string) string
	// Matcher caches the compiled subscriptions across checks; nil loads
	// them on every commit.
	Matcher *event.MatcherCache
	// Now stamps the check; nil means time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

var _ scheduler.Checker = (*Checker)(nil)

// Check implements scheduler.Checker. A source error, an unknown kind or a
// failed commit is returned (the scheduler backs off); nothing is
// committed then, and the fetch state is kept.
func (c *Checker) Check(ctx context.Context, t store.Target) (scheduler.Outcome, error) {
	if c.Store == nil || c.Sources == nil {
		return scheduler.Outcome{}, errors.New("detect: nil store or sources")
	}
	log := c.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log = log.With("target", t.ID, "kind", t.Kind)
	// t.Kind is the name the watch gave: a kind, or an instance of one. The
	// source, the events, the version scheme and the policy see the base kind.
	src, err := c.Sources.Get(t.Kind)
	if err != nil {
		return scheduler.Outcome{}, fmt.Errorf("detect: target %s: %w", t.ID, err)
	}
	name := t.Kind
	t.Kind = src.Kind()
	var state source.FetchState
	if len(t.FetchState) > 0 {
		// The state is only a hint for a conditional request: one that no
		// longer decodes costs a full fetch, not a stuck target.
		if err := json.Unmarshal(t.FetchState, &state); err != nil {
			log.WarnContext(ctx, "ignoring unreadable fetch state", "error", err)
			state = source.FetchState{}
		}
	}
	res, err := src.List(ctx, source.Target{Kind: t.Kind, Coordinates: maps.Clone(t.Coordinates)}, state)
	if err != nil {
		return scheduler.Outcome{}, fmt.Errorf("detect: target %s: %w", t.ID, err)
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	registry := ""
	if c.RegistryURL != nil {
		registry = c.RegistryURL(name)
	}
	if _, err := event.Commit(ctx, c.Store, event.CheckInput{
		Target: t, Fetched: res, Instance: c.Instance, RegistryURL: registry,
		Now: now(), Logger: log, Matcher: c.Matcher,
	}); err != nil {
		return scheduler.Outcome{}, fmt.Errorf("detect: target %s: commit: %w", t.ID, err)
	}
	// A 304 without validators keeps the stored state (nil); otherwise the
	// source's state replaces it, an empty one included.
	if res.NotModified && res.State.IsZero() {
		return scheduler.Outcome{}, nil
	}
	b, err := json.Marshal(res.State)
	if err != nil {
		return scheduler.Outcome{}, fmt.Errorf("detect: target %s: encode fetch state: %w", t.ID, err)
	}
	return scheduler.Outcome{FetchState: b}, nil
}
