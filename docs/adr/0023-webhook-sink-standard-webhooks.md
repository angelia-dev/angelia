# ADR 0023: The webhook sink posts structured CloudEvents signed with Standard Webhooks v1, takes its secrets from the core, and dials only through netguard

- **Status:** Accepted (2026-11-05, maintainer decision on #194: "go with what proposed")
- **Date:** 2026-11-05 (revised the same day after the joint security review of ADRs 0023 and 0024)
- **Story:** angelia-dev/angelia#108

> Written as the design note for a **Human-led** story; the maintainer accepted it, with every open
> question settled as recommended, before the implementation.

## Context

[ADR 0005](0005-standard-webhooks-signing.md) chose Standard Webhooks for signing and left the header
values, the secret format and rotation open. The pieces around the sink now exist:

- The sink contract ([ADR 0017](0017-dispatcher-delivery-leases-and-retries.md) §7–§10, `internal/sink`):
  `Deliver(ctx, Request)` gets the decrypted `Config`, the stored event JSON, the `Rendered` body and
  content type, and `IdempotencyKey` = `<event id>:<subscription id>`. `sink.HTTPStatus` classifies a
  response, `sink.Permanent` stops retries, `sink.RedactURL` cleans `last_error` and logs.
- The dispatcher bounds one attempt (rendering and the sink call) with `DefaultDeliverTimeout` = 15 s,
  and the finishing store write with `writeTimeout` = 8 s, both below the 25 s drain timeout.
- The watch file and the OpenAPI spec already define the `webhook` config
  ([watch-file.md §2.4](../watch-file.md), spec `WebhookSinkConfig`): `url`, `secret_env`, `timeout`
  (default 10 s, `1s`–`2m`), `additionalProperties: false`. Decision D14 stores the *name* of the
  environment variable, never the value; D12 allows `http://` only for allowlisted hosts. Spec
  docs/api.md decisions 25 and 26: an API-set `*_env` name must be in `api.allowed_env`.
- netguard (#49) gives an `*http.Client` with a URL-level SSRF guard (`Config.Allow` prefixes, no YAML
  key yet), a 10 MiB body cap, a 30 s per-attempt timeout, a per-host limiter that honours
  `Retry-After`, `Proxy: http.ProxyFromEnvironment`, and no retries for `POST`. It follows up to 10
  redirects unless the caller overrides `CheckRedirect`. It does **not** check the dialled address:
  DNS rebinding is #76 (open).
- Secrets at rest are #75 (open, Human-led), designed in ADR 0024 (drafted in parallel). ADR 0024 §5 has
  the core resolve secret references into the request; it adds `secret_ref` next to `secret_env`.

The Standard Webhooks spec (fetched 2026-11-05) signs `msg_id.timestamp.payload` with HMAC-SHA256,
serialises secrets as `whsec_` + base64 of 24 to 64 random bytes, sends `v1,<base64>` entries
space-separated in `webhook-signature` for rotation, and says the id and timestamp must not contain `.`.

## Decision

The `webhook` kind lives in `internal/sink/webhook`. Decisions are numbered to match the design brief.

### 1. Wire format

- `POST` to `config.url`. Body: `Rendered.Body`, byte for byte. Without a template that is the stored
  CloudEvents JSON (structured mode) and `Content-Type: application/cloudevents+json; charset=utf-8`
  (`sink.CloudEventsContentType`); with a template, `Rendered.ContentType`. The signed bytes are the sent
  bytes; the sink never re-serialises.
- `webhook-id` = `Request.IdempotencyKey`, i.e. `<event ULID>:<subscription ULID>`. It is the same on
  every attempt **and on replay**, so receivers deduplicate on it as the spec recommends. Consequence to
  document for operators: a receiver that deduplicates drops a replay of a delivery it already
  processed; replay is meant for deliveries the receiver never accepted. Both parts are ULIDs, so the id
  never holds `.` or white space; the sink still rejects such a key as permanent (defence in depth).
- `webhook-timestamp` = Unix seconds of **this attempt**, as a decimal string, from the sink's clock read
  just before sending. The clock is a constructor option, `webhook.New(client, webhook.Options{Now:
  func() time.Time})`, defaulting to `time.Now`; `sink.Request` gets no clock. The HMAC input uses the
  exact header string, not a re-formatted number.
- `webhook-signature` = one `v1,<std base64 HMAC-SHA256(key, id + "." + timestamp + "." + body)>` per
  secret, joined by a single space, in the order the secrets were given. Absent when the sink has no
  secret (unsigned delivery, already allowed by the contract with a `config check` warning). The docs
  tell receivers to **require** a valid signature: removing `secret_env` silently downgrades deliveries
  to unsigned, and only a receiver that insists on a signature notices.
- **No `ce-*` headers.** They belong to CloudEvents binary mode; in structured mode the attributes are
  in the body, and duplicating them invites receivers to read the unsigned copy.
- `User-Agent` comes from netguard. No custom headers in #108 (see Future work).

### 2. Secret format and parsing

- A secret is `whsec_` + **strict padded standard base64** (`base64.StdEncoding.Strict()`) of **24 to 64
  decoded bytes**. URL-safe and unpadded forms are rejected, so every key the sender accepts also decodes
  in the reference receiver libraries. The HMAC key is the decoded bytes. A key of a single repeated byte
  is refused as a cheap guard against placeholders; entropy itself cannot be measured, so the docs say
  "generate it, do not type it".
- Parsing a secret value: `strings.TrimSpace`, then `strings.Fields`, then for each field require and
  strip `whsec_` and decode as above. One to three fields; duplicates refused. Trimming makes a trailing
  newline from a mounted file or `set --from-file` harmless (ADR 0024 cross-check C7).
- **Always user-supplied.** `angelia webhook secret new` prints `whsec_` + base64 of 32 bytes from
  `crypto/rand` to stdout, locally, and stores and sends nothing. This is **new public CLI surface**
  (decided question 6). Angelia has no server-side generation.
- A missing, empty or invalid secret is a **retried** error (the operator fixes it by setting the value),
  raised by the core before `Deliver` (section 4). `last_error` names the reference and the rule, never
  the value: `webhook: secret_env WH_CI: invalid secret (want 1-3 whsec_ secrets of 24-64 bytes)`.

### 3. Rotation

- One secret value holds **one to three** space-separated secrets, whether it comes from `secret_env`
  today or from an ADR 0024 stored secret via `secret_ref`. The sink signs with each. Three covers
  current, previous and one emergency key during a planned rotation, and keeps the header under 200
  bytes.
- Adding: set the value to `whsec_NEW whsec_OLD` (for `secret_env`, then restart, since the process
  environment is read at start). Deliveries carry both signatures; receivers with either key verify.
- Retiring: once every receiver has the new key, set the value to `whsec_NEW`.
- API and watch file: **no change**. They carry only the reference (`secret_env`, later `secret_ref`;
  at most one of the two per sink, both optional), which is returned as-is. No key ids, expiry, count or
  fingerprint are exposed.

### 4. Secret resolution and storage

- **Resolution is in the core**, matching ADR 0024 §5. `sink.Request` gains `Secrets map[string][]byte`
  (the shape is ADR 0024's), filled by the dispatcher when it leases the delivery: from the environment
  for `secret_env` today, from the encrypted store for `secret_ref` after #75. An unresolvable
  reference fails the attempt before `Deliver` as a retried error. **The sink never calls
  `os.Getenv`**; it parses the value it is given (section 2). This adds a field to the `Request` struct,
  which ADR 0017 §8 anticipated ("signing secret").
- **#108 does not wait for #75.** No secret value ever reaches the database, the watch file or the API:
  the sink row stores the reference only.
- **References only.** The webhook config never gets an inline secret key, so no `x-angelia-secret` key
  is expected for this kind.
- Logs and errors never contain the secret, the decoded key, the HMAC input or the signature header.
  The sink logs nothing itself; it returns errors built from fixed strings plus the reference name, the
  status code and a `RedactURL`-cleaned URL. Base64 decode errors are replaced, not wrapped.
- **Capture test** (acceptance criterion 3): a dispatcher with a `slog` JSON handler at debug level
  writing to a buffer and an in-memory span recorder (`internal/telemetry/telemetrytest`), one webhook
  sink, a known secret, and `httptest` receivers that answer 200, 500, 404, 429, a redirect, a timeout
  and a reset; plus an invalid secret, an unset variable and an SSRF refusal. It asserts that none of the
  full secret, its base64 part, the hex of the key or any emitted `webhook-signature` value appears in
  the log buffer, span attributes and events, `delivery.last_error`, or any returned error string.

### 5. SSRF

- Every request goes through the netguard client handed to the sink's constructor; the sink's copy of
  the `*http.Client` overrides only `CheckRedirect` (below). `http.DefaultClient` is never used.
- Private, loopback, link-local, CGNAT, multicast and IPv4-embedding IPv6 targets are refused unless the
  address is in netguard's `Allow` prefixes. That list lives **only in the instance configuration**
  (`angelia.yaml`), set by the operator; the watch file and the API cannot widen it (keys: open
  question 4).
- **Guard gaps closed in #108** (acceptance criterion 4): add to netguard's `blockedPrefixes` Teredo
  `2001::/32`, NAT64 local-use `64:ff9b:1::/48`, IETF protocol assignments `192.0.0.0/24`, benchmarking
  `198.18.0.0/15` and reserved `240.0.0.0/4`, each with a guard test.
- **DNS rebinding: dial-time check, with the proxy exempt.** netguard's transport dials through a
  wrapped `DialContext`: for a direct connection it dials with a `net.Dialer.Control` hook that runs
  `Guard.CheckAddr` on the address actually connected. A dial to the **configured proxy** (the
  `host:port` of `HTTPS_PROXY`/`HTTP_PROXY`, read once when the client is built) skips that hook: corporate
  proxies usually have RFC 1918 addresses, and the proxy, not Angelia, resolves the target. **Through a
  proxy only the URL-level check applies**, and rebinding protection is the proxy's job; the deployment
  docs say so. Tests: (a) direct, a resolver that answers a public address to the URL check and
  `127.0.0.1` to the dial is refused at dial time; (b) proxied, with the proxy variable pointing at an
  `httptest` proxy on loopback, a public-hostname target reaches the proxy, and a private-literal
  target is still refused by the URL check before any dial.
  This is a change to **netguard, a sensitive path shared with every source**, and overlaps #76: #108
  takes the dial hook and the five prefixes; #76 keeps the allowlist config keys, redirects for
  sources and the rest of its scope.
- A refused address is **permanent** (a configuration fault; replay exists once the operator
  allowlists it). `last_error` and logs store `webhook: destination not allowed (<host>)` with the URL
  host only, **never the resolved IP address**, so `read` callers learn nothing about internal ranges.
- **Redirects are not followed**: `CheckRedirect` returns `http.ErrUseLastResponse`; a 3xx is permanent
  (maintainer decision on #169).
- **Timeouts**: connect 30 s and TLS 10 s (netguard transport), all bounded by one context deadline of
  `min(config.timeout, what is left of the dispatcher's DeliverTimeout)`. Rendering and any wait in
  netguard's per-host limiter come out of the same 15 s. With the defaults the request gets at most 10 s;
  a `timeout` above 15 s has no effect, so `config check` warns (decided question 3).
- **Per-host limiter**: a receiver's `429`/`503` with `Retry-After` pauses its host in netguard's shared
  limiter, which also delays sources on that host (for example a webhook on `api.github.com`). A pause
  longer than the remaining deadline makes the attempt fail as retried, and the dispatcher's backoff,
  floored by `Retry-After`, takes over.
- **Response body**: the sink reads at most 64 KiB and discards it (so the connection can be reused),
  then closes. netguard's 10 MiB cap is a backstop. Nothing of the body is stored.

### 6. Response handling

| Outcome | Dispatcher result |
|---|---|
| 2xx | delivered (`Result.StatusCode`) |
| 408, 429, 5xx, 1xx, other unexpected codes | retried; `Retry-After` in `sink.StatusError.RetryAfter` |
| 3xx, other 4xx (incl. 401, 404, 410) | permanent, dead at once (ADR 0017 §7, maintainer decision 3) |
| connection refused/reset, DNS failure, TLS error, timeout | retried |
| SSRF refusal, invalid URL, `.` in the id | permanent |
| secret unresolved or invalid | retried (sections 2 and 4) |

This is `sink.HTTPStatus` unchanged. `Retry-After` parsing reuses netguard's parser, exported as
`netguard.ParseRetryAfter`. The response body is **not stored**, even truncated: it is
receiver-controlled and `last_error` is shown to `read` API callers. `last_error` holds
`webhook: <scheme://host>: HTTP <code> <text>`.

### 7. Verification

- `internal/sink/webhook` exports, next to `Sign`:

  ```go
  const DefaultTolerance = 5 * time.Minute
  func Verify(secrets []string, h http.Header, body []byte, now time.Time, tolerance time.Duration) error
  ```

  It accepts `whsec_`-prefixed or bare base64 secrets of any non-zero length (receiver side, like the
  reference library). It requires the three headers. The timestamp must be ASCII digits only and fit in
  an `int64`; it is valid when `|now − ts| ≤ tolerance` (**inclusive** at both ends). The signature
  header is split on spaces; entries that are not `v1,` or not valid base64 are ignored; comparison is
  `hmac.Equal`. Errors are sentinels (`ErrMissingHeaders`, `ErrTimestamp`, `ErrNoMatchingSignature`).
  It is used by the sink's tests; an `angelia webhook verify` CLI is not part of #108 (decided question 6).
  External Go consumers use the official library.
- **Test vectors** (acceptance criterion 1) are vendored from the official repository
  `github.com/standard-webhooks/standard-webhooks` (Apache-2.0, per its `LICENSE`), pinned to a commit
  recorded in `internal/sink/webhook/testdata/standard-webhooks/README.md` with the licence text, and
  its `NOTICE` file copied alongside if the pinned commit has one:
  - signing: secret `whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw` (24 bytes), id
    `msg_p5jXN8AQM9LWM0D4loKWxJek`, timestamp `1614265330`, body `{"test": 2432232314}` gives
    `v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE=` (`libraries/go/webhook_test.go`,
    `TestWebhookSign`; recomputed independently for this note and by the reviewer);
  - verification cases from the same file: wrong signature, empty `v1,`, several entries with one
    valid, garbage before a valid entry, `v2,` prefix, trailing `,extra`, and timestamps at and just
    beyond the 5 min tolerance in both directions.
  A test runs every vector through `Sign` and `Verify`; a round-trip test signs Angelia-shaped
  deliveries (ULID id, CloudEvents body, three secrets) and verifies each secret alone.
- **Proposal only**: publish Angelia-shaped vectors in the spec repository under
  `webhooks/v1/vectors.json`, CC-BY-4.0 like the schemas (decided question 7).

### Future work (not in #108)

Custom request headers are deferred, including credential headers such as `Authorization`. Header values
would be returned to `read` callers, and the token heuristics cannot recognise a random API key, so they
need the stored-secret references of ADR 0024 and their own masking rules. A later ADR settles the key
shape and the reserved names (at least `Host`, `Content-Length`, `Content-Type`, hop-by-hop headers,
`webhook-*`, `ce-*` and `User-Agent`).

## Consequences

### Positive

- Receivers use any Standard Webhooks library unchanged; `webhook-id` is a real idempotency key.
- #108 ships without pre-empting #75: no secret value reaches the database or the API, and the sink
  receives secrets the same way before and after ADR 0024.
- Rotation works today with no contract change, only a documented format for the secret value.
- Webhooks get the dial-time SSRF check and the missing reserved ranges before API-configured sinks ship.

### Negative

- Rotating a `secret_env` secret needs a process restart.
- No custom or credential headers until a later story.
- A refused SSRF target, a redirect and a 410 are dead at once; the operator must replay.
- The sender requires `whsec_` and padded base64 while the reference library tolerates bare keys.
- Through an HTTP proxy there is no rebinding protection on Angelia's side.
- The netguard change touches every source's HTTP path.

### Decided questions

The maintainer accepted every recommendation on #194 (2026-11-05). Settled earlier by the coordinator
after review: custom headers (deferred), secret resolution in core (`Request.Secrets`), the dial-time
check with proxy exemption in #108, the clock option, the parsing rules, and adding the guard gaps.

1. **`webhook-id` value:** the idempotency key `<event>:<subscription>`, stable across retries and
   replay and distinct per subscription.
2. **Maximum number of secrets in one value:** three.
3. **`timeout` above 15 s:** the contract stays (`1s`–`2m`); the sink applies
   `min(timeout, dispatcher deliver timeout)` and `config check` warns when `timeout` exceeds it.
4. **Allowlist keys:** instance keys `http.allow_networks` (CIDRs) and `http.allow_http_hosts`
   (hostnames, D12), shared by sources and sinks, added in #108.
5. **Unsigned deliveries:** `secret_env` (later `secret_ref`) stays optional; an unsigned sink logs a
   warning and `config check` reports it; the docs tell receivers to require signatures.
6. **CLI:** `angelia webhook secret new` ships in #108; no `angelia webhook verify` for now.
7. **Spec-repo vectors:** the section 7 proposal becomes a follow-up spec story.

## Alternatives considered

- **Store the secret in the sink config now, plaintext or with an ad hoc key.** It would pre-empt #75 and
  ADR 0024 and break architecture.md §8. Rejected.
- **Wait for #75 before #108.** Blocks delivery on a Phase 1 story while D14 already gives a safe path.
- **The sink reads the environment itself.** Two resolution paths after ADR 0024; rejected for
  `Request.Secrets`.
- **Dial-time check for every connection, proxy included.** Breaks every proxied deployment (proxies
  are usually on private addresses). Rejected for the proxy exemption.
- **Asymmetric `v1a` (ed25519) signatures.** Better when the receiver is untrusted, but needs key
  distribution; HMAC is what ADR 0005 and the issue name. Can be added later.
- **Binary-mode CloudEvents (`ce-*` headers, data-only body).** Splits the event between signed body and
  unsigned headers. Rejected as the default.
- **Storing a truncated response body.** Receiver-controlled content shown to `read` callers.

## References

- [ADR 0005](0005-standard-webhooks-signing.md), [ADR 0017](0017-dispatcher-delivery-leases-and-retries.md),
  [ADR 0022](0022-rest-api-in-house-spec-driven-validation.md); ADR 0024 (secrets at rest, #75)
- [architecture.md §3.5](../architecture.md#35-dispatcher-and-sink-plugins-deliver),
  [§8](../architecture.md#8-security); [watch-file.md](../watch-file.md) §2.4, D12, D14;
  [configuration.md](../configuration.md) (`api.allowed_env`)
- Spec repository: docs/api.md decisions 25–27, `openapi/angelia.v1.yaml` `WebhookSinkConfig`
- Issues: #108, #75, #76, #49, #107
- Standard Webhooks specification, `spec/standard-webhooks.md`, and reference library
  `libraries/go/webhook_test.go`, github.com/standard-webhooks/standard-webhooks (Apache-2.0)
- CloudEvents HTTP protocol binding 1.0 (structured and binary content modes)

## Amendment (2026-11-05): implementation notes from #108

Recorded by the implementation; no decision above changes.

- **Licence of the vectors.** The repository's root `LICENSE` is Apache-2.0, but its `libraries/`
  directory, which holds `libraries/go/webhook_test.go`, carries its own MIT licence ("Copyright (c) 2023
  Svix"). Both are copied into `internal/sink/webhook/testdata/standard-webhooks/`; the pinned commit has
  no `NOTICE`. The vectors are transcribed into `vectors.json`; the boundary cases follow section 7
  (inclusive tolerance), where the reference test fails at exactly 5 minutes only because time passes.
- **`Request.Secrets`** is `map[string][]byte` keyed by the JSON Pointer of the reference key
  (`/secret_env`), resolved by `dispatch.ResolveSecrets` for every `*_env` key of the sink config.
  An unset or empty variable fails as `dispatch: secret /secret_env: environment variable WH_CI is not
  set or empty`; a value that is not a valid secret fails in the sink with the section 2 message.
- **Proxy exemption.** netguard gains a Go-level `Config.Proxy` (default `http.ProxyFromEnvironment`);
  the exempt addresses are what it returns for an http and an https probe URL when the client is built.
- **Allowlist keys.** `http.allow_networks` (CIDR prefixes; `/0` warns) and `http.allow_http_hosts`
  (lower-case host names or IPv4 addresses), with comma-separated `ANGELIA_HTTP_ALLOW_NETWORKS` and
  `ANGELIA_HTTP_ALLOW_HTTP_HOSTS`. `allow_http_hosts` reaches the watch-file loader and the API;
  `HTTP.AllowPrefixes()` is ready for `netguard.Config.Allow`, which `angelia serve` passes once it wires
  the dispatcher role (it still runs a stub, see `cmd/angelia/serve.go`; follow-up #198).

### Runtime choices after the review of #108 (2026-11-05)

- **URL errors (§6).** netguard's URL-check errors that no retry can fix (scheme, missing host,
  non-canonical numeric host) match the new `netguard.ErrInvalidURL`; the sink makes them permanent
  (`webhook: <origin>: invalid URL (scheme or host not allowed)`). The sink's `Validate` and the
  watch-file loader (so the API too) refuse non-canonical numeric hosts up front. DNS failures stay
  retried.
- **Transport errors (extends §5's "never the resolved IP address").** A request that got no response
  fails with a fixed class plus scheme and host: `connection refused`, `connection reset`, `timeout`,
  `DNS lookup failed`, `TLS handshake failed`, `canceled` or `request failed`. The dialled address and
  port, the URL path and the cause's text never reach the sink's error or `last_error`; the cause stays
  in the error chain (`errors.Is`). netguard's own `angelia.http` span still records the cause, refused
  addresses included: spans are operator-side.
- **Unsigned sinks (decided question 5).** Besides the load-time warnings, the sink logs one warning
  per sink id, the first time the process delivers for it, through `webhook.Options.Logger`;
  `sink.Request` gains `SinkID` for this. This covers API-created sinks, whose PUT drops warnings. The
  log names the sink id and scheme://host only.
- **D12 at delivery.** `webhook.Options.AllowHTTPHosts` (the instance's `http.allow_http_hosts`) is
  checked on every delivery: `http://` to a host not on it is permanent. Removing a host therefore stops
  cleartext even for sinks stored while it was listed. It reaches the sink the same way as `Allow`
  reaches netguard: wired with the dispatcher role (#198).
- **Timestamp.** `webhook-timestamp` is read just before the request is handed to the netguard client,
  before its per-host rate-limit wait. The wait is bounded by the delivery deadline (at most 15 s with
  the defaults), so the timestamp can trail the actual send by that much. This is far inside the 5 min
  tolerance, and moving the read after the wait would need a hook inside netguard.
- `http.allow_networks` refuses IPv4-mapped prefixes (`::ffff:a.b.c.d/n`), which would never match.

## Amendment (2026-11-06): wired by #198

No decision above changes. `angelia serve` now runs the scheduler and dispatcher roles. It builds one
netguard client with `Allow: cfg.HTTP.AllowPrefixes()`, shared by the sources and the sinks, and
registers `webhook.New` with `AllowHTTPHosts: cfg.HTTP.AllowHTTPHosts` (`cmd/angelia/roles.go`). The
follow-ups to #198 noted above are therefore done: `http.allow_networks` is what the running process may
dial, and D12 is checked at every delivery.
