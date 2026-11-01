// Package netguard is the one HTTP client used by every source and sink, so
// that politeness and safety are enforced in one place.
//
// A [Client] combines:
//
//   - a per-host token bucket ([Limiter]) with defaults for known hosts
//     ([DefaultHostLimits]) and separate budgets for authenticated and
//     anonymous requests;
//   - pauses driven by Retry-After and GitHub's X-RateLimit-* headers;
//   - retries with exponential backoff and full jitter on 429, 5xx and
//     timeouts, for idempotent requests only;
//   - conditional requests ([Client.Fetch], [Validators]): a 304 is reported
//     as [Result.NotModified] and its body is never read;
//   - a User-Agent with the build version, per-attempt timeouts and a
//     response size cap ([ErrBodyTooLarge]);
//   - a basic SSRF guard ([Guard]) that refuses loopback, private and
//     link-local addresses unless they are allowlisted.
//
// The SSRF guard checks the URL host (and each redirect hop) before the
// request is sent, resolving hostnames once. It is not proof against DNS
// rebinding: the transport resolves the name again when it dials. Hardening
// that is a separate story (angelia-dev/angelia#76).
package netguard
