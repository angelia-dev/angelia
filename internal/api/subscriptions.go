package api

import (
	"context"
	"slices"

	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/subscription"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

type subscriptionReader interface {
	GetSink(ctx context.Context, id string) (store.Sink, error)
}

// subscriptionJSONOf builds the response of a subscription.
func subscriptionJSONOf(ctx context.Context, r subscriptionReader, sub store.Subscription) (subscriptionJSON, error) {
	f, err := subscription.FromJSON(sub.Filter)
	if err != nil {
		return subscriptionJSON{}, err
	}
	pol, err := subscription.PolicyFromJSON(sub.Policy)
	if err != nil {
		return subscriptionJSON{}, err
	}
	k, err := r.GetSink(ctx, sub.SinkID)
	if err != nil {
		return subscriptionJSON{}, err
	}
	var pre *string
	if pol.Prerelease != "" {
		pre = &pol.Prerelease
	}
	labels := f.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	types := f.Type
	if len(types) == 0 {
		types = []string{"release.published"}
	}
	return subscriptionJSON{
		Name: sub.Name, ID: sub.ID, ManagedBy: ownerOf(sub), Sink: k.Name,
		Filter: subscriptionFilterJSON{
			Source: nonNil(f.Source), Labels: labels, Bump: nonNil(f.Bump), Package: f.Package, Type: types,
		},
		Policy:   subscriptionPolicyJSON{Prerelease: pre, Cooldown: formatDuration(pol.Cooldown)},
		Template: sub.Template,
	}, nil
}

// GetSubscription implements StrictServerInterface.
func (s *Server) GetSubscription(ctx context.Context, req GetSubscriptionRequestObject) (GetSubscriptionResponseObject, error) {
	sub, err := s.opts.Store.SubscriptionByName(ctx, req.Name)
	if sub, err = readByName(sub, err, "subscription", req.Name); err != nil {
		return nil, err
	}
	body, err := subscriptionJSONOf(ctx, s.opts.Store, sub)
	if err != nil {
		return nil, err
	}
	return jsonOK(body), nil
}

// ListSubscriptions implements StrictServerInterface.
func (s *Server) ListSubscriptions(ctx context.Context, req ListSubscriptionsRequestObject) (ListSubscriptionsResponseObject, error) {
	cur, p := decodeCursor(req.Params.Cursor, cursorSubscriptions)
	if p != nil {
		return nil, p
	}
	var sinkIDs []string
	if req.Params.Sink != nil {
		k, err := s.opts.Store.SinkByName(ctx, *req.Params.Sink)
		switch {
		case isNotFound(err):
			sinkIDs = []string{} // a sink that does not exist matches nothing
		case err != nil:
			return nil, err
		default:
			sinkIDs = []string{k.ID}
		}
	}
	items, next, err := pageByName(cur, pageLimit(req.Params.Limit),
		func(afterName, afterID string, n int) ([]store.Subscription, error) {
			return s.opts.Store.ListNamedSubscriptions(ctx, afterName, afterID, n)
		},
		func(sub store.Subscription) (string, string) { return sub.Name, sub.ID },
		func(sub store.Subscription) (bool, error) {
			return sinkIDs == nil || slices.Contains(sinkIDs, sub.SinkID), nil
		})
	if err != nil {
		return nil, err
	}
	page := pageJSON[subscriptionJSON]{Items: make([]subscriptionJSON, 0, len(items)), NextCursor: next}
	for _, sub := range items {
		sj, err := subscriptionJSONOf(ctx, s.opts.Store, sub)
		if err != nil {
			return nil, err
		}
		page.Items = append(page.Items, sj)
	}
	return jsonOK(page), nil
}

// parseSubscriptionBody validates a PUT body by the rules of the watch file.
// Whether its sink exists is checked in the write transaction.
func (s *Server) parseSubscriptionBody(name string, raw []byte) (watchfile.Subscription, []fieldError) {
	errs := bodyNameErrors(raw, name)
	data := raw
	// A null prerelease inherits the watch's mode: the watch file says so by
	// leaving the key out.
	if obj := rawObject(data); obj != nil {
		if pol, ok := obj["policy"].(map[string]any); ok {
			if v, ok := pol["prerelease"]; ok && v == nil {
				delete(pol, "prerelease")
				data = mustJSON(obj)
			}
		}
	}
	sub, perrs := watchfile.ParseSubscription(data, name, s.parseOptions())
	return sub, append(errs, parseErrors(perrs, "filter.labels")...)
}

// PutSubscription implements StrictServerInterface.
func (s *Server) PutSubscription(ctx context.Context, req PutSubscriptionRequestObject) (PutSubscriptionResponseObject, error) {
	sub, errs := s.parseSubscriptionBody(req.Name, stateFrom(ctx).body)
	if len(errs) > 0 {
		return nil, validationProblem("subscription", errs)
	}
	dry := req.Params.DryRun != nil && *req.Params.DryRun
	var body subscriptionJSON
	var change Change
	err := s.inTx(ctx, dry, func(tx store.APITx) error {
		old, err := tx.SubscriptionByName(ctx, sub.Name)
		cur, err := writeByName(old, err, "subscription", sub.Name)
		if err != nil {
			return err
		}
		before, err := subscriptionBody(ctx, tx, cur)
		if err != nil {
			return err
		}
		after, _ := decodeJSON(stateFrom(ctx).body)
		if err := checkRefScope(ctx, before, after); err != nil {
			return err
		}
		sinkID, p, err := s.subscriptionSink(ctx, tx, sub.Sink)
		if p != nil || err != nil {
			return firstErr(p, err)
		}
		filter, pol := sub.FilterJSON(), sub.PolicyJSON()
		row := store.Subscription{Filter: filter, Policy: pol, SinkID: sinkID, Template: sub.Template, Name: sub.Name, ManagedBy: store.ManagedByAPI}
		switch {
		case cur == nil:
			row.ID = s.opts.NewID()
			if err := tx.CreateSubscription(ctx, row); err != nil {
				return err
			}
			change = ChangeCreated
		case cur.SinkID != sinkID || cur.Template != sub.Template || !jsonEqual(cur.Filter, filter) || !samePolicy(cur.Policy, sub.Policy):
			row.ID = cur.ID
			if err := tx.UpdateSubscription(ctx, row); err != nil {
				return err
			}
			if !jsonEqual(cur.Filter, filter) || !samePolicy(cur.Policy, sub.Policy) {
				f, err := subscription.FromJSON(filter)
				if err != nil {
					return err
				}
				if _, err := watchfile.ReevaluateHeld(ctx, tx, cur.ID, f, sub.Policy, s.now()); err != nil {
					return err
				}
			}
			change = ChangeUpdated
		default:
			row = *cur
			change = ChangeUnchanged
		}
		body, err = subscriptionJSONOf(ctx, tx, row)
		return err
	})
	if err != nil {
		return nil, err
	}
	return putResponse(change, location("subscriptions", sub.Name), body), nil
}

// subscriptionBody is cur in the shape of its PUT body, nil when cur is
// nil, for checkRefScope.
func subscriptionBody(ctx context.Context, tx store.APITx, cur *store.Subscription) (any, error) {
	if cur == nil {
		return nil, nil
	}
	sj, err := subscriptionJSONOf(ctx, tx, *cur)
	if err != nil {
		return nil, err
	}
	return asBody(sj), nil
}

func firstErr(p *ProblemDetail, err error) error {
	if p != nil {
		return p
	}
	return err
}

// subscriptionSink resolves the sink an API subscription names: an API sink,
// never one from the watch file (spec docs/api.md decision 4).
func (s *Server) subscriptionSink(ctx context.Context, tx store.APITx, name string) (string, *ProblemDetail, error) {
	k, err := tx.SinkByName(ctx, name)
	switch {
	case isNotFound(err):
		return "", validationProblem("subscription", []fieldError{{Pointer: "/sink", Detail: "no sink named " + name}}), nil
	case err != nil:
		return "", nil, err
	case ownerOf(k) != store.ManagedByAPI:
		return "", problem(SlugCrossOwnerReference, "sink %s is managed by the watch file; an API subscription cannot use it", name), nil
	}
	return k.ID, nil, nil
}

// samePolicy compares a stored subscription policy with a parsed one.
func samePolicy(stored []byte, p subscription.Policy) bool {
	cur, err := subscription.PolicyFromJSON(stored)
	return err == nil && cur == p.Clamp()
}

// DeleteSubscription implements StrictServerInterface.
func (s *Server) DeleteSubscription(ctx context.Context, req DeleteSubscriptionRequestObject) (DeleteSubscriptionResponseObject, error) {
	dry := req.Params.DryRun != nil && *req.Params.DryRun
	err := s.inTx(ctx, dry, func(tx store.APITx) error {
		old, err := tx.SubscriptionByName(ctx, req.Name)
		cur, err := writeByName(old, err, "subscription", req.Name)
		if err != nil {
			return err
		}
		if cur == nil {
			return problem(SlugNotFound, "no subscription named %s", req.Name)
		}
		before, err := subscriptionBody(ctx, tx, cur)
		if err != nil {
			return err
		}
		if err := checkRefScope(ctx, before, nil); err != nil {
			return err
		}
		// Its pending deliveries, held or not, become dead; in-flight ones
		// finish (spec, deleteSubscription; docs/watch-file.md D8).
		if _, err := tx.FailPendingDeliveries(ctx, cur.ID, watchfile.DeadOnDelete); err != nil {
			return err
		}
		return tx.DeleteSubscription(ctx, cur.ID)
	})
	if err != nil {
		return nil, err
	}
	return deleteResponse(), nil
}
