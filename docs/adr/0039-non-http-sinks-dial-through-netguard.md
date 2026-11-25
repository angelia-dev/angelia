# ADR 0039: Non-HTTP sinks dial through netguard

- **Status:** Proposed (decisions D75 to D79 accepted by the maintainer on 2026-11-25; this record awaits review)
- **Date:** 2026-11-25
- **Story:** angelia-dev/angelia#93

## Context

PRD F8 asks for MQTT and NATS sinks, and [architecture.md §3.5](../architecture.md#35-dispatcher-and-sink-plugins-deliver)
lists `mqtt` and `nats` as v1 built-ins. Every sink so far speaks HTTP and sends through the one netguard
client ([internal/netguard](../../internal/netguard/doc.go)): the SSRF guard checks each URL, the transport's
dialer checks the address it actually connects to (DNS rebinding, ADR 0023 §5), and the per-host limiter
spaces the requests (ADR 0027).

A broker is reached over raw TCP. Before this decision:

- netguard had no TCP entry point. Its dialer was private to the HTTP transport, the guard's `CheckURL`
  accepted only `http` and `https`, and the limiter ran only in `RoundTrip`. A sink that called `net.Dial`
  would bypass the guard, `http.allow_networks` and the limiter entirely: an SSRF hole open to anyone who
  can write a sink configuration.
- `secrets.Destination` accepted only `http` and `https`, so a stored password bound to hosts
  (ADR 0024 §2) was refused for a broker with `ErrHostMismatch`.
- Plaintext needed an opt-in, as for webhooks (`http.allow_http_hosts`), and TLS needed the instance's
  roots (`http.ca_file`).

## Decision

1. **`netguard.Client.DialContext(ctx, network, address)`** is the only way a sink opens a TCP
   connection ([tcp.go](../../internal/netguard/tcp.go)). It:
   - accepts `tcp`, `tcp4` and `tcp6` only, a `host:port` address and a canonical IP literal
     (`NonCanonicalIP`); anything else wraps `ErrInvalidURL`;
   - waits on the host's **anonymous** limiter bucket, keyed by host name, so `http.host_limits` applies
     to brokers as to any host (a broker connection carries no `Authorization` header, so it never uses
     the authenticated budget);
   - checks the name first, as `CheckURL` does for a URL host (resolve with `Config.Resolver`, every
     address allowed), then dials with the same `ControlContext` check as the HTTP dialer on the address
     actually connected;
   - **never uses a proxy**: `HTTPS_PROXY` is an HTTP setting, and the HTTP dialer's exemption of the
     proxy address does not apply, so a configured proxy address is checked like any other;
   - reports a refusal with an error that wraps `*BlockedError` and `ErrBlocked` and names the host,
     never the resolved address.
2. **No direct dial under `internal/sink`.** The `sink` package doc says so, and a test
   ([nodial_test.go](../../internal/sink/nodial_test.go)) parses every Go file under `internal/sink/`
   with `go/parser` and fails on `net.Dial`, `net.DialTimeout`, `net.Dialer`, `tls.Dial` and their
   variants. `.golangci.yml` is unchanged (no `forbidigo` or `depguard`).
3. **Private brokers** need `http.allow_networks`, exactly like a webhook on a LAN. No new YAML key.
4. **TLS by default.** A TLS sink wraps the netguard connection in `tls.Client` with `MinVersion` TLS 1.2,
   `ServerName` the URL host and `RootCAs` the instance pool (system roots plus `http.ca_file`, the pool
   `cfg.HTTP.LoadCAPool()` builds for the HTTP client). No option disables verification. Client
   certificates are a later, additive option.
5. **Plaintext reuses `http.allow_http_hosts`.** A cleartext scheme (`mqtt://`) is accepted only for a host
   listed there, checked at every delivery; the key already means "hosts I accept cleartext to", and the
   docs now say it covers MQTT.
6. **Secret destinations learn broker schemes.** `secrets.Destination` accepts `mqtt` (default port 1883)
   and `mqtts` (default port 8883) besides `http` and `https`. `hostKey` is unchanged: a hosts entry
   without a port still means 443, so the docs say to bind a broker secret with an explicit port
   (`--host broker.example.net:8883`). The NATS story adds its own schemes.

## Consequences

### Positive

- One place enforces the SSRF rules for HTTP and TCP alike, including the rebinding check; a new
  non-HTTP sink (NATS next) only reuses `DialContext`.
- The limiter protects brokers from a burst of deliveries, configured as for any host.
- Host-bound stored secrets work for brokers with no new concept.

### Negative

- A broker on a private network needs `http.allow_networks`; most MQTT brokers are on a LAN, so most MQTT
  users set it. The docs and `config check` say so.
- A broker reachable only through an HTTP proxy cannot be used.
- The name check and the dial each resolve the host (two lookups), as for HTTP.

### Open questions

- Whether `config check` should list the broker hosts that `http.allow_networks` does not cover by name
  (it warns for private address literals only).
- Connection pooling across deliveries is out of scope: one connection per attempt.

## Alternatives considered

- **Each sink calls `Guard.CheckAddr` itself.** The check would be duplicated, and a sink that checks the
  name and then dials with `net.Dial` reopens the rebinding gap.
- **No guard for brokers**, since most are private. An SSRF hole for anyone who can configure a sink.
- **A new `mqtt.allow_plaintext_hosts` key.** A clearer name, but a YAML contract change and one more
  setting with the same meaning as `http.allow_http_hosts`.
- **Honour `HTTPS_PROXY` with `CONNECT`.** Few brokers sit behind an HTTP proxy, and the proxy would
  resolve the target, which leaves only the name check.
- **A lint rule (`forbidigo`) instead of the source test.** Needs a change to `.golangci.yml`; the test
  needs nothing new and runs with `go test`.

## References

- Issue angelia-dev/angelia#93 (decisions D75 to D79), PRD F8.
- [ADR 0023](0023-webhook-sink-standard-webhooks.md) §5 (dial-time check), [ADR 0024](0024-secrets-encrypted-at-rest.md)
  §2 (secrets bound to hosts), [ADR 0027](0027-configurable-per-host-rate-limits.md) (per-host limits).
- [docs/configuration.md, Outgoing HTTP and private networks](../configuration.md#outgoing-http-and-private-networks).
