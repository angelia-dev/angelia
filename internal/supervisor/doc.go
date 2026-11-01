// Package supervisor runs the Angelia roles (api, scheduler, worker,
// dispatcher) inside one process, serves /healthz and /readyz, and shuts
// everything down gracefully on cancellation.
package supervisor
