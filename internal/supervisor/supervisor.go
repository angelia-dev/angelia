package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

// DefaultDrainTimeout is how long roles and the HTTP server get to finish
// in-flight work after shutdown starts.
const DefaultDrainTimeout = 30 * time.Second

// ReadyChecker reports whether the instance can serve work, for example the
// database is reachable and migrations are applied. A non-nil error means
// not ready.
type ReadyChecker interface {
	Ready(ctx context.Context) error
}

// ReadyFunc adapts a function to ReadyChecker.
type ReadyFunc func(ctx context.Context) error

// Ready implements ReadyChecker.
func (f ReadyFunc) Ready(ctx context.Context) error { return f(ctx) }

// Supervisor runs roles and the health server together.
type Supervisor struct {
	// Roles to run.
	Roles []Role
	// Listen is the host:port of the health server. Ignored when Listener is set.
	Listen string
	// Listener, if set, is used instead of listening on Listen.
	Listener net.Listener
	// Ready gates /readyz. Nil means always ready.
	Ready ReadyChecker
	// DrainTimeout bounds shutdown; zero means DefaultDrainTimeout.
	DrainTimeout time.Duration
	// Logger; nil means slog.Default().
	Logger *slog.Logger
}

// Run starts every role and the health server and blocks until ctx is
// cancelled or a role fails. A cancelled ctx is a clean shutdown and returns
// nil if everything stopped within the drain timeout. A role error cancels
// the others and is returned.
func (s *Supervisor) Run(ctx context.Context) error {
	log := s.Logger
	if log == nil {
		log = slog.Default()
	}
	drain := s.DrainTimeout
	if drain <= 0 {
		drain = DefaultDrainTimeout
	}

	ln := s.Listener
	if ln == nil {
		var err error
		ln, err = (&net.ListenConfig{}).Listen(ctx, "tcp", s.Listen)
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
	}

	var draining atomic.Bool
	srv := &http.Server{
		Handler:           s.handler(&draining, log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	g, gctx := errgroup.WithContext(ctx)
	for _, r := range s.Roles {
		g.Go(func() error {
			log.Info("role starting", "role", r.Name())
			err := r.Run(gctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Error("role failed", "role", r.Name(), "error", err)
				return fmt.Errorf("role %s: %w", r.Name(), err)
			}
			log.Info("role stopped", "role", r.Name())
			return nil
		})
	}
	g.Go(func() error {
		log.Info("health server listening", "addr", ln.Addr().String())
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("health server: %w", err)
		}
		return nil
	})
	// On shutdown or first failure: report not ready, then drain the server.
	g.Go(func() error {
		<-gctx.Done()
		draining.Store(true)
		log.Info("shutting down", "drain_timeout", drain.String())
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drain)
		defer cancel()
		if err := srv.Shutdown(dctx); err != nil {
			_ = srv.Close()
			return fmt.Errorf("health server shutdown: %w", err)
		}
		return nil
	})

	done := make(chan error, 1)
	go func() { done <- g.Wait() }()
	select {
	case err := <-done:
		return err
	case <-gctx.Done():
	}
	// Shutdown has begun: roles get the drain timeout, then we give up on them.
	select {
	case err := <-done:
		return err
	case <-time.After(drain + time.Second):
		return fmt.Errorf("shutdown: roles did not stop within %s", drain)
	}
}

func (s *Supervisor) handler(draining *atomic.Bool, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"status":"ok"}`)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if draining.Load() {
			writeJSON(w, http.StatusServiceUnavailable, `{"status":"shutting down"}`)
			return
		}
		if s.Ready != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			if err := s.Ready.Ready(ctx); err != nil {
				log.Debug("not ready", "error", err)
				writeJSON(w, http.StatusServiceUnavailable, `{"status":"not ready"}`)
				return
			}
		}
		writeJSON(w, http.StatusOK, `{"status":"ready"}`)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}
