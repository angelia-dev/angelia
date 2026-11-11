package storetest

import (
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

func testCountDeliveriesByStatus(t *testing.T, open opener) {
	s := open(t)
	ctx := t.Context()
	got, err := s.CountDeliveriesByStatus(ctx)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty store: %v, %v; want an empty map", got, err)
	}
	seedDeliveries(t, s)
	if got, err = s.CountDeliveriesByStatus(ctx); err != nil || got[store.DeliveryPending] != 2 || len(got) != 1 {
		t.Fatalf("two pending: %v, %v", got, err)
	}
	leased, err := s.LeaseDueDeliveries(ctx, "a", t0, t0.Add(time.Minute), 1)
	must(t, err)
	if len(leased) != 1 {
		t.Fatalf("leased %d deliveries, want 1", len(leased))
	}
	if got, err = s.CountDeliveriesByStatus(ctx); err != nil || got[store.DeliveryPending] != 1 || got[store.DeliveryInFlight] != 1 {
		t.Fatalf("one in flight: %v, %v", got, err)
	}
	ok, err := s.FinishDelivery(ctx, store.DeliveryFinish{ID: leased[0].Delivery.ID, Attempt: 1, Status: store.DeliveryDead, LastError: "boom"}, "a")
	if err != nil || !ok {
		t.Fatalf("FinishDelivery = %v, %v", ok, err)
	}
	if got, err = s.CountDeliveriesByStatus(ctx); err != nil || got[store.DeliveryDead] != 1 || got[store.DeliveryPending] != 1 || got[store.DeliveryInFlight] != 0 {
		t.Fatalf("one dead: %v, %v", got, err)
	}
}
