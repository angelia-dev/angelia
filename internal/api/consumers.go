package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strconv"

	"github.com/angelia-dev/angelia/internal/store"
)

// Consumers: named server-side event cursors, moved only by an ack (spec
// docs/api.md decision 16, core #79).

// parseConsumerFilter reads a stored or written filter. Every key is
// present in the result; an absent key is an empty list (matches every
// event).
func parseConsumerFilter(raw []byte) consumerFilterJSON {
	var f consumerFilterJSON
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &f) // validated against the spec when written
	}
	f.Watch, f.Type, f.Source = nonNil(f.Watch), nonNil(f.Type), nonNil(f.Source)
	return f
}

func (f consumerFilterJSON) equal(g consumerFilterJSON) bool {
	return slices.Equal(f.Watch, g.Watch) && slices.Equal(f.Type, g.Type) && slices.Equal(f.Source, g.Source)
}

func consumerJSONOf(c store.Consumer) consumerJSON {
	return consumerJSON{
		Name: c.Name, ID: c.ID, Filter: parseConsumerFilter(c.Filter), AckedSeq: c.AckedSeq,
		AckedAt: ts(c.AckedAt), CreatedAt: timestamp(c.CreatedAt), UpdatedAt: timestamp(c.UpdatedAt),
	}
}

// readConsumer returns the consumer of a read: 404 when there is none.
func (s *Server) readConsumer(ctx context.Context, name string) (store.Consumer, error) {
	c, err := s.opts.Store.ConsumerByName(ctx, name)
	return readByName(c, err, "consumer", name)
}

// consumerCheck refuses a consumer whose cursor points into pruned events:
// 410 cursor-expired, also at acked_seq 0, unlike after=0 (spec docs/api.md
// decision 16, D4).
func (s *Server) consumerCheck(ctx context.Context, c store.Consumer) error {
	return s.expiredCheck(ctx, c.AckedSeq,
		"consumer "+c.Name+" is at "+strconv.FormatInt(c.AckedSeq, 10)+", but events after "+
			strconv.FormatInt(c.AckedSeq, 10)+" were pruned; acknowledge oldest_seq - 1 to resume")
}

// consumerFilter resolves the stored filter of a consumer as the query
// parameters would be: watch names now.
func (s *Server) consumerFilter(ctx context.Context, c store.Consumer) (*eventFilter, error) {
	f := parseConsumerFilter(c.Filter)
	types := make([]ShortEventType, len(f.Type))
	for i, t := range f.Type {
		types[i] = ShortEventType(t)
	}
	return s.newEventFilter(ctx, f.Watch, types, f.Source)
}

// GetConsumer implements StrictServerInterface.
func (s *Server) GetConsumer(ctx context.Context, req GetConsumerRequestObject) (GetConsumerResponseObject, error) {
	c, err := s.readConsumer(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	return jsonOK(consumerJSONOf(c)), nil
}

// ListConsumers implements StrictServerInterface.
func (s *Server) ListConsumers(ctx context.Context, req ListConsumersRequestObject) (ListConsumersResponseObject, error) {
	cur, p := decodeCursor(req.Params.Cursor, cursorConsumers)
	if p != nil {
		return nil, p
	}
	items, next, err := pageByName(cur, pageLimit(req.Params.Limit),
		func(afterName, afterID string, n int) ([]store.Consumer, error) {
			return s.opts.Store.ListConsumers(ctx, afterName, afterID, n)
		},
		func(c store.Consumer) (string, string) { return c.Name, c.ID },
		func(store.Consumer) (bool, error) { return true, nil })
	if err != nil {
		return nil, err
	}
	page := pageJSON[consumerJSON]{Items: make([]consumerJSON, 0, len(items)), NextCursor: next}
	for _, c := range items {
		page.Items = append(page.Items, consumerJSONOf(c))
	}
	return jsonOK(page), nil
}

// PutConsumer implements StrictServerInterface. The body replaces the
// filter; start applies on create only, and no PUT moves acked_seq.
func (s *Server) PutConsumer(ctx context.Context, req PutConsumerRequestObject) (PutConsumerResponseObject, error) {
	raw := stateFrom(ctx).body
	if errs := bodyNameErrors(raw, req.Name); len(errs) > 0 {
		return nil, validationProblem("consumer", errs)
	}
	var w struct {
		Filter json.RawMessage `json:"filter"`
		Start  string          `json:"start"`
	}
	_ = json.Unmarshal(raw, &w) // validated against the spec
	filter := parseConsumerFilter(w.Filter)
	filterJSON := mustJSON(filter)
	dry := req.Params.DryRun != nil && *req.Params.DryRun
	var row store.Consumer
	var change Change
	err := s.inTx(ctx, dry, func(tx store.APITx) error {
		cur, err := tx.ConsumerByName(ctx, req.Name)
		switch {
		case isNotFound(err):
			// The start position is read in the create transaction, so a
			// prune cannot slip in between (spec D5).
			var start int64
			if w.Start == string(Earliest) {
				start, err = tx.HighestPrunedSeq(ctx)
			} else {
				start, err = tx.MaxEventSeq(ctx)
			}
			if err != nil {
				return err
			}
			now := s.now()
			row = store.Consumer{
				ID: s.opts.NewID(), Name: req.Name, Filter: filterJSON, AckedSeq: start,
				CreatedAt: now, UpdatedAt: now,
			}
			change = ChangeCreated
			return tx.CreateConsumer(ctx, row)
		case err != nil:
			return err
		case !parseConsumerFilter(cur.Filter).equal(filter):
			row = cur
			row.Filter, row.UpdatedAt = filterJSON, s.now()
			change = ChangeUpdated
			return tx.UpdateConsumer(ctx, row)
		default:
			row, change = cur, ChangeUnchanged
			return nil
		}
	})
	if err != nil {
		return nil, err
	}
	body := consumerJSONOf(row)
	if !dry && change != ChangeUnchanged {
		action := "consumer.create"
		if change == ChangeUpdated {
			action = "consumer.update"
		}
		s.audit(ctx, action, slog.String("consumer", row.Name), slog.String("id", row.ID),
			slog.Int64("acked_seq", row.AckedSeq))
	}
	return putResponse(change, location("consumers", req.Name), body), nil
}

// DeleteConsumer implements StrictServerInterface.
func (s *Server) DeleteConsumer(ctx context.Context, req DeleteConsumerRequestObject) (DeleteConsumerResponseObject, error) {
	dry := req.Params.DryRun != nil && *req.Params.DryRun
	var id string
	err := s.inTx(ctx, dry, func(tx store.APITx) error {
		c, err := tx.ConsumerByName(ctx, req.Name)
		if c, err = readByName(c, err, "consumer", req.Name); err != nil {
			return err
		}
		id = c.ID
		return tx.DeleteConsumer(ctx, c.ID)
	})
	if err != nil {
		return nil, err
	}
	if !dry {
		s.audit(ctx, "consumer.delete", slog.String("consumer", req.Name), slog.String("id", id))
	}
	return deleteResponse(), nil
}

// AckConsumer implements StrictServerInterface: a monotonic cursor commit.
// It is not audited (frequent); the access log records it.
func (s *Server) AckConsumer(ctx context.Context, req AckConsumerRequestObject) (AckConsumerResponseObject, error) {
	if _, err := s.readConsumer(ctx, req.Name); err != nil {
		return nil, err // 404 before the body's rules
	}
	body := rawObject(stateFrom(ctx).body)
	var seq int64
	if n, ok := body["seq"].(json.Number); ok {
		seq, _ = intValue(n) // the spec validated it: an integer >= 0
	} else {
		id, _ := body["event_id"].(string) // the spec requires one of the two
		ev, err := s.opts.Store.GetEvent(ctx, id)
		switch {
		case isNotFound(err):
			return nil, validationProblem("ack", []fieldError{{Pointer: "/event_id", Detail: "no event with this id in the log"}})
		case err != nil:
			return nil, err
		}
		seq = ev.Seq
	}
	var c store.Consumer
	err := s.inTx(ctx, false, func(tx store.APITx) error {
		head, err := tx.MaxEventSeq(ctx)
		if err != nil {
			return err
		}
		if seq > head {
			return validationProblem("ack", []fieldError{{
				Pointer: "/seq", Detail: "is beyond the last event committed (" + strconv.FormatInt(head, 10) + ")",
			}})
		}
		c, err = tx.AckConsumer(ctx, req.Name, seq, s.now())
		if isNotFound(err) { // deleted since the read above
			return problem(SlugNotFound, "no consumer named %s", req.Name)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return jsonOK(consumerJSONOf(c)), nil
}
