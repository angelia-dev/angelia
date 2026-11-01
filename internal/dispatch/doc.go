// Package dispatch is the "dispatcher" role: it leases due deliveries,
// sends them through their sink, retries failures with full-jitter
// exponential backoff, and sets them dead after a permanent error or the
// last attempt (ADR 0016). Delivery is at least once.
package dispatch
