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

// DefaultDrainTimeout is how long roles and the HTTP server get, in total, to
// finish in-flight work after shutdown starts. It must stay below the
// Kubernetes terminationGracePeriodSeconds (30s by default).
const DefaultDrainTimeout = 25 * time.Second

// DefaultWriteTimeout is the HTTP server's write timeout.
const DefaultWriteTimeout = 10 * time.Second

// HTTPRole is implemented by roles that serve HTTP routes. The supervisor
// owns the single listener and calls Mount before it starts serving, so
// roles share the port with /healthz and /readyz.
type HTTPRole interface {
	Mount(mux *http.ServeMux)
}

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
	// WriteTimeout is the HTTP server's write timeout; zero means
	// DefaultWriteTimeout. A long-lived response (the event stream) moves its
	// own deadline.
	WriteTimeout time.Duration
	// Logger; nil means slog.Default().
	Logger *slog.Logger
}

// Run starts every role and the health server and blocks until ctx is
// cancelled or a role stops. A cancelled ctx is a clean shutdown and returns
// nil if the roles stopped within the drain timeout. A role that returns
// while the supervisor is still running, or fails, cancels the others and
// makes Run return an error.
func (s *Supervisor) Run(ctx context.Context) error {
	log := s.Logger
	if log == nil {
		log = slog.Default()
	}
	drain := s.DrainTimeout
	if drain <= 0 {
		drain = DefaultDrainTimeout
	}
	if len(s.Roles) == 0 {
		return errors.New("no roles to run")
	}

	ln := s.Listener
	if ln == nil {
		var err error
		ln, err = (&net.ListenConfig{}).Listen(ctx, "tcp", s.Listen)
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
	}

	writeTimeout := s.WriteTimeout
	if writeTimeout <= 0 {
		writeTimeout = DefaultWriteTimeout
	}
	var draining atomic.Bool
	srv := &http.Server{
		Handler:           s.handler(&draining, log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       60 * time.Second,
	}

	rootCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	g, gctx := errgroup.WithContext(rootCtx)
	for _, r := range s.Roles {
		g.Go(func() error {
			log.Info("role starting", "role", r.Name())
			err := r.Run(gctx)
			switch {
			case gctx.Err() == nil && err == nil:
				log.Error("role exited unexpectedly", "role", r.Name())
				return fmt.Errorf("role %s exited unexpectedly", r.Name())
			case err != nil && !errors.Is(err, context.Canceled):
				log.Error("role failed", "role", r.Name(), "error", err)
				return fmt.Errorf("role %s: %w", r.Name(), err)
			}
			log.Info("role stopped", "role", r.Name())
			return nil
		})
	}
	rolesDone := make(chan error, 1)
	go func() { rolesDone <- g.Wait() }()

	serveErr := make(chan error, 1)
	go func() {
		log.Info("health server listening", "addr", ln.Addr().String())
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- fmt.Errorf("health server: %w", err)
		}
	}()

	var result error
	var rolesFinished bool
	select {
	case <-gctx.Done(): // signal, or a role stopped
	case result = <-rolesDone:
		rolesFinished = true
	case result = <-serveErr:
	}

	// Single deadline for the whole shutdown. The health server stays up,
	// reporting not ready, while the roles drain.
	draining.Store(true)
	log.Info("shutting down", "drain_timeout", drain.String())
	cancel()
	dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), drain)
	defer dcancel()

	if !rolesFinished {
		select {
		case err := <-rolesDone:
			result = errors.Join(result, err)
		case <-dctx.Done():
			result = errors.Join(result, fmt.Errorf("shutdown: roles did not stop within %s", drain))
		}
	}

	if err := srv.Shutdown(dctx); err != nil {
		_ = srv.Close()
		log.Warn("health server did not shut down cleanly", "error", err)
	}
	return result
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
	for _, r := range s.Roles {
		if h, ok := r.(HTTPRole); ok {
			h.Mount(mux)
		}
	}
	return mux
}

func writeJSON(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}
