# ADR 0040: SSRF guard: a hostname allowlist that never opens metadata, at most 5 redirects, no https-to-http redirect

- **Status:** Accepted for D105–D109 (maintainer, 2026-11-25, on the recommended options); implementation reviewed in the PR
- **Date:** 2026-11-25
- **Story:** angelia-dev/angelia#76

## Context

Every source and sink sends through one client, `internal/netguard` ([architecture.md §8](../architecture.md#8-security)).
Its SSRF guard refuses loopback, private, link-local, CGNAT and other special ranges. Two earlier stories
closed most of the original scope of #76:

- #49 checks the URL host of **every redirect hop** (`Client.RoundTrip` runs `Guard.CheckURL` per hop) and
  added `http.allow_networks`, the allowlist by CIDR. Sinks follow no redirect at all.
- #108 ([ADR 0023](0023-webhook-sink-standard-webhooks.md) §5) added the **dial-time check**: the
  default transport's dialer runs `Guard.CheckAddr` on the address the socket actually connects to,
  which closes DNS rebinding for direct connections; a connection to the configured proxy is exempt.
  ADR 0023 records "DNS rebinding is #76 (open)" in its context and leaves "the allowlist config keys,
  redirects for sources and the rest of its scope" to #76.

What remained:

- An operator whose internal registry has a changing (DHCP) address had to open a whole subnet with
  `allow_networks`, which also opens the router's admin page and every other host on that network.
- `HTTPClient` followed up to 10 redirects (net/http's default), and so did the fallbacks of
  `apiclient.GuardRedirects` and of the WASM plugin host. No public registry a built-in source reads needs
  more than two hops (PyPI name normalisation and a renamed GitHub repository are one hop each).
- A redirect from `https` to `http` was followed for anonymous requests (`apiclient.GuardRedirects`
  refuses it only when credentials are present; the WASM host refuses it on its own), so a network
  attacker able to forge a 3xx on a TLS-terminating middlebox, or an open redirector on the registry,
  could downgrade a fetch and feed a source a forged version list.
- `TestDialRefusesRebindingToLoopback` skips itself when the machine does not resolve `localhost` to
  loopback only, so a CI image change could turn it off silently.

`Guard.CheckAddr(host, addr)` already receives the host name, from `CheckURL` and from the dial hook
(`dialer.DialContext` gets the URL's `host:port` for a direct connection), so a name-based exemption can
be applied at both checks without changing how dialling works.

## Decision

### D105: `http.allow_private_hosts`, a hostname allowlist that keeps metadata closed

- New key `http.allow_private_hosts` (list) in `angelia.yaml`, env `ANGELIA_HTTP_ALLOW_PRIVATE_HOSTS`
  (comma-separated). It lives only in the instance configuration: the watch file and the REST API cannot
  widen it, like `allow_networks`.
- Validation (`config check` and `serve`): each entry is a lower-case DNS name (`hostRE`), not an IP
  address in any spelling (canonical, or numeric such as `2130706433`, `0x7f000001`, `127.1`), no wildcard,
  no port, no scheme, no trailing dot, no duplicates; at most 64 entries
  (`config.MaxAllowPrivateHosts`). An error names the entry: `http.allow_private_hosts[i] "…": …`.
- `netguard.Config.AllowHosts []string`, copied in `New`. For a hop whose host name is listed (matched
  case-insensitively, a trailing dot ignored), `Guard.CheckAddr` accepts **loopback** (`127.0.0.0/8`,
  `::1`), **private** (RFC 1918, `fc00::/7`) and **CGNAT** (`100.64.0.0/10`) addresses, at the URL check
  and at dial time. It still refuses **link-local** (`169.254.0.0/16`, `fe80::/10`), the metadata
  addresses **`100.100.100.200`** and **`fd00:ec2::254`**, and every other range of the deny list; only
  `allow_networks` opens those. An IP literal in `AllowHosts` (possible from Go, refused by the config) is
  ignored.
- The exemption is **per hop**: a redirect to another name is judged by that name.
- An IP-literal URL is judged by its address, never by the host list.

### D106: one redirect cap, `netguard.MaxRedirects = 5`

`HTTPClient`'s `CheckRedirect` stops a chain when it already holds 5 requests, with
`netguard: stopped after 5 redirects`; net/http counts its own default of 10 the same way. The
fallbacks of `apiclient.GuardRedirects` and of the WASM plugin host (`internal/plugin/wasm`), used when
the wrapped client has no `CheckRedirect`, use the same constant. In production they always wrap
netguard's client, so its check applies. Not configurable.

### D107: the rebinding test fails, not skips, in CI

`needLocalhost` (the helper of the dial tests that use `localhost` as the name whose dial-time answer
differs from the stub resolver's) calls `t.Fatalf` instead of `t.Skipf` when the environment variable `CI`
is non-empty. The dial path is not made injectable: the existing test proves the property on the real
`net.Dialer` path, Happy Eyeballs included.

### D108: this ADR, and docs

This ADR records D105 to D109 and the threat cases below, and closes the "DNS rebinding is #76 (open)"
item of ADR 0023 (ADR 0023 is not edited). [configuration.md](../configuration.md#outgoing-http-and-private-networks)
gains the key and the redirect rules; [architecture.md §8](../architecture.md#8-security) one line. The
threat model (#116, `docs/security.md`) links the table below.

### D109: no redirect from https to http

netguard's `CheckRedirect` (so every client built on `HTTPClient`) refuses a hop whose scheme is not
`https` once any earlier hop of the chain was `https`, with an error matching `netguard.ErrInvalidURL`
(not retried; the message names the scheme and host only). A chain that starts on `http://` (a source
whose `base_url` is `http://`) may still be redirected to `http://`.

### Threat cases and the tests that cover them

| Threat | Defence | Test |
|---|---|---|
| URL to a private, loopback, link-local or metadata address | URL check on every resolved address | `TestGuardDefaultDenyRanges`, `TestGuardResolvedHostnames`, `TestGuardAddressSpellings` (netguard) |
| DNS rebinding: public at the URL check, private at the dial | dial-time check on the connected address | `TestDialRefusesRebindingToLoopback`, `TestDialControlFailsClosed` (netguard); fails in CI instead of skipping (D107) |
| Redirect to a private address | URL check per hop | `TestClientRefusesRedirectToBlockedAddress` (netguard) |
| Redirect to metadata from a listed host | the host list exempts per hop and never link-local or metadata | `TestAllowHostsPerHop`, `TestGuardAllowHosts` (netguard) |
| Listed name whose DNS is wrong or hijacked to metadata | link-local and metadata never exempt, at the URL check and the dial | `TestGuardAllowHostsURLCheck`, `TestDialAllowHostsRefusesMetadata` (netguard) |
| Listed name used to reach another host on its network | exact names, no wildcard; IP-literal URLs judged by address | `TestGuardAllowHosts` (netguard), `TestNetClientAllowPrivateHosts` (cmd/angelia), `TestHTTPAllowPrivateHosts` (config) |
| Open redirector chain | at most 5 requests per chain, each hop checked | `TestHTTPClientStopsAfterMaxRedirects` (netguard), `TestRedirectGuard` loop case (apiclient and ghapi) |
| Downgrade from https to http | refused, `ErrInvalidURL` | `TestHTTPClientRefusesHTTPSDowngrade` (netguard); credentialed chains also by `apiclient.GuardRedirects` |
| Request through a proxy | URL check only; rebinding is the proxy's job (ADR 0023 §5) | `TestDialProxyExempt`, `TestDialProxyFromEnvironment` (netguard) |
| Resolved IP leaked to a sink's `last_error` | sinks record the URL host only | `TestDeliverSSRF` (webhook) |

## Consequences

### Positive

- A private registry or homelab host can be reached by name without opening its network, and a wrong or
  hijacked DNS answer for that name still cannot reach cloud metadata.
- Fewer redirect hops, and no silent downgrade, for every source.
- The rebinding test cannot disappear from CI unnoticed.
- One constant for every redirect cap that wraps netguard's client.

### Negative

- A new key in the YAML config format (a public contract), and one more knob to explain next to
  `allow_networks` and `allow_http_hosts`.
- A listed name is trusted for every loopback, private and CGNAT address it resolves to; an attacker who
  controls the name's DNS can point it at another internal host (but not at link-local or metadata).
  Operators should list names whose DNS they control.
- A registry that redirects more than 4 times, or from https to http, now fails. None known.

### Open questions

- Other cloud metadata addresses inside private ranges, if any appear, need adding to
  `metadataAddrs` in `internal/netguard/guard.go`.
- `secrets.Bound.CheckRedirect(nil)` still has its own fallback of 10; nothing in production calls it
  without a `next` function, and `internal/secrets` does not import netguard.

## Alternatives considered

- **No hostname allowlist; document a /32 in `allow_networks`.** Breaks on a changing address and makes the
  operator open a whole subnet.
- **A hostname allowlist exempting every range.** A listed name that later resolves to
  `169.254.169.254` would reach cloud metadata.
- **A restrictive egress allowlist (only listed hosts reachable).** A different feature; it breaks every
  public registry by default.
- **Keep 10 redirects, or make the cap configurable.** No source needs more than 2; nobody asked.
- **An injectable dialer lookup for a hermetic rebinding test.** Rewrites the dial path #108 just
  hardened and loses `net.Dialer`'s Happy Eyeballs.
- **Refuse https-to-http only with credentials.** Already done by `apiclient.GuardRedirects`; it leaves
  anonymous fetches open to a forged version list.

## References

- Issues: #76, #49, #108, #116; epic #13.
- [ADR 0023](0023-webhook-sink-standard-webhooks.md) §5 (dial-time check, proxy exemption).
- `internal/netguard` (`guard.go`, `dial.go`, `client.go`), `internal/config/config.go`,
  `cmd/angelia/roles.go`, `internal/source/internal/apiclient/apiclient.go`, `internal/plugin/wasm/source.go`.
- [configuration.md, Outgoing HTTP and private networks](../configuration.md#outgoing-http-and-private-networks).
