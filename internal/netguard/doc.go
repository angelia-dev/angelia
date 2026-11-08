// Package netguard is the one HTTP client used by every source and sink, so
// that politeness and safety are enforced in one place.
//
// A [Client] is an http.RoundTripper; sources and sinks receive the
// *http.Client from [Client.HTTPClient] and use [Fetch] for conditional
// requests. Every hop, redirects included, gets:
//
//   - a per-host token bucket with defaults for known hosts
//     ([DefaultHostLimits]) and separate budgets for authenticated and
//     anonymous requests. [Config].HostLimits and FallbackLimit (from
//     http.host_limits, ADR 0027) add hosts and replace the fallback, but
//     can only lower a built-in budget ([EffectiveLimits]);
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
// resolving hostnames with Config.Resolver. The default transport then
// checks again at dial time: its dialer runs [Guard.CheckAddr] on the
// address the socket actually connects to, after its own DNS lookup, so a
// name that resolves to a public address for the URL check and to a
// private one for the dial (DNS rebinding) is refused. A connection to the
// configured proxy (Config.Proxy, by default HTTPS_PROXY and HTTP_PROXY as
// read once when the client is built) is exempt: proxies usually have
// private addresses, and the proxy, not Angelia, resolves the target.
// Through a proxy only the URL check applies, and rebinding protection is
// the proxy's job. A custom Config.Transport bypasses the dial-time check.
package netguard
