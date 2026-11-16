package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"time"

	"github.com/angelia-dev/angelia/internal/sink"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/ulid"
)

// maxLastError bounds Delivery.last_error (1 KiB).
const maxLastError = 1024

// replayListMax is how many deliveries a bulk replay lists.
const replayListMax = 100

type deliveryReader interface {
	GetSubscription(ctx context.Context, id string) (store.Subscription, error)
	GetSink(ctx context.Context, id string) (store.Sink, error)
}

// deliveryNames are the current names of a delivery's subscription and of
// the sink it delivers to, nil once the subscription was deleted (or for a
// row without a name).
type deliveryNames struct{ subscription, sink *string }

// namesCache caches deliveryNames by subscription id.
type namesCache map[string]deliveryNames

// namesOf returns the current names for a subscription id.
func (c namesCache) namesOf(ctx context.Context, r deliveryReader, subID string) (deliveryNames, error) {
	if n, ok := c[subID]; ok {
		return n, nil
	}
	var n deliveryNames
	sub, err := r.GetSubscription(ctx, subID)
	switch {
	case isNotFound(err):
	case err != nil:
		return n, err
	default:
		n.subscription = orNull(sub.Name)
		k, err := r.GetSink(ctx, sub.SinkID)
		switch {
		case isNotFound(err):
		case err != nil:
			return n, err
		default:
			n.sink = orNull(k.Name)
		}
	}
	c[subID] = n
	return n, nil
}

// deliveryJSONOf builds the response of a delivery. names caches the current
// names of each subscription and its sink.
func deliveryJSONOf(ctx context.Context, r deliveryReader, d store.Delivery, now time.Time, names namesCache) (deliveryJSON, error) {
	n, err := names.namesOf(ctx, r, d.SubscriptionID)
	if err != nil {
		return deliveryJSON{}, err
	}
	out := deliveryJSON{
		ID: d.ID, EventID: d.EventID, SubscriptionID: d.SubscriptionID, SubscriptionName: n.subscription, Sink: n.sink,
		IdempotencyKey: d.EventID + ":" + d.SubscriptionID, Status: d.Status, Attempts: d.Attempts,
	}
	if d.Status == store.DeliveryPending {
		out.NextAttemptAt = ts(d.NextAttemptAt)
		if d.Hold != nil && d.Hold.Until.After(now) {
			out.HeldUntil = ts(d.Hold.Until)
		}
	}
	if d.LastError != "" {
		// The dispatcher stores errors with URLs cut already; cut again in
		// case a row predates that, and bound the size.
		msg := sink.RedactURL(errorString(d.LastError))
		if len(msg) > maxLastError {
			msg = truncateUTF8(msg, maxLastError)
		}
		out.LastError = &msg
	}
	return out, nil
}

type errorString string

func (e errorString) Error() string { return string(e) }

// truncateUTF8 cuts s to at most n bytes without splitting a character.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// GetDelivery implements StrictServerInterface.
func (s *Server) GetDelivery(ctx context.Context, req GetDeliveryRequestObject) (GetDeliveryResponseObject, error) {
	d, err := s.opts.Store.GetDelivery(ctx, req.Id)
	if err != nil {
		return nil, storeProblem(err, "delivery", req.Id)
	}
	body, err := deliveryJSONOf(ctx, s.opts.Store, d, s.now(), namesCache{})
	if err != nil {
		return nil, err
	}
	return jsonOK(body), nil
}

// ListDeliveries implements StrictServerInterface.
func (s *Server) ListDeliveries(ctx context.Context, req ListDeliveriesRequestObject) (ListDeliveriesResponseObject, error) {
	cur, p := decodeCursor(req.Params.Cursor, cursorDeliveries)
	if p != nil {
		return nil, p
	}
	limit := pageLimit(req.Params.Limit)
	q := store.DeliveryQuery{BeforeSeq: cur.Seq, BeforeID: cur.I, Limit: limit + 1}
	for _, st := range deref(req.Params.Status) {
		q.Statuses = append(q.Statuses, string(st))
	}
	if req.Params.Event != nil {
		q.EventID = *req.Params.Event
	}
	if req.Params.Subscription != nil {
		ids, err := s.subscriptionIDs(ctx, *req.Params.Subscription)
		if err != nil {
			return nil, err
		}
		q.SubscriptionIDs = ids
	}
	if req.Params.Sink != nil {
		ids, err := s.sinkSubscriptionIDs(ctx, *req.Params.Sink)
		if err != nil {
			return nil, err
		}
		if q.SubscriptionIDs != nil { // both filters: both must hold
			ids = slices.DeleteFunc(ids, func(id string) bool { return !slices.Contains(q.SubscriptionIDs, id) })
		}
		q.SubscriptionIDs = ids
	}
	if p := deliveryWindow(&q, req.Params.Since, req.Params.Until); p != nil {
		return nil, p
	}
	rows := []store.DeliveryRow{}
	if q.SubscriptionIDs == nil || len(q.SubscriptionIDs) > 0 {
		var err error
		if rows, err = s.opts.Store.ListDeliveryPage(ctx, q); err != nil {
			return nil, err
		}
	}
	page := pageJSON[deliveryJSON]{Items: make([]deliveryJSON, 0, min(len(rows), limit))}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1]
		page.NextCursor = encodeCursor(pageCursor{K: cursorDeliveries, Seq: last.EventSeq, I: last.ID})
	}
	now, names := s.now(), namesCache{}
	for _, r := range rows {
		dj, err := deliveryJSONOf(ctx, s.opts.Store, r.Delivery, now, names)
		if err != nil {
			return nil, err
		}
		page.Items = append(page.Items, dj)
	}
	return jsonOK(page), nil
}

// sinkSubscriptionIDs returns the ids of the subscriptions that deliver to
// the sink named name now; an unknown sink gives an empty, non-nil list.
func (s *Server) sinkSubscriptionIDs(ctx context.Context, name string) ([]string, error) {
	k, err := s.opts.Store.SinkByName(ctx, name)
	switch {
	case isNotFound(err):
		return []string{}, nil
	case err != nil:
		return nil, err
	}
	subs, err := s.opts.Store.SubscriptionsBySink(ctx, k.ID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(subs))
	for _, sub := range subs {
		ids = append(ids, sub.ID)
	}
	return ids, nil
}

// deliveryWindow sets the event-time window of a delivery query from since
// (inclusive) and until (exclusive), already checked as timestamps; until
// must be after since (spec docs/api.md decision 48).
func deliveryWindow(q *store.DeliveryQuery, since, until *Timestamp) *ProblemDetail {
	parse := func(v *Timestamp) time.Time {
		if v == nil {
			return time.Time{}
		}
		return v.UTC()
	}
	q.Since, q.Until = parse(since), parse(until)
	if since != nil && until != nil && !q.Until.After(q.Since) {
		return problem(SlugBadRequest, "query parameter until: must be after since")
	}
	return nil
}

// subscriptionIDs resolves a subscription given by name (its current id) or
// by id (also a deleted one). An unknown name gives an empty, non-nil list.
func (s *Server) subscriptionIDs(ctx context.Context, nameOrID string) ([]string, error) {
	if ulid.Valid(nameOrID) {
		return []string{nameOrID}, nil
	}
	sub, err := s.opts.Store.SubscriptionByName(ctx, nameOrID)
	switch {
	case isNotFound(err):
		return []string{}, nil
	case err != nil:
		return nil, err
	}
	return []string{sub.ID}, nil
}

// replayable reports why a delivery cannot be replayed, or "".
func replayable(ctx context.Context, tx store.APITx, d store.Delivery) (string, error) {
	switch d.Status {
	case store.DeliveryDead, store.DeliveryDelivered:
	default:
		return "delivery " + d.ID + " is " + d.Status + "; only dead or delivered deliveries can be replayed", nil
	}
	sub, err := tx.GetSubscription(ctx, d.SubscriptionID)
	if isNotFound(err) {
		return "delivery " + d.ID + ": its subscription no longer exists", nil
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.GetSink(ctx, sub.SinkID); isNotFound(err) {
		return "delivery " + d.ID + ": its sink no longer exists", nil
	} else if err != nil {
		return "", err
	}
	return "", nil
}

// ReplayDelivery implements StrictServerInterface. The replay keeps attempts
// and starts a new retry budget (store.Delivery.ReplayBaseAttempts, spec
// docs/api.md decision 28).
func (s *Server) ReplayDelivery(ctx context.Context, req ReplayDeliveryRequestObject) (ReplayDeliveryResponseObject, error) {
	now := s.now()
	var body deliveryJSON
	var from string
	err := s.opts.Store.InAPITx(ctx, func(tx store.APITx) error {
		d, err := tx.GetDelivery(ctx, req.Id)
		if err != nil {
			return storeProblem(err, "delivery", req.Id)
		}
		why, err := replayable(ctx, tx, d)
		if err != nil {
			return err
		}
		if why != "" {
			return problem(SlugNotReplayable, "%s", why)
		}
		from = d.Status
		if err := tx.ReplayDelivery(ctx, d.ID, now); err != nil {
			return err
		}
		if d, err = tx.GetDelivery(ctx, d.ID); err != nil {
			return err
		}
		body, err = deliveryJSONOf(ctx, tx, d, now, namesCache{})
		return err
	})
	if err != nil {
		return nil, err
	}
	s.audit(ctx, "replay", slog.String("delivery", req.Id), slog.Int("deliveries", 1),
		slog.Any("ids", []string{req.Id}), slog.Any("from_status", map[string]int{from: 1}))
	return jsonOK(body), nil
}

// CreateReplay implements StrictServerInterface.
func (s *Server) CreateReplay(ctx context.Context, _ CreateReplayRequestObject) (CreateReplayResponseObject, error) {
	var in struct {
		Event        *string   `json:"event"`
		Subscription *string   `json:"subscription"`
		Since        *string   `json:"since"`
		Until        *string   `json:"until"`
		Status       []string  `json:"status"`
		DryRun       bool      `json:"dry_run"`
		since, until time.Time `json:"-"`
	}
	if err := json.Unmarshal(stateFrom(ctx).body, &in); err != nil {
		return nil, problem(SlugBadRequest, "the request body is not a replay request")
	}
	var errs []fieldError
	if (in.Since != nil || in.Until != nil) && (in.Subscription == nil || in.Since == nil || in.Until == nil) {
		errs = append(errs, fieldError{Pointer: "", Detail: "since and until go together, with subscription"})
	}
	if in.Subscription != nil && in.Since == nil && in.Event == nil {
		errs = append(errs, fieldError{Pointer: "", Detail: "subscription needs since and until, or event"})
	}
	if in.Since != nil && in.Until != nil {
		in.since, _ = time.Parse(time.RFC3339Nano, *in.Since)
		in.until, _ = time.Parse(time.RFC3339Nano, *in.Until)
		if !in.until.After(in.since) {
			errs = append(errs, fieldError{Pointer: "/until", Detail: "must be after since"})
		}
	}
	q := store.ReplayQuery{Statuses: in.Status}
	if len(q.Statuses) == 0 {
		q.Statuses = []string{store.DeliveryDead}
	}
	if in.Event != nil {
		q.EventID = *in.Event
	}
	if in.Subscription != nil && len(errs) == 0 {
		ids, err := s.subscriptionIDs(ctx, *in.Subscription)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			errs = append(errs, fieldError{Pointer: "/subscription", Detail: "no subscription named " + *in.Subscription})
		} else {
			q.SubscriptionID = ids[0]
		}
	}
	if len(errs) > 0 {
		return nil, validationProblem("replay request", errs)
	}
	now := s.now()
	res := replayResultJSON{DryRun: in.DryRun, Deliveries: []deliveryJSON{}}
	ids, from := []string{}, map[string]int{}
	err := s.inTx(ctx, in.DryRun, func(tx store.APITx) error {
		rows, err := tx.ListReplayCandidates(ctx, q)
		if err != nil {
			return err
		}
		names := namesCache{}
		for _, r := range rows {
			if !in.since.IsZero() && !eventTimeIn(r.EventPayload, in.since, in.until) {
				continue
			}
			why, err := replayable(ctx, tx, r.Delivery)
			if err != nil {
				return err
			}
			if why != "" {
				continue // its subscription or sink is gone
			}
			if err := tx.ReplayDelivery(ctx, r.ID, now); err != nil {
				return err
			}
			res.Replayed++
			ids = append(ids, r.ID)
			from[r.Status]++
			if len(res.Deliveries) < replayListMax {
				d, err := tx.GetDelivery(ctx, r.ID)
				if err != nil {
					return err
				}
				dj, err := deliveryJSONOf(ctx, tx, d, now, names)
				if err != nil {
					return err
				}
				res.Deliveries = append(res.Deliveries, dj)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	res.Truncated = res.Replayed > len(res.Deliveries)
	if !in.DryRun {
		attrs := replaySelector(in.Event, in.Subscription, in.Since, in.Until, q.Statuses)
		if q.SubscriptionID != "" {
			attrs = append(attrs, slog.String("subscription_id", q.SubscriptionID))
		}
		attrs = append(attrs, slog.Int("deliveries", len(ids)), slog.Any("ids", firstN(ids, replayListMax)),
			slog.Any("from_status", from))
		s.audit(ctx, "replay", attrs...)
	}
	return jsonOK(res), nil
}

// replaySelector is what a bulk replay asked for, for its audit record: the
// members given, as given, and the statuses it selected.
func replaySelector(event, subscription, since, until *string, statuses []string) []slog.Attr {
	var attrs []slog.Attr
	for _, f := range []struct {
		key string
		val *string
	}{{"event", event}, {"subscription", subscription}, {"since", since}, {"until", until}} {
		if f.val != nil {
			attrs = append(attrs, slog.String(f.key, *f.val))
		}
	}
	return append(attrs, slog.Any("status", statuses))
}

// eventTimeIn reports whether the CloudEvents time of a stored envelope is in
// [since, until).
func eventTimeIn(payload []byte, since, until time.Time) bool {
	var env struct {
		Time string `json:"time"`
	}
	if json.Unmarshal(payload, &env) != nil {
		return false
	}
	t, err := time.Parse(time.RFC3339Nano, env.Time)
	return err == nil && !t.Before(since) && t.Before(until)
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// audit records an action in the log, with who did it (spec: "The replay is
// recorded in the audit log").
func (s *Server) audit(ctx context.Context, action string, attrs ...slog.Attr) {
	p := principalFrom(ctx)
	base := []slog.Attr{
		slog.Bool("audit", true), slog.String("action", action),
		slog.String("principal", p.ID), slog.String("request_id", stateFrom(ctx).id),
	}
	if p.Name != "" {
		base = append(base, slog.String("principal_name", p.Name))
	}
	s.log.LogAttrs(ctx, slog.LevelInfo, "api audit", append(base, attrs...)...)
}
