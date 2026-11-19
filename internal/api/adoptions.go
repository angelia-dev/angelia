package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"

	"github.com/angelia-dev/angelia/internal/store"
)

// adoptedJSON is one AdoptedObject.
type adoptedJSON struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	ID   string `json:"id"`
}

// adoptionResultJSON is the AdoptionResult body.
type adoptionResultJSON struct {
	DryRun  bool          `json:"dry_run"`
	Adopted []adoptedJSON `json:"adopted"`
}

// kindOrder sorts adopted objects: sinks, watches, subscriptions.
var kindOrder = map[string]int{store.KindSink: 0, store.KindWatch: 1, store.KindSubscription: 2}

// AdoptResources implements POST /v1/adoptions (spec docs/api.md item 55):
// watch-file rows become API rows in place, in one transaction, all or
// nothing. A subscription is adopted only with its file sink, and a file
// sink only with every file subscription that uses it, so no row ever
// references a row of another owner (item 4).
func (s *Server) AdoptResources(ctx context.Context, _ AdoptResourcesRequestObject) (AdoptResourcesResponseObject, error) {
	var in struct {
		Objects []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"objects"`
		DryRun bool `json:"dry_run"`
	}
	if err := json.Unmarshal(stateFrom(ctx).body, &in); err != nil {
		return nil, problem(SlugBadRequest, "the request body is not an adoption request")
	}
	want := map[string]bool{}
	for _, o := range in.Objects {
		want[o.Kind+"\x00"+o.Name] = true
	}
	now := s.now()
	res := adoptionResultJSON{DryRun: in.DryRun, Adopted: []adoptedJSON{}}
	err := s.inTx(ctx, in.DryRun, func(tx store.APITx) error {
		res.Adopted = res.Adopted[:0]
		type row struct {
			kind, name, id, sinkID string
			ptr                    string
		}
		var rows []row
		var errs []fieldError
		for i, o := range in.Objects {
			ptr := fmt.Sprintf("/objects/%d", i)
			var id, owner, sinkID string
			var err error
			switch o.Kind {
			case store.KindSink:
				var k store.Sink
				k, err = tx.SinkByName(ctx, o.Name)
				id, owner = k.ID, ownerOf(k)
			case store.KindWatch:
				var w store.Watch
				w, err = tx.WatchByName(ctx, o.Name)
				id, owner = w.ID, ownerOf(w)
			case store.KindSubscription:
				var sub store.Subscription
				sub, err = tx.SubscriptionByName(ctx, o.Name)
				id, owner, sinkID = sub.ID, ownerOf(sub), sub.SinkID
			default:
				errs = append(errs, fieldError{Pointer: ptr + "/kind", Detail: "must be sink, watch or subscription"})
				continue
			}
			switch {
			case isNotFound(err):
				errs = append(errs, fieldError{Pointer: ptr + "/name", Detail: fmt.Sprintf("no %s named %s", o.Kind, o.Name)})
				continue
			case err != nil:
				return err
			case owner != store.ManagedByFile:
				errs = append(errs, fieldError{Pointer: ptr, Detail: fmt.Sprintf("%s %s is not managed by the watch file", o.Kind, o.Name)})
				continue
			}
			rows = append(rows, row{kind: o.Kind, name: o.Name, id: id, sinkID: sinkID, ptr: ptr})
		}
		// Ownership stays consistent across the references subscription -> sink.
		for _, r := range rows {
			switch r.kind {
			case store.KindSubscription:
				k, err := tx.GetSink(ctx, r.sinkID)
				if err != nil {
					return err
				}
				if ownerOf(k) == store.ManagedByFile && !want[store.KindSink+"\x00"+k.Name] {
					errs = append(errs, fieldError{Pointer: r.ptr, Detail: fmt.Sprintf("adopt its sink %s too: a subscription managed by the API cannot use a sink of the watch file", k.Name)})
				}
			case store.KindSink:
				subs, err := tx.SubscriptionsBySink(ctx, r.id)
				if err != nil {
					return err
				}
				for _, sub := range subs {
					if ownerOf(sub) == store.ManagedByFile && !want[store.KindSubscription+"\x00"+sub.Name] {
						errs = append(errs, fieldError{Pointer: r.ptr, Detail: fmt.Sprintf("adopt the subscription %s too: it uses this sink and is managed by the watch file", sub.Name)})
					}
				}
			}
		}
		if len(errs) > 0 {
			return validationProblem("adoption request", errs)
		}
		for _, r := range rows {
			if err := tx.AdoptRow(ctx, r.kind, r.id, r.name, now); err != nil {
				return err
			}
			res.Adopted = append(res.Adopted, adoptedJSON{Kind: r.kind, Name: r.name, ID: r.id})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(res.Adopted, func(a, b adoptedJSON) int {
		if d := kindOrder[a.Kind] - kindOrder[b.Kind]; d != 0 {
			return d
		}
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		}
		return 0
	})
	if !in.DryRun {
		names := make([]string, len(res.Adopted))
		for i, a := range res.Adopted {
			names[i] = a.Kind + "/" + a.Name
		}
		s.audit(ctx, "adopt", slog.Any("objects", names))
	}
	return jsonOK(res), nil
}
