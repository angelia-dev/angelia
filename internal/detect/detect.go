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
	"slices"
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

// NotesFiller fills the release notes of new versions from outside their
// source (the GitHub release of the repository a PyPI or npm package
// declares, ADR 0045). Fill gets the new versions of one check, newest first,
// and returns them with Notes and NotesURL set where it found some; it never
// fails, and an unchanged copy is a valid answer. repolink.Filler implements
// it.
type NotesFiller interface {
	Fill(ctx context.Context, t source.Target, versions []source.Version) []source.Version
}

// KnownVersions is the optional part of the store that reads a target's known
// set outside a check transaction; the SQLite and Postgres stores implement
// it ([store.Tx].ListVersions). Without it the notes step is skipped.
type KnownVersions interface {
	ListVersions(ctx context.Context, targetID string) ([]store.Version, error)
}

// MaxNotesVersions is how many of a check's new versions the notes step
// looks at (the newest ones).
const MaxNotesVersions = 5

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
	// Notes, when set, fills the notes of the new versions of a check before
	// it is committed (ADR 0045). The step runs only for a fetch that
	// returned data (not a 304), only when the target already has a known
	// set (its first check is the baseline and emits nothing), only for
	// versions that are new, not yanked and not among the MaxNotesVersions
	// newest, and never inside the commit's transaction. A failure of the
	// step never fails the check.
	Notes NotesFiller
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
	log = log.With("target", t.ID, "source", t.Kind, "kind", t.EffectiveSourceKind())
	// t.Kind is the name the watch gave: a kind, or an instance of one. The
	// source, the events, the version scheme and the policy see the base kind.
	src, err := c.Sources.Get(t.Kind)
	if err != nil {
		return scheduler.Outcome{}, fmt.Errorf("detect: target %s: %w", t.ID, err)
	}
	// A target keeps the kind it was created for: a source that was
	// reconfigured to another kind, or a built-in that took an instance's
	// name, is not allowed to reinterpret it (no fetch, no events).
	name := t.Kind
	if want := t.EffectiveSourceKind(); src.Kind() != want {
		return scheduler.Outcome{}, fmt.Errorf("detect: target %s: %w: source %q is now of kind %q, but the target was created for kind %q; give the source a new name",
			t.ID, scheduler.ErrPermanent, name, src.Kind(), want)
	}
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
	if c.Notes != nil && !res.NotModified {
		res.Versions = c.fillNotes(ctx, log, t, res.Versions)
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

// fillNotes runs the notes step (D81): the versions of fetched that the
// target's known set lacks, are not yanked and are among the newest
// MaxNotesVersions go to Notes, and only their Notes and NotesURL are taken
// back. It reads the known set with its own short read, before the commit's
// transaction, so no network call happens inside it. fetched is not modified.
func (c *Checker) fillNotes(ctx context.Context, log *slog.Logger, t store.Target, fetched []source.Version) []source.Version {
	kv, ok := c.Store.(KnownVersions)
	if !ok || len(fetched) == 0 {
		return fetched
	}
	known, err := kv.ListVersions(ctx, t.ID)
	if err != nil {
		log.WarnContext(ctx, "release notes: cannot read the known versions", "error", err)
		return fetched
	}
	if len(known) == 0 {
		return fetched // the baseline check: nothing is emitted
	}
	seen := make(map[string]bool, len(known))
	for _, v := range known {
		seen[v.Raw] = true
	}
	var picked []source.Version
	for _, v := range fetched {
		if len(picked) == MaxNotesVersions {
			break
		}
		if v.Raw == "" || v.Yanked || seen[v.Raw] {
			continue
		}
		picked = append(picked, v)
	}
	if len(picked) == 0 {
		return fetched
	}
	filled := c.Notes.Fill(ctx, source.Target{Kind: t.Kind, Coordinates: maps.Clone(t.Coordinates)}, slices.Clone(picked))
	if len(filled) != len(picked) {
		log.WarnContext(ctx, "release notes: the filler returned a different number of versions; ignored")
		return fetched
	}
	byRaw := make(map[string]source.Version, len(filled))
	for i, v := range filled {
		if v.Raw == picked[i].Raw {
			byRaw[v.Raw] = v
		}
	}
	out := slices.Clone(fetched)
	for i, v := range out {
		if f, ok := byRaw[v.Raw]; ok && !seen[v.Raw] && !v.Yanked {
			out[i].Notes, out[i].NotesURL = f.Notes, f.NotesURL
		}
	}
	return out
}
