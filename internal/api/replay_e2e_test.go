package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/dispatch"
	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/sink"
	"github.com/angelia-dev/angelia/internal/store"
)

// recordingSink stands in for the webhook sink and reports every attempt.
type recordingSink struct{ sent chan sink.Request }

func (recordingSink) Kind() string               { return "webhook" }
func (recordingSink) Validate(sink.Config) error { return nil }
func (s recordingSink) Deliver(_ context.Context, req sink.Request) (sink.Result, error) {
	s.sent <- req
	return sink.Result{StatusCode: 204}, nil
}

// A delivery dead after its last attempt is sent again once replayed through
// the API: the replay restarts the retry budget (spec docs/api.md decision
// 28), so the dispatcher's lease is not taken for an exhausted delivery.
func TestReplayExhaustedIsSent(t *testing.T) {
	e := newEnv(t)
	e.seedConfig()
	sid, wid := e.subscriptionID("platform-ci"), e.watchID("requests")
	ev := e.appendEvent(wid, event.TypePublished, "pypi", e.now, store.Delivery{
		SubscriptionID: sid, Status: store.DeliveryDead, Attempts: dispatch.DefaultMaxAttempts, LastError: "giving up after 8 attempts: 503",
	})
	id := e.deliveryOf(ev)["id"].(string)
	e.must(http.StatusOK, e.do("POST", "/v1/deliveries/"+id+"/replay", tokWrite, nil))

	reg := sink.NewRegistry()
	rs := recordingSink{sent: make(chan sink.Request, 4)}
	if err := reg.Register(rs); err != nil {
		t.Fatal(err)
	}
	d, err := dispatch.New(e.st, reg, dispatch.Options{
		PollInterval: 10 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	select {
	case req := <-rs.sent:
		if req.Attempt != dispatch.DefaultMaxAttempts+1 {
			t.Errorf("attempt %d", req.Attempt)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the replayed delivery was not sent")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	m := e.deliveryOf(ev)
	if m["status"] != "delivered" || m["attempts"] != float64(dispatch.DefaultMaxAttempts+1) {
		t.Errorf("after the send: %v", m)
	}
}
