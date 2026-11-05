package api

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/angelia-dev/angelia/internal/policy"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/version"
	"github.com/angelia-dev/angelia/internal/version/schemes"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

// watchReader is the lookup both the store and a transaction offer.
type watchReader interface {
	WatchByName(ctx context.Context, name string) (store.Watch, error)
	GetTarget(ctx context.Context, id string) (store.Target, error)
}

func (s *Server) parseOptions() watchfile.Options {
	return watchfile.Options{
		Sources: s.opts.Sources,
		// Only feeds warnings, which the API drops.
		LookupEnv: func(string) (string, bool) { return "set", true },
	}
}

// effectiveInterval is the interval the scheduler uses for a target.
func effectiveInterval(t store.Target) time.Duration {
	if t.Interval > 0 {
		return t.Interval
	}
	return watchfile.DefaultInterval
}

// watchJSONOf builds the response of a watch and its target.
func watchJSONOf(w store.Watch, t store.Target) (watchJSON, error) {
	p, err := policy.Decode(w.Policy)
	if err != nil {
		return watchJSON{}, err
	}
	scheme := p.Scheme
	if scheme == "" {
		scheme = schemes.ForSourceKind(t.Kind).Name()
	}
	bump := make([]string, len(p.Bump))
	for i, b := range p.Bump {
		bump[i] = string(b)
	}
	labels := w.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	effective := formatDuration(effectiveInterval(t))
	interval := effective // a row written before watches stored their own
	if w.Interval > 0 {
		interval = formatDuration(w.Interval)
	}
	return watchJSON{
		Name: w.Name, ID: w.ID, ManagedBy: ownerOf(w), Source: t.Kind, Coordinates: t.Coordinates,
		Interval: interval, Scheme: scheme, Labels: labels,
		Policy: watchPolicyJSON{
			Prerelease: string(p.Prerelease), Include: nonNil(p.Include), Exclude: nonNil(p.Exclude),
			Constraint: p.Constraint, Bump: bump, Cooldown: formatDuration(time.Duration(p.Cooldown)), Baseline: p.Baseline,
		},
		Status: watchStatusJSON{
			BaselineDone: w.BaselineDone, EffectiveInterval: effective,
			LastCheckedAt: ts(t.LastCheckedAt), NextCheckAt: ts(t.NextCheckAt), ErrorCount: t.ErrorCount,
		},
	}, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (s *Server) readWatch(ctx context.Context, r watchReader, name string) (store.Watch, store.Target, error) {
	w, err := r.WatchByName(ctx, name)
	if w, err = readByName(w, err, "watch", name); err != nil {
		return store.Watch{}, store.Target{}, err
	}
	t, err := r.GetTarget(ctx, w.TargetID)
	if err != nil {
		return store.Watch{}, store.Target{}, storeProblem(err, "target of watch", name)
	}
	return w, t, nil
}

// GetWatch implements StrictServerInterface.
func (s *Server) GetWatch(ctx context.Context, req GetWatchRequestObject) (GetWatchResponseObject, error) {
	w, t, err := s.readWatch(ctx, s.opts.Store, req.Name)
	if err != nil {
		return nil, err
	}
	body, err := watchJSONOf(w, t)
	if err != nil {
		return nil, err
	}
	return jsonOK(body), nil
}

// ListWatches implements StrictServerInterface.
func (s *Server) ListWatches(ctx context.Context, req ListWatchesRequestObject) (ListWatchesResponseObject, error) {
	cur, p := decodeCursor(req.Params.Cursor, cursorWatches)
	if p != nil {
		return nil, p
	}
	limit := pageLimit(req.Params.Limit)
	var sources []string
	if req.Params.Source != nil {
		sources = *req.Params.Source
	}
	labels := map[string]string{}
	if req.Params.Label != nil {
		for _, l := range *req.Params.Label {
			k, v, _ := strings.Cut(l, "=")
			labels[k] = v
		}
	}
	targets := map[string]store.Target{}
	target := func(id string) (store.Target, error) {
		if t, ok := targets[id]; ok {
			return t, nil
		}
		t, err := s.opts.Store.GetTarget(ctx, id)
		targets[id] = t
		return t, err
	}
	items, next, err := pageByName(cur, limit,
		func(afterName, afterID string, n int) ([]store.Watch, error) {
			return s.opts.Store.ListNamedWatches(ctx, afterName, afterID, n)
		},
		func(w store.Watch) (string, string) { return w.Name, w.ID },
		func(w store.Watch) (bool, error) {
			for k, v := range labels {
				if w.Labels[k] != v {
					return false, nil
				}
			}
			if len(sources) == 0 {
				return true, nil
			}
			t, err := target(w.TargetID)
			return slices.Contains(sources, t.Kind), err
		})
	if err != nil {
		return nil, err
	}
	page := pageJSON[watchJSON]{Items: make([]watchJSON, 0, len(items)), NextCursor: next}
	for _, w := range items {
		t, err := target(w.TargetID)
		if err != nil {
			return nil, err
		}
		wj, err := watchJSONOf(w, t)
		if err != nil {
			return nil, err
		}
		page.Items = append(page.Items, wj)
	}
	return jsonOK(page), nil
}

// pageByName pages through rows ordered by (name, id), keeping those keep
// accepts, and returns up to limit of them with the cursor of the next page.
func pageByName[T any](cur pageCursor, limit int,
	fetch func(afterName, afterID string, n int) ([]T, error),
	key func(T) (string, string),
	keep func(T) (bool, error),
) ([]T, *string, error) {
	const batch = 500
	afterName, afterID := cur.N, cur.I
	var items []T
	for len(items) <= limit {
		rows, err := fetch(afterName, afterID, batch)
		if err != nil {
			return nil, nil, err
		}
		for _, row := range rows {
			afterName, afterID = key(row)
			ok, err := keep(row)
			if err != nil {
				return nil, nil, err
			}
			if ok {
				if items = append(items, row); len(items) > limit {
					break
				}
			}
		}
		if len(rows) < batch {
			break
		}
	}
	if len(items) <= limit {
		return items, nil, nil
	}
	items = items[:limit]
	n, i := key(items[limit-1])
	return items, encodeCursor(pageCursor{K: cur.K, N: n, I: i}), nil
}

// parseWatchBody validates a PUT body by the rules of the watch file.
func (s *Server) parseWatchBody(name string, raw []byte) (watchfile.Watch, []fieldError) {
	errs := bodyNameErrors(raw, name)
	data := truncateDurations(raw, []string{"interval"}, []string{"policy", "cooldown"})
	w, perrs := watchfile.ParseWatch(data, name, s.parseOptions())
	return w, append(errs, parseErrors(perrs, "labels", "coordinates")...)
}

// PutWatch implements StrictServerInterface.
func (s *Server) PutWatch(ctx context.Context, req PutWatchRequestObject) (PutWatchResponseObject, error) {
	w, errs := s.parseWatchBody(req.Name, stateFrom(ctx).body)
	if len(errs) > 0 {
		return nil, validationProblem("watch", errs)
	}
	dry := req.Params.DryRun != nil && *req.Params.DryRun
	var body watchJSON
	var change Change
	err := s.inTx(ctx, dry, func(tx store.APITx) error {
		var err error
		change, err = s.applyWatch(ctx, tx, w)
		if err != nil {
			return err
		}
		sw, t, err := s.readWatch(ctx, tx, w.Name)
		if err != nil {
			return err
		}
		body, err = watchJSONOf(sw, t)
		return err
	})
	if err != nil {
		return nil, err
	}
	return putResponse(change, location("watches", w.Name), body), nil
}

// applyWatch creates or updates the API watch w, as the watch file apply
// does for one of its watches.
func (s *Server) applyWatch(ctx context.Context, tx store.APITx, w watchfile.Watch) (Change, error) {
	old, err := tx.WatchByName(ctx, w.Name)
	cur, err := writeByName(old, err, "watch", w.Name)
	if err != nil {
		return "", err
	}
	now := s.now()
	t, err := tx.FindTarget(ctx, w.Source, w.Coordinates)
	newTarget := false
	switch {
	case err == nil:
	case isNotFound(err):
		t = store.Target{ID: s.opts.NewID(), Kind: w.Source, Coordinates: w.Coordinates, NextCheckAt: now, Interval: w.Interval}
		if err := tx.CreateTarget(ctx, t); err != nil {
			return "", err
		}
		newTarget = true
	default:
		return "", err
	}
	pol := w.PolicyJSON()
	change := ChangeUnchanged
	if cur == nil {
		if err := tx.CreateWatch(ctx, watchRow(s.opts.NewID(), t.ID, w)); err != nil {
			return "", err
		}
		change = ChangeCreated
	} else {
		moved := cur.TargetID != t.ID
		if moved || cur.Interval != w.Interval || !jsonEqual(cur.Policy, pol) || !sameLabels(cur.Labels, w.Labels) {
			if err := tx.UpdateWatch(ctx, watchRow(cur.ID, t.ID, w)); err != nil {
				return "", err
			}
			if moved {
				// As in the file apply: a moved watch starts a new baseline,
				// and versions held on the old target must not come back.
				if _, err := tx.DeletePendingReleasesOfWatch(ctx, cur.ID); err != nil {
					return "", err
				}
				if err := tx.ResetWatchBaseline(ctx, cur.ID); err != nil {
					return "", err
				}
				gone, err := tx.DeleteTargetIfUnused(ctx, cur.TargetID)
				if err != nil {
					return "", err
				}
				if !gone {
					if _, _, err := tx.RecomputeTargetInterval(ctx, cur.TargetID, now); err != nil {
						return "", err
					}
				}
			}
			change = ChangeUpdated
		}
	}
	if !newTarget { // a new target was created with the watch's interval
		if _, _, err := tx.RecomputeTargetInterval(ctx, t.ID, now); err != nil {
			return "", err
		}
	}
	return change, nil
}

// watchRow is the store row of an API watch. Interval is always set:
// UpdateWatch clears a zero one.
func watchRow(id, targetID string, w watchfile.Watch) store.Watch {
	return store.Watch{
		ID: id, TargetID: targetID, Policy: w.PolicyJSON(), Labels: w.Labels, Interval: w.Interval,
		Name: w.Name, ManagedBy: store.ManagedByAPI,
	}
}

func sameLabels(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// DeleteWatch implements StrictServerInterface.
func (s *Server) DeleteWatch(ctx context.Context, req DeleteWatchRequestObject) (DeleteWatchResponseObject, error) {
	dry := req.Params.DryRun != nil && *req.Params.DryRun
	err := s.inTx(ctx, dry, func(tx store.APITx) error {
		old, err := tx.WatchByName(ctx, req.Name)
		cur, err := writeByName(old, err, "watch", req.Name)
		if err != nil {
			return err
		}
		if cur == nil {
			return problem(SlugNotFound, "no watch named %s", req.Name)
		}
		if err := tx.DeleteWatch(ctx, cur.ID); err != nil {
			return err
		}
		gone, err := tx.DeleteTargetIfUnused(ctx, cur.TargetID)
		if err != nil || gone {
			return err
		}
		_, _, err = tx.RecomputeTargetInterval(ctx, cur.TargetID, s.now())
		return err
	})
	if err != nil {
		return nil, err
	}
	return deleteResponse(), nil
}

// ListWatchVersions implements StrictServerInterface.
func (s *Server) ListWatchVersions(ctx context.Context, req ListWatchVersionsRequestObject) (ListWatchVersionsResponseObject, error) {
	cur, p := decodeCursor(req.Params.Cursor, cursorVersions)
	if p != nil {
		return nil, p
	}
	w, t, err := s.readWatch(ctx, s.opts.Store, req.Name)
	if err != nil {
		return nil, err
	}
	pol, err := policy.Decode(w.Policy)
	if err != nil {
		return nil, err
	}
	scheme, err := schemes.Select(t.Kind, pol.Scheme, nil)
	if err != nil {
		return nil, err
	}
	known, err := s.opts.Store.ListVersions(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	sortVersions(scheme, known)
	start := 0
	if cur.N != "" {
		i := slices.IndexFunc(known, func(v store.Version) bool { return v.Raw == cur.N })
		if i < 0 || cur.I != t.ID {
			return nil, problem(SlugBadRequest, "query parameter cursor: not a cursor issued by this server")
		}
		start = i + 1
	}
	limit := pageLimit(req.Params.Limit)
	page := pageJSON[versionJSON]{Items: []versionJSON{}}
	for i := start; i < len(known); i++ {
		v := known[i]
		if req.Params.Prerelease != nil && v.Prerelease != *req.Params.Prerelease {
			continue
		}
		if req.Params.Yanked != nil && v.Yanked != *req.Params.Yanked {
			continue
		}
		if len(page.Items) == limit {
			last := page.Items[len(page.Items)-1].Version
			page.NextCursor = encodeCursor(pageCursor{K: cursorVersions, N: last, I: t.ID})
			break
		}
		page.Items = append(page.Items, versionJSON{
			Version: v.Raw, NormalizedVersion: v.Normalized, Prerelease: v.Prerelease, Yanked: v.Yanked,
			PublishedAt: ts(v.PublishedAt), FirstSeenAt: timestamp(v.FirstSeenAt), RemovedAt: ts(v.RemovedAt),
		})
	}
	return jsonOK(page), nil
}

// sortVersions orders a known set greatest first by the watch's scheme;
// versions the scheme cannot parse go last, by raw string.
func sortVersions(scheme version.Scheme, vs []store.Version) {
	parsed := make(map[string]version.Version, len(vs))
	for _, v := range vs {
		if pv, err := scheme.Parse(v.Raw); err == nil {
			parsed[v.Raw] = pv
		}
	}
	slices.SortStableFunc(vs, func(a, b store.Version) int {
		pa, oka := parsed[a.Raw]
		pb, okb := parsed[b.Raw]
		switch {
		case oka && okb:
			if c := scheme.Compare(pb, pa); c != 0 {
				return c
			}
		case oka:
			return -1
		case okb:
			return 1
		}
		return cmp.Compare(b.Raw, a.Raw)
	})
}
