package watchfile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// Action is what apply does, or would do, to one object.
type Action string

// The actions of docs/watch-file.md section 4.
const (
	Create    Action = "create"
	Update    Action = "update"
	Delete    Action = "delete"
	Unchanged Action = "unchanged"
)

// Object kinds in a Change.
const (
	KindSink         = "sink"
	KindWatch        = "watch"
	KindSubscription = "subscription"
	KindTarget       = "target"
)

// Change is one line of a Plan.
type Change struct {
	Kind   string
	Name   string // the file name; for a target, its id
	Action Action
	Detail string // what differs, or what is removed with it
}

func (c Change) String() string {
	s := fmt.Sprintf("%-9s %s %s", c.Action, c.Kind, c.Name)
	if c.Detail != "" {
		s += " (" + c.Detail + ")"
	}
	return s
}

// Plan is the ordered list of changes of one reconciliation.
type Plan struct {
	Changes []Change
}

// Count returns how many changes have the given kind and action.
func (p *Plan) Count(kind string, a Action) int {
	n := 0
	for _, c := range p.Changes {
		if c.Kind == kind && c.Action == a {
			n++
		}
	}
	return n
}

// Empty reports whether applying the plan changes nothing.
func (p *Plan) Empty() bool {
	return !slices.ContainsFunc(p.Changes, func(c Change) bool { return c.Action != Unchanged })
}

// String lists the changes that are not no-ops, one per line.
func (p *Plan) String() string {
	var b strings.Builder
	for _, c := range p.Changes {
		if c.Action != Unchanged {
			b.WriteString(c.String() + "\n")
		}
	}
	return b.String()
}

// ApplyOptions controls Plan and Apply. The zero value is the real clock and
// random ULIDs.
type ApplyOptions struct {
	Now   func() time.Time
	NewID func() string
}

// Apply reconciles the file into the store in one transaction and returns what
// it did. On any error nothing is written. Rows not owned by the file
// (managed_by other than "file") are never read for changes or touched.
func Apply(ctx context.Context, st store.Store, f *File, o ApplyOptions) (*Plan, error) {
	return run(ctx, st, f, o, false)
}

// PlanFile computes what Apply would do and writes nothing.
func PlanFile(ctx context.Context, st store.Store, f *File, o ApplyOptions) (*Plan, error) {
	return run(ctx, st, f, o, true)
}

func run(ctx context.Context, st store.Store, f *File, o ApplyOptions, dry bool) (*Plan, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	now := o.Now().UTC().Truncate(time.Millisecond)
	if o.NewID == nil {
		o.NewID = func() string { return newULID(o.Now()) }
	}
	plan := &Plan{}
	err := st.InConfigTx(ctx, func(tx store.ConfigTx) error {
		plan = &Plan{} // a retried transaction starts over
		r := &reconciler{ctx: ctx, tx: tx, now: now, newID: o.NewID, dry: dry, plan: plan}
		return r.reconcile(f)
	})
	if err != nil {
		return nil, fmt.Errorf("apply watch file: %w", err)
	}
	return plan, nil
}

type reconciler struct {
	ctx   context.Context
	tx    store.ConfigTx
	now   time.Time
	newID func() string
	dry   bool
	plan  *Plan
}

func (r *reconciler) add(kind, name string, a Action, detail string) int {
	r.plan.Changes = append(r.plan.Changes, Change{Kind: kind, Name: name, Action: a, Detail: detail})
	return len(r.plan.Changes) - 1
}

// write runs fn unless this is a dry run.
func (r *reconciler) write(fn func() error) error {
	if r.dry {
		return nil
	}
	return fn()
}

func (r *reconciler) reconcile(f *File) error {
	oldSinks, err := r.tx.ListManagedSinks(r.ctx, store.ManagedByFile)
	if err != nil {
		return err
	}
	oldWatches, err := r.tx.ListManagedWatches(r.ctx, store.ManagedByFile)
	if err != nil {
		return err
	}
	oldSubs, err := r.tx.ListManagedSubscriptions(r.ctx, store.ManagedByFile)
	if err != nil {
		return err
	}

	sinkIDs, err := r.sinks(f, oldSinks)
	if err != nil {
		return err
	}
	targetsToCheck, wanted, err := r.watches(f, oldWatches)
	if err != nil {
		return err
	}
	if err := r.subscriptions(f, oldSubs, sinkIDs); err != nil {
		return err
	}

	// Deletes, in reverse dependency order.
	for _, s := range oldSubs {
		if slices.ContainsFunc(f.Subscriptions, func(d Subscription) bool { return d.Name == s.Name }) {
			continue
		}
		var pending int64
		if !r.dry {
			if pending, err = r.tx.DeletePendingDeliveries(r.ctx, s.ID); err != nil {
				return err
			}
		}
		if err := r.write(func() error { return r.tx.DeleteSubscription(r.ctx, s.ID) }); err != nil {
			return err
		}
		detail := ""
		if pending > 0 {
			detail = fmt.Sprintf("%d pending deliveries removed", pending)
		}
		r.add(KindSubscription, s.Name, Delete, detail)
	}
	for _, w := range oldWatches {
		if slices.ContainsFunc(f.Watches, func(d Watch) bool { return d.Name == w.Name }) {
			continue
		}
		if err := r.write(func() error { return r.tx.DeleteWatch(r.ctx, w.ID) }); err != nil {
			return err
		}
		targetsToCheck[w.TargetID] = true
		r.add(KindWatch, w.Name, Delete, "history kept")
	}
	for _, s := range oldSinks {
		if slices.ContainsFunc(f.Sinks, func(d Sink) bool { return d.Name == s.Name }) {
			continue
		}
		if err := r.write(func() error { return r.tx.DeleteSink(r.ctx, s.ID) }); err != nil {
			return err
		}
		r.add(KindSink, s.Name, Delete, "")
	}

	// Targets no watch uses any more, with their known versions.
	for _, id := range sortedKeys(targetsToCheck) {
		if wanted[id] {
			continue // a watch of the file still uses it
		}
		ws, err := r.tx.ListWatches(r.ctx, id)
		if err != nil {
			return err
		}
		// Watches the file owns are gone or moved away; only the others stay.
		if slices.ContainsFunc(ws, func(w store.Watch) bool { return w.ManagedBy != store.ManagedByFile }) {
			continue
		}
		deleted := true
		if !r.dry {
			if err := r.write(func() error {
				var err error
				deleted, err = r.tx.DeleteTargetIfUnused(r.ctx, id)
				return err
			}); err != nil {
				return err
			}
		}
		if deleted {
			r.add(KindTarget, id, Delete, "no watch uses it")
		}
	}
	return nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func (r *reconciler) sinks(f *File, old []store.Sink) (map[string]string, error) {
	byName := map[string]store.Sink{}
	for _, s := range old {
		byName[s.Name] = s
	}
	ids := map[string]string{}
	for _, d := range f.Sinks {
		cur, ok := byName[d.Name]
		switch {
		case !ok:
			id := r.newID()
			ids[d.Name] = id
			if err := r.write(func() error {
				return r.tx.CreateSink(r.ctx, store.Sink{ID: id, Kind: d.Kind, ConfigEncrypted: d.Config, Name: d.Name, ManagedBy: store.ManagedByFile})
			}); err != nil {
				return nil, err
			}
			r.add(KindSink, d.Name, Create, d.Kind)
		case cur.Kind != d.Kind || !bytes.Equal(cur.ConfigEncrypted, d.Config):
			ids[d.Name] = cur.ID
			if err := r.write(func() error {
				return r.tx.UpdateSink(r.ctx, store.Sink{ID: cur.ID, Kind: d.Kind, ConfigEncrypted: d.Config})
			}); err != nil {
				return nil, err
			}
			r.add(KindSink, d.Name, Update, "")
		default:
			ids[d.Name] = cur.ID
			r.add(KindSink, d.Name, Unchanged, "")
		}
	}
	return ids, nil
}

// targetKey identifies a target by kind and canonical coordinates.
func targetKey(kind string, coords map[string]string) string {
	b, _ := json.Marshal(coords) // map keys are sorted: canonical
	return kind + "\x00" + string(b)
}

type targetState struct {
	id       string
	existing *store.Target // nil: created by this run
	interval time.Duration // desired: the smallest interval among the file's watches on it
	watches  []int         // plan indexes of the file's watches on it
}

func (r *reconciler) watches(f *File, old []store.Watch) (toCheck, wanted map[string]bool, err error) {
	toCheck, wanted = map[string]bool{}, map[string]bool{}
	byName := map[string]store.Watch{}
	for _, w := range old {
		byName[w.Name] = w
	}

	targets := map[string]*targetState{}
	var order []string
	for _, d := range f.Watches {
		k := targetKey(d.Source, d.Coordinates)
		if t := targets[k]; t != nil {
			t.interval = min(t.interval, d.Interval)
			continue
		}
		t := &targetState{interval: d.Interval}
		ex, err := r.tx.FindTarget(r.ctx, d.Source, d.Coordinates)
		switch {
		case err == nil:
			t.id, t.existing = ex.ID, &ex
		case errors.Is(err, store.ErrNotFound):
			t.id = r.newID()
		default:
			return nil, nil, err
		}
		targets[k] = t
		wanted[t.id] = true
		order = append(order, k)
	}

	for _, d := range f.Watches {
		t := targets[targetKey(d.Source, d.Coordinates)]
		if t.existing == nil && len(t.watches) == 0 { // the first watch on a new target creates it
			tgt := store.Target{ID: t.id, Kind: d.Source, Coordinates: d.Coordinates, NextCheckAt: r.now, Interval: t.interval}
			if err := r.write(func() error { return r.tx.CreateTarget(r.ctx, tgt) }); err != nil {
				return nil, nil, err
			}
			r.add(KindTarget, t.id, Create, d.Source)
		}
		policy := d.PolicyJSON()
		cur, ok := byName[d.Name]
		var idx int
		switch {
		case !ok:
			id := r.newID()
			if err := r.write(func() error {
				return r.tx.CreateWatch(r.ctx, store.Watch{
					ID: id, TargetID: t.id, Policy: policy, Labels: d.Labels, Name: d.Name, ManagedBy: store.ManagedByFile,
				})
			}); err != nil {
				return nil, nil, err
			}
			idx = r.add(KindWatch, d.Name, Create, d.Source)
		default:
			var diffs []string
			moved := cur.TargetID != t.id
			if moved {
				diffs = append(diffs, "source/coordinates (baseline reset)")
				toCheck[cur.TargetID] = true
			}
			if !jsonEqual(cur.Policy, policy) {
				diffs = append(diffs, "policy")
			}
			if !reflect.DeepEqual(nonNilMap(cur.Labels), nonNilMap(d.Labels)) {
				diffs = append(diffs, "labels")
			}
			if len(diffs) == 0 {
				idx = r.add(KindWatch, d.Name, Unchanged, "")
				break
			}
			nw := store.Watch{ID: cur.ID, TargetID: t.id, Policy: policy, Labels: d.Labels, BaselineDone: cur.BaselineDone && !moved}
			if err := r.write(func() error { return r.tx.UpdateWatch(r.ctx, nw) }); err != nil {
				return nil, nil, err
			}
			idx = r.add(KindWatch, d.Name, Update, strings.Join(diffs, ", "))
		}
		t.watches = append(t.watches, idx)
	}

	// A target's interval is the smallest among the file's watches on it.
	for _, k := range order {
		t := targets[k]
		if t.existing == nil || t.existing.Interval == t.interval {
			continue
		}
		next := t.existing.NextCheckAt
		if due := r.now.Add(t.interval); due.Before(next) {
			next = due
		}
		if err := r.write(func() error { return r.tx.SetTargetInterval(r.ctx, t.id, t.interval, next) }); err != nil {
			return nil, nil, err
		}
		for _, i := range t.watches {
			c := &r.plan.Changes[i]
			if c.Action == Unchanged {
				c.Action, c.Detail = Update, "interval "+t.interval.String()
			}
		}
	}
	return toCheck, wanted, nil
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// jsonEqual compares two JSON documents by value.
func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

func (r *reconciler) subscriptions(f *File, old []store.Subscription, sinkIDs map[string]string) error {
	byName := map[string]store.Subscription{}
	for _, s := range old {
		byName[s.Name] = s
	}
	for _, d := range f.Subscriptions {
		sinkID := sinkIDs[d.Sink]
		filter := d.FilterJSON()
		cur, ok := byName[d.Name]
		switch {
		case !ok:
			id := r.newID()
			if err := r.write(func() error {
				return r.tx.CreateSubscription(r.ctx, store.Subscription{
					ID: id, Filter: filter, SinkID: sinkID, Template: d.Template, Name: d.Name, ManagedBy: store.ManagedByFile,
				})
			}); err != nil {
				return err
			}
			r.add(KindSubscription, d.Name, Create, "sink "+d.Sink)
		case cur.SinkID != sinkID || cur.Template != d.Template || !jsonEqual(cur.Filter, filter):
			if err := r.write(func() error {
				return r.tx.UpdateSubscription(r.ctx, store.Subscription{ID: cur.ID, Filter: filter, SinkID: sinkID, Template: d.Template})
			}); err != nil {
				return err
			}
			r.add(KindSubscription, d.Name, Update, "")
		default:
			r.add(KindSubscription, d.Name, Unchanged, "")
		}
	}
	return nil
}
