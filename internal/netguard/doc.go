// Package netguard is the one HTTP client used by every source and sink, so
// that politeness and safety are enforced in one place.
//
// A [Client] is an http.RoundTripper; sources and sinks receive the
// *http.Client from [Client.HTTPClient] and use [Fetch] for conditional
// requests. Every hop, redirects included, gets:
//
//   - a per-host token bucket with defaults for known hosts
//     ([DefaultHostLimits]) and separate budgets for authenticated and
//     anonymous requests;
//   - pauses driven by Retry-After and GitHub's X-RateLimit-* headers;
//   - retries with exponential backoff and full jitter on 429, 5xx and
//     timeouts, for idempotent requests only;
//   - conditional requests ([Fetch], [Validators]): a 304 is reported
//     as [Result.NotModified] and its body is never read;
//   - a User-Agent with the build version and a per-attempt timeout
//     ([Config].Timeout, 30 s);
//   - a response size cap ([ErrBodyTooLarge]) on the body after the
//     transport's transparent gzip decoding (a caller setting
//     Accept-Encoding itself is capped on wire bytes): 10 MiB
//     ([Config].MaxBodyBytes);
//   - per-host overrides of both ([HostLimit].MaxBodyBytes and
//     [HostLimit].Timeout), with built-in ones for the large registries
//     registry.npmjs.org, pypi.org and index.crates.io (64 MiB, 2 minutes).
//     A redirect does not carry them to the next host; each hop's host
//     decides;
//   - a basic SSRF guard ([Guard]) that refuses loopback, private,
//     link-local, CGNAT, multicast, broadcast and IPv4-embedding IPv6
//     addresses unless they are allowlisted.
//
// The SSRF guard checks the URL host of each hop before it is sent,
// resolving hostnames once. It is not proof against DNS rebinding: the
// transport resolves the name again when it dials. Hardening that, with a
// dial-time check in netguard's own dialer, is a separate story
// (angelia-dev/angelia#76). A custom Config.Transport bypasses any
// dial-level check.
package netguard
