package metrics

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/angelia-dev/angelia/internal/store"
)

const (
	// deliveryCountTTL is how long a count of the delivery table is reused,
	// so that frequent scrapes do not each scan it.
	deliveryCountTTL = 10 * time.Second
	// deliveryCountTimeout bounds one count.
	deliveryCountTimeout = 5 * time.Second
)

// DeliveryCounter is the part of store.Store that RegisterDeliveryCounts
// uses.
type DeliveryCounter interface {
	CountDeliveriesByStatus(ctx context.Context) (map[string]int64, error)
}

// RegisterDeliveryCounts registers angelia_deliveries and angelia_dlq_size,
// read from the store at scrape time (at most once per 10 s). Call it once,
// when the process has a store.
func (m *Metrics) RegisterDeliveryCounts(c DeliveryCounter) {
	m.reg.MustRegister(&deliveryCollector{src: c, now: time.Now, log: m.log})
}

var (
	deliveriesDesc = prometheus.NewDesc("angelia_deliveries",
		"Delivery rows in the store by status (pending, in_flight, delivered, dead). Delivered and dead rows are removed with their event by history pruning.",
		[]string{"status"}, nil)
	dlqDesc = prometheus.NewDesc("angelia_dlq_size",
		"Dead-letter size: deliveries that gave up (status dead) and are still in the store.",
		nil, nil)
	deliveryStatuses = []string{store.DeliveryPending, store.DeliveryInFlight, store.DeliveryDelivered, store.DeliveryDead}
)

// deliveryCollector reports the delivery table. A failed query reports an
// invalid metric, which the handler leaves out of the scrape.
type deliveryCollector struct {
	src DeliveryCounter
	now func() time.Time

	log *slog.Logger

	mu     sync.Mutex
	at     time.Time
	counts map[string]int64
	err    error // the last failure, kept for the TTL like a success
}

func (c *deliveryCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- deliveriesDesc
	ch <- dlqDesc
}

func (c *deliveryCollector) Collect(ch chan<- prometheus.Metric) {
	counts, err := c.load()
	if err != nil {
		c.log.Warn("count deliveries for metrics", "error", err)
		ch <- prometheus.NewInvalidMetric(deliveriesDesc, err)
		return
	}
	for _, s := range deliveryStatuses {
		ch <- prometheus.MustNewConstMetric(deliveriesDesc, prometheus.GaugeValue, float64(counts[s]), s)
	}
	ch <- prometheus.MustNewConstMetric(dlqDesc, prometheus.GaugeValue, float64(counts[store.DeliveryDead]))
}

func (c *deliveryCollector) load() (map[string]int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now := c.now(); (c.counts != nil || c.err != nil) && now.Sub(c.at) < deliveryCountTTL {
		return c.counts, c.err
	}
	// Scrapes are not tied to one request's context (the result is shared
	// by all scrapers for the TTL), hence Background with its own timeout.
	// The mutex is held on purpose: concurrent scrapes wait for this one
	// query instead of each running their own.
	ctx, cancel := context.WithTimeout(context.Background(), deliveryCountTimeout)
	defer cancel()
	counts, err := c.src.CountDeliveriesByStatus(ctx)
	c.at = c.now()
	if err != nil {
		// Cached too, so that a struggling database is not queried by
		// every scrape.
		c.counts, c.err = nil, err
		return nil, err
	}
	if counts == nil {
		counts = map[string]int64{}
	}
	c.counts, c.err = counts, nil
	return counts, nil
}
