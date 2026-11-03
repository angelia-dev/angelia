package metrics

import (
	"context"
	"net/http"

	"github.com/angelia-dev/angelia/internal/supervisor"
)

// Role returns a supervisor role that serves GET /metrics on the
// supervisor's listener, the one that already serves /healthz and /readyz
// (ADR 0019). It does no work of its own: Run only waits for shutdown.
func (m *Metrics) Role() supervisor.Role { return metricsRole{h: m.Handler()} }

type metricsRole struct{ h http.Handler }

var _ supervisor.HTTPRole = metricsRole{}

func (metricsRole) Name() string { return "metrics" }

func (metricsRole) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// Mount implements supervisor.HTTPRole.
func (r metricsRole) Mount(mux *http.ServeMux) { mux.Handle("GET /metrics", r.h) }
