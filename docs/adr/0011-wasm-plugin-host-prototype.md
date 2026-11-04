# ADR 0011: WASM source plugins on Extism, with HTTP through a netguard host function (prototype)

- **Status:** Proposed
- **Date:** 2026-11-01
- **Story:** angelia-dev/angelia#59; amended by #145 (see [Amendment](#amendment-2026-11-04-145-operator-allowed_hosts-and-host-injected-credentials))

## Context

[architecture.md §3.6](../architecture.md#36-third-party-plugins) assumes third-party sources and sinks are WASM
plugins on Extism (`extism/go-sdk` on `wazero`, no CGO). HTTP must go through a host function so that the core's
rate limiter, conditional requests and SSRF guard ([netguard](../../internal/netguard/doc.go)) apply, and a plugin
only reaches the hosts its manifest declares ([§8](../architecture.md#8-security)).
[§10 Q1](../architecture.md#10-open-questions-for-phase-0) asks whether that host function handles auth headers,
pagination and ETags cleanly; benchmark 4 ([§6](../architecture.md#6-language-and-framework-phase-0-assumptions))
asks for < 5 ms of overhead per `List` call against a native source.

This story built the prototype: the host `internal/plugin/wasm`, a crates.io source plugin in Rust
(`bench/plugins/crates`), a misbehaving plugin for the deny-path tests (`bench/plugins/hostile`) and benchmark 4
(`bench/wasm`).

## Decision

Everything below is a **draft**, not a public contract. It is versioned `v0` so it can change freely.

**Manifest** (`bench/plugins/crates/manifest.json`), JSON, unknown fields rejected:

| Field | Meaning |
|---|---|
| `kind` | the source kind, `^[a-z][a-z0-9-]{0,62}$` |
| `config_schema` | JSON Schema of the plugin config map. The host only checks that every config key is one of its top-level `properties` (full validation needs a JSON Schema library, not in §6) |
| `allowed_hosts` | hostnames or IP literals. Exact, case-insensitive match on every hop (redirects included), any port, `http`/`https` only. No wildcards, ports, schemes or paths |

**Plugin exports** (Extism functions, JSON in and out): `validate(Target) -> {reason}` (empty reason = valid; no
network) and `list({target, state}) -> {versions, state, not_modified}`. The wire types mirror `source.Target`,
`source.FetchState` and `source.Version` (`internal/plugin/wasm/wire.go`, `bench/plugins/abi/src/lib.rs`).

**Host functions**, import module `angelia:host/v0`, one JSON block in, one out:

- `http_request({method, url, headers, body, validators}) -> {status, headers, body, validators, not_modified, error}`.
  Bodies are base64 and **buffered** (no streaming), capped at 10 MiB. Methods: GET, HEAD, POST. A request the host
  could not complete returns `error: {code, message}` with codes `host_not_allowed`, `blocked` (netguard SSRF
  guard), `not_allowed_here` (network during `validate`), `too_many_requests` (more than 100 HTTP requests in one
  call, each redirect hop counting as one), `bad_request`, `too_large`, `canceled`, `transport`. Any HTTP status,
  4xx and 5xx included, is a response.
- Request headers: names must be RFC 9110 tokens and values free of control characters; two names that are equal
  once canonicalised (`x-token`, `X-Token`) are a `bad_request`. Hop-by-hop and transport headers are owned by the
  host and refused with `bad_request`: `Connection`, `Upgrade`, `TE`, `Keep-Alive`, `Proxy-Authorization`,
  `Proxy-Connection`, `Expect`, `Transfer-Encoding`, `Content-Length`, `Host`. The ETag and Last-Modified
  validators are checked like header values. `User-Agent` stays overridable (see Open questions).
- `log({level, message})`: written to the core's `slog` logger with `plugin=<kind>`, message capped at 4 KiB.
  Each call has a log budget of 256 entries or 256 KiB, shared with Extism's own `log_*` functions; dropped
  entries (too large, not JSON) count against it, and once it is spent the rest of the call's entries are dropped
  with a single "log budget exhausted" warning.

**Security boundary.**

- Every hop goes through an `allowlistTransport` placed above the injected netguard client: the first request and
  each redirect are checked against `allowed_hosts` before netguard (and so before DNS or the network) sees them.
  Non-ASCII hostnames are refused outright. netguard then applies the SSRF guard, so a host in `allowed_hosts`
  that resolves to a private, loopback, link-local or metadata address is still refused.
- **DNS rebinding is not mitigated.** `allowed_hosts` constrains names, not addresses, and netguard checks the
  address when it resolves the URL host, then the transport resolves the name again when it dials
  ([netguard doc](../../internal/netguard/doc.go)). A plugin author who controls the DNS of one of its allowed
  hosts can answer a public address to the check and a private one to the dial. The dial-time check of
  angelia-dev/angelia#76 is therefore a **precondition before loading untrusted plugins**; until it lands, only
  plugins whose allowed hosts are trusted may run.
- A redirect from `https` to any other scheme is refused before it is sent: net/http keeps `Authorization` on a
  same-host redirect, so a downgrade would put the credential on the wire in clear text. A redirect to another
  host that is not a subdomain loses `Authorization` (net/http's rule), even if that host is allowed.
- A denied host, an SSRF block, network access during `validate` or an exhausted request budget fails the whole
  call (`ErrHostNotAllowed`, `netguard.ErrBlocked`, `ErrPolicy`), even if the plugin swallows the error.
- Extism's built-in `extism:host/env::http_request` uses `http.DefaultClient`; the host leaves the Extism manifest's
  `allowed_hosts` empty, so it refuses every host.
- No WASI, no file system, memory capped (default 64 MiB), each call bounded by a timeout and the caller's context
  (wazero `CloseOnContextDone`): 2 minutes for `list`, 2 seconds for `validate` (`Options.ValidateTimeout`), which
  has no network and must not stall config apply. Output capped at 16 MiB. An instance that trapped or was
  interrupted is discarded, never reused. No cookie jar.
- The output cap is checked late: Extism has already copied the output into host memory when the host compares
  its length, so the cap bounds what the core decodes, not what is allocated. The memory cap is what bounds the
  allocation.
- Healthy instances are pooled for reuse (up to `MaxIdle`, default GOMAXPROCS). A pooled instance keeps its linear
  memory, grown up to the 64 MiB cap, and its global state between calls, so the resident cost of a Source is up to
  `MaxIdle` × 64 MiB, and a plugin can carry state from one target's call to another's. Plugins are not isolated
  between targets of the same kind.

### Answer to §10 Q1: auth headers, pagination and ETags

Yes, all three are clean with the draft host function. From `bench/plugins/crates/src/lib.rs`:

```rust
// Auth headers: the plugin sets them like any header. The token comes from the plugin
// config map, filled by the core (and declared in config_schema).
if let Some(token) = config::get("token")? {
    headers.insert("Authorization".to_string(), token);
}

// ETags: pass the fetch state as validators; the host sends If-None-Match /
// If-Modified-Since and returns not_modified plus the validators to store.
let validators = (page == 0).then(|| Validators::from(&input.state));
let resp = abi::http(HttpRequest { url: url.clone(), headers: headers.clone(), validators, ..Default::default() })?;
if page == 0 && resp.not_modified {
    return Ok(Json(ListOutput { state: /* resp.validators */, not_modified: true, ..Default::default() }));
}

// Pagination: a loop in the plugin. Body cursors (crates.io meta.next_page) and Link headers
// (resp.header("link"), GitHub style) both work, since headers come back lower-cased.
match body.meta.next_page {
    Some(next) if next.starts_with('?') => url = format!("{first}{next}"),
    _ => return Ok(Json(ListOutput { versions, state, not_modified: false })),
}
```

netguard counts a request with `Authorization` against the authenticated budget of its host, so per-host limits
work unchanged. Tested in `TestCratesPaginationETagAuth` (3 pages, the token on every request, a 304 on the second
call) and in the contract suite.

### Benchmark 4

`go test -tags bench -run TestBenchmark4 -v ./bench/wasm`: 2,000 alternating calls per workload, same netguard
client, in-memory fake upstream (no sockets), so the difference is the plugin runtime. Measured in a 4-vCPU cloud
sandbox (Xeon 2.1 GHz, linux/amd64), not on reference hardware:

| Workload | native p50 / p95 | wasm p50 / p95 | overhead p50 / p95 | < 5 ms? |
|---|---|---|---|---|
| 1 page, 10 versions | 85 µs / 205 µs | 0.92 ms / 2.30 ms | 0.83 ms / 2.09 ms | yes |
| 1 page, 100 versions | 0.43 ms / 1.10 ms | 5.55 ms / 9.14 ms | 5.12 ms / 8.04 ms | **no** |
| 3 pages, 300 versions | 1.46 ms / 3.07 ms | 18.2 ms / 25.5 ms | 16.7 ms / 22.4 ms | **no** |
| 304 Not Modified | 12 µs / 56 µs | 0.25 ms / 0.77 ms | 0.24 ms / 0.71 ms | yes |

The overhead grows roughly linearly with the bytes moved between host and plugin. A CPU profile puts
it in wazero's call trampoline and the Extism kernel: Extism keeps plugin data in a separate kernel memory, and the
Rust PDK (1.4.1) copies it in and out with one host call per 8 bytes (`load_u64` / `store_u64`). Host-side HTTP and
JSON are a small share. The 304 path, the most common one with ETags, is well within target.

**Benchmark 4 does not pass for realistic payloads.** The maintainer's answer (2026-11-01, PR #144) is **go**:
the overhead is accepted for Phase 0, and the 5 ms target becomes a per-size budget. A zero-copy ABI (option b
below) is measured before the plugin SDK freezes the ABI.

### Maintainer decisions (2026-11-01, PR #144)

- **Benchmark 4: go.** Overhead accepted for Phase 0.
- **`allowed_hosts` ownership:** the manifest declares it, and the operator can extend or configure it when the
  plugin is installed (for example a registry mirror). Not implemented in this prototype; follow-up #145
  (done, see the amendment).
- **Credentials:** configured in the host when the plugin is installed, never in the plugin source. The host
  injects them; the plugin does not need to see the token. Not implemented in this prototype; follow-up #145
  (done, see the amendment).
- **Choices within the draft confirmed:** JSON with base64 and buffered bodies, GET/HEAD/POST only, 100 requests
  per call, exact-match `allowed_hosts`, and a `User-Agent` the plugin can override.

## Amendment (2026-11-04, #145): operator allowed_hosts and host-injected credentials

Implements the two maintainer decisions above. The original text is kept; where it conflicts, this section wins.
Still a draft (`v0`), and both items are **for the maintainer to confirm**: the config keys are a public contract
once released, and credential injection is a security decision.

**Where it is configured.** In the instance config, under `plugins.<kind>` ([configuration.md](../configuration.md#installed-plugins-draft)),
next to `sources.<kind>.token_env`, not in the watch file and not in the plugin's config map:

```yaml
plugins:
  crates:
    allowed_hosts: [crates-mirror.example.com]
    credentials:
      crates-mirror.example.com: {token_env: CRATES_MIRROR_TOKEN, scheme: Bearer}
```

In Go: `wasm.Options.AllowedHosts` and `wasm.Options.Credentials` (`internal/plugin/wasm/install.go`);
`config.Config.PluginToken(kind, host)` reads the token. `angelia serve` does not load plugins yet, so nothing
connects the two today.

**Operator `allowed_hosts`.** Merged with the manifest's list (a union; the operator cannot remove a manifest
host), same validation and matching. `Source.Hosts()` returns the merged list, each entry marked `manifest`,
`operator` and `credential`, and `New` logs it as `wasm plugin installed`.

**Credentials.**

- One credential per host, and the host must be in the merged `allowed_hosts`. It is always the `Authorization`
  header, `<scheme> <token>` or the bare token. Authorization only, so that netguard counts the request against
  the authenticated budget of the host with no change (custom headers such as `PRIVATE-TOKEN` can follow once
  netguard knows them, #151).
- Injected per hop in `allowlistTransport`, after the allowlist check and before netguard. A hop to the bound host
  gets the header, replacing any `Authorization` the plugin set; a hop to any other host is left as the plugin
  built it. The injected value lives only on the transport's copy of the request, so net/http's redirect code
  never sees it: a redirect to another host, allowed or not, does not carry it, and a redirect back to the bound
  host gets it again.
- Only over https: a plain http hop to a bound host fails before it is sent (the existing downgrade rule already
  refuses https → http redirects).
- The token is resolved for every hop through `Credential.Token(ctx)`, so a rotated value is picked up. This is
  the seam for the secret store (#75): today the instance config resolves `token_env`; a store-backed resolver
  replaces it without changing the host. An empty token, a value with control characters, or a resolver error
  fails the request, and the error never quotes the token.
- The plugin never receives it: not in its config map, and every injected token echoed back in a response body,
  response header, validator or error message is replaced by `[REDACTED]` before the plugin sees it. This does
  not catch a transformed echo (encoded, split); the allowlist is what limits where such an echo could be sent.
- The `crates` example plugin still sets `Authorization` from a `token` config key when one is given. With an
  injected credential no `token` is configured and the host's header wins; the plugin needs no change
  (`TestCratesMirrorWithInjectedCredential`).

Tested in `internal/plugin/wasm/credentials_test.go` (injection on the bound host only, the plugin's value
replaced, echo redacted, redirect to another allowed host without the credential, http refused, resolver
errors, merged hosts, authenticated budget) and `internal/config/plugins_test.go`.

**Open questions for the maintainer.**

1. Key names and place: `plugins.<kind>.allowed_hosts` and `plugins.<kind>.credentials.<host>.{token_env,scheme}`
   in the instance config. Alternative: a separate install file per plugin (with the `.wasm` path and manifest),
   which a later "install plugin" story will need anyway.
2. Authorization only, or also an arbitrary header name (`header:`)? Recommendation: Authorization only until a
   plugin needs more, then an additive `header` key restricted to names netguard counts as authenticated.
3. Plain http to a bound host is refused. Allow it for a mirror on a private network? Recommendation: no.

## Consequences

### Positive

- One allowlist and one netguard client for every plugin request: rate limits, retries, SSRF guard and logging
  apply to third-party code with no plugin cooperation.
- A plugin is one `.wasm` file plus a manifest; any language with an Extism PDK works.
- Q1 is answered positively: no host-function extension is needed for auth, pagination or ETags.

### Negative

- Per-call overhead scales with payload size and misses the 5 ms target from ~100 versions per page.
- In this prototype credentials still reach the plugin in its config map; host-side injection is follow-up #145.
  (Resolved by the amendment: the config map remains, but credentials belong in `plugins.<kind>.credentials`.)
- Buffered bodies: a plugin cannot stream a large response.
- Nothing in CI runs `bench/plugins/build.sh --check` yet, so a committed `.wasm` that no longer matches its Rust
  source would go unnoticed. A follow-up issue is #143 for a CI job (it needs the Rust toolchain and
  crates.io access); this story does not change the CI workflows.
- Untrusted plugins must wait for angelia-dev/angelia#76 (DNS rebinding, above).

### Open questions

- Wildcards in `allowed_hosts` (`*.example.com`): not supported; needed for CDNs.
- Full JSON Schema validation of the plugin config needs a dependency not listed in §6.
- Streaming bodies, methods beyond GET/HEAD/POST (sinks will need PUT), the size of the request and log budgets.
- An ABI version field in the manifest; today the import module name `angelia:host/v0` carries it.

## Alternatives considered

- **Extism's built-in `http_request`.** Uses `http.DefaultClient`: no netguard, no rate limits, no SSRF guard, and
  glob host matching. Kept disabled.
- **Host-managed ETags** (the host stores validators per URL). Simpler plugins, but state would live outside the
  target's `fetch_state_json` (ADR 0006). The plugin passes validators explicitly instead.
- **gRPC subprocess and exec plugins** (§3.6) are not measured by this story.

## References

- angelia-dev/angelia#59, epic #7; ADR 0006; `internal/plugin/wasm`, `bench/plugins`, `bench/wasm`
- Extism kernel memory: https://github.com/extism/proposals/blob/main/EIP-007-extism-runtime-kernel.md
