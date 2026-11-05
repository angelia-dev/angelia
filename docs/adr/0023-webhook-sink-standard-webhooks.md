# ADR 0023: The webhook sink posts structured CloudEvents signed with Standard Webhooks v1, reads its secrets from the environment, and dials only through netguard

- **Status:** Proposed
- **Date:** 2026-11-05
- **Story:** angelia-dev/angelia#108

> Design note for a **Human-led** story: nothing here is implemented yet. The maintainer reviews the
> crypto, secret and SSRF choices (sections 2 to 5) and the open questions before the code is written.

## Context

[ADR 0005](0005-standard-webhooks-signing.md) chose Standard Webhooks for signing and left the header
values, the secret format and rotation open. The pieces around the sink now exist:

- The sink contract ([ADR 0017](0017-dispatcher-delivery-leases-and-retries.md) §7–§10, `internal/sink`):
  `Deliver(ctx, Request)` gets the decrypted `Config`, the stored event JSON, the `Rendered` body and
  content type, and `IdempotencyKey` = `<event id>:<subscription id>`. `sink.HTTPStatus` classifies a
  response, `sink.Permanent` stops retries, `sink.RedactURL` cleans `last_error` and logs.
- The dispatcher bounds one attempt with `DefaultDeliverTimeout` = 15 s and the finishing store write
  with `writeTimeout` = 8 s (`internal/dispatch/dispatch.go`), both below the 25 s drain timeout.
- The watch file and the OpenAPI spec already define the `webhook` config
  ([watch-file.md §2.4](../watch-file.md), spec `WebhookSinkConfig`): `url`, `secret_env`, `timeout`
  (default 10 s, `1s`–`2m`), `additionalProperties: false`. Decision D14 stores the *name* of the
  environment variable, never the value; decision D12 allows `http://` only for allowlisted hosts. Spec
  docs/api.md decisions 25 and 26: an API-set `*_env` name must be in `api.allowed_env`; a key marked
  `x-angelia-secret` is masked as `[REDACTED]` and refused in writes.
- netguard (#49) gives an `*http.Client` with a URL-level SSRF guard (`Config.Allow` prefixes, no YAML
  key yet), a 10 MiB body cap, a 30 s per-attempt timeout and no retries for `POST`. It follows up to 10
  redirects unless the caller overrides `CheckRedirect`. It does **not** check the dialled address:
  DNS rebinding is #76 (open).
- Secrets at rest are #75 (open, Human-led); a parallel design note, ADR 0024, designs secret storage.
  architecture.md [§8](../architecture.md#8-security) requires secrets encrypted at rest and never
  returned by the API.

The Standard Webhooks spec (fetched 2026-11-05) signs `msg_id.timestamp.payload` with HMAC-SHA256,
serialises secrets as `whsec_` + base64 of 24 to 64 random bytes, sends `v1,<base64>` entries
space-separated in `webhook-signature` for rotation, and says the id and timestamp must not contain `.`.

## Decision

The `webhook` kind lives in `internal/sink/webhook`. Proposed decisions, numbered to match the brief.

### 1. Wire format

- `POST` to `config.url`. Body: `Rendered.Body`, byte for byte. Without a template that is the stored
  CloudEvents JSON (structured mode) and `Content-Type: application/cloudevents+json; charset=utf-8`
  (`sink.CloudEventsContentType`); with a template, `Rendered.ContentType`. The signed bytes are the sent
  bytes; the sink never re-serialises.
- `webhook-id` = `Request.IdempotencyKey`, i.e. `<event ULID>:<subscription ULID>`. It is the same on
  every attempt and on replay, so receivers deduplicate on it as the spec recommends. Both parts are ULIDs
  (Crockford base32), so the id never holds `.` or white space; the sink still rejects such a key as a
  permanent error (defence in depth). Two subscriptions to the same URL get two ids, which is right: they
  are two deliveries. A receiver that wants per-event deduplication uses the CloudEvents `id` in the body.
- `webhook-timestamp` = Unix seconds of **this attempt** (the dispatcher's injected clock, read just
  before sending), as the spec requires. A retry 12 h later is fresh, so receiver tolerance still works.
- `webhook-signature` = one `v1,<std base64 HMAC-SHA256(key, id + "." + timestamp + "." + body)>` per
  active secret, joined by a single space, in configuration order. Absent when no secret is configured
  (unsigned delivery, already allowed by the contract with a `config check` warning).
- **No `ce-*` headers.** They belong to CloudEvents binary mode; in structured mode the attributes are
  in the body, and duplicating them invites receivers to read the unsigned copy. Binary mode can be a
  later, explicit config option.
- `User-Agent` comes from netguard. No `Accept`, no cookies, `Expect` not set.
- **Custom headers**: a new optional `headers` map (string to string) in the webhook config. This is a
  public-contract change (watch file and `WebhookSinkConfig`), see open question 3. Rules:
  - names are RFC 9110 tokens, compared case-insensitively, at most 32 headers and 8 KiB in total;
  - forbidden names: `Host`, `Content-Length`, `Content-Type`, `Content-Encoding`, `Transfer-Encoding`,
    `Connection`, `Keep-Alive`, `Upgrade`, `TE`, `Trailer`, `Expect`, `Proxy-*`, `User-Agent`, `Cookie`,
    any `webhook-*`, any `ce-*`, and `Authorization` (see below);
  - values pass the existing `secretFree` / `config.LooksLikeToken` check and contain no CR, LF or NUL;
  - header values are returned by the API like other non-secret config.
  `Authorization` and other credential headers wait for stored secrets (#75): putting a bearer token in a
  plain map would store and return it. Until then a receiver authenticates Angelia by the signature.

### 2. Secret format

- Format: `whsec_` + standard base64 (padding accepted) of **24 to 64 decoded bytes**; anything else is
  an invalid secret. The sender requires the prefix, so a password pasted by mistake is caught.
  Minimum entropy is enforced by length only (192 bits when generated by a CSPRNG); a decoded key of a
  single repeated byte is also refused, as a cheap guard against placeholders. Real entropy cannot be
  measured, so the docs say "generate it, do not type it".
- **Always user-supplied in this story**, through `secret_env` (D14). Angelia helps generate one:
  `angelia webhook secret new` prints `whsec_` + base64 of 32 bytes from `crypto/rand` to stdout and
  stores nothing; that output is the only time Angelia shows it. Server-side generation, with the value
  returned once in the create response, belongs with stored secrets (#75, ADR 0024).
- A missing, empty or invalid secret at delivery time is a **retried** error, like a config decryption
  error in ADR 0017 §7, because the operator fixes it by setting the variable. `last_error` names the
  variable and the rule, never the value: `webhook: secret_env WH_CI: invalid secret (want whsec_ and
  base64 of 24-64 bytes)`.

### 3. Rotation

- The environment variable holds **one to three** secrets separated by single spaces. The sink signs
  with each, so `webhook-signature` carries one `v1,` entry per secret. Three covers current, previous
  and one emergency key during a planned rotation, and keeps the header under 200 bytes. More than three
  is an invalid secret. Duplicates are refused.
- Adding a secret: set `WH_CI="whsec_NEW whsec_OLD"`, restart (environment variables are read by the
  process; `serve` reads the value at each delivery, so a restart is the only step). Deliveries now carry
  both signatures; receivers that know either key verify.
- Retiring: once every receiver has the new key, set `WH_CI="whsec_NEW"` and restart.
- API and watch file: **no change**. Both carry only the variable name (`secret_env`), which is returned
  as-is. The API exposes no count or fingerprint of the keys in this story.
- After #75, rotation becomes API operations on stored keys (add a key, retire a key by id, each with an
  optional expiry) in ADR 0024's model; the "sign with every active key, at most three" rule stays.

### 4. Secret storage before and after #75

- **Today, #108 does not wait for #75.** The secret is never in the database, the watch file or the API:
  the sink row stores `secret_env` (a name) in `config_json_encrypted`, and the value is read from the
  process environment at delivery (D14). Nothing is plaintext at rest that Angelia writes.
- API: `secret_env` is not secret and is returned. API writes of it are gated by `api.allowed_env`
  (empty by default; `ANGELIA_*` always refused). The webhook config schema marks no key
  `x-angelia-secret` today; a future inline secret key must be marked, so ADR 0022's masking fails
  closed (masked for every caller, refused in writes).
- Logs and errors never contain the secret, the decoded key, the HMAC input or the signature header.
  The sink logs nothing itself; it returns errors built from fixed strings plus the variable name, the
  status code and a `RedactURL`-cleaned URL. Base64 decode errors are replaced, not wrapped.
- **Test that captures logs** (acceptance criterion 3): a dispatcher with a `slog` JSON handler at
  debug level writing to a buffer, one webhook sink, a known `whsec_` secret, and `httptest` receivers
  that answer 200, 500, 404, 429, a redirect, a timeout and a reset; plus an invalid secret and an unset
  variable. The test asserts that none of: the full secret, its base64 part, the hex of the key, any
  emitted `webhook-signature` value appears in the log buffer, in `delivery.last_error`, or in any
  returned error string.
- **After #75** (ADR 0024): `secret_env` stays valid; a stored-secret alternative is added by ADR 0024,
  not here. This ADR only fixes what a secret is (section 2) and how many sign (section 3).

### 5. SSRF

- Every request goes through the netguard client handed to the sink's constructor; `http.DefaultClient`
  is never used.
- Private, loopback, link-local, CGNAT, multicast and IPv4-embedding IPv6 targets are refused unless the
  address is in netguard's `Allow` prefixes. That list lives **only in the instance configuration**
  (`angelia.yaml`), set by the operator; the watch file and the API cannot widen it. It needs a new
  instance key, proposed `http.allow_networks` (CIDR list) beside D12's http allowlist, proposed
  `http.allow_http_hosts` (open question 6). A refused address (`netguard.ErrBlocked`) is **permanent**:
  it is a configuration fault, and replay exists once the operator allowlists it.
- **DNS rebinding**: the URL check resolves once, then the transport resolves again. The webhook is the
  highest-value SSRF target (a `POST` with a body to a URL an `admin` API caller chooses), so #108 adds
  the dial-time check that netguard's doc reserves for it: a `net.Dialer.Control` hook that runs
  `Guard.CheckAddr` on the address actually connected. When `HTTPS_PROXY` is in use the dialled address
  is the proxy's, so the check applies to the proxy and the proxy is trusted to filter. The rest of #76
  (allowlist config, redirect policy for sources) stays in #76. See open question 5.
- **Redirects are not followed**: the sink's client copies netguard's with `CheckRedirect` returning
  `http.ErrUseLastResponse`; a 3xx is permanent (maintainer decision on #169).
- **Timeouts**: connect 30 s and TLS handshake 10 s (netguard transport), all bounded by one context
  deadline of `min(config.timeout, the dispatcher's remaining DeliverTimeout)`. With the defaults that is
  10 s; a `timeout` above 15 s has no effect, so `config check` warns about it (open question 4).
- **Response body**: the sink reads at most 64 KiB and discards it (so the connection can be reused),
  then closes. netguard's 10 MiB cap is a backstop. Nothing of the body is stored.

### 6. Response handling

| Outcome | Dispatcher result |
|---|---|
| 2xx | delivered (`Result.StatusCode`) |
| 408, 429, 5xx, 1xx, other unexpected codes | retried; `Retry-After` (seconds or HTTP date) passed in `sink.StatusError.RetryAfter` |
| 3xx, other 4xx (incl. 401, 404, 410) | permanent, dead at once (ADR 0017 §7 and maintainer decision 3) |
| connection refused/reset, DNS failure, TLS error, timeout | retried |
| netguard SSRF refusal, invalid URL, `.` in the id | permanent |
| secret variable unset or invalid | retried (section 2) |

This is `sink.HTTPStatus` unchanged. `Retry-After` parsing reuses netguard's parser (exported as
`netguard.ParseRetryAfter`) rather than a second copy. The response body is **not stored**, even
truncated: it is receiver-controlled, may echo request data, and `last_error` is shown to `read` API
callers. `last_error` holds `webhook: <scheme://host>: HTTP <code> <text>`.

### 7. Verification

- `internal/sink/webhook` exports, next to `Sign`:

  ```go
  const DefaultTolerance = 5 * time.Minute
  func Verify(secrets []string, h http.Header, body []byte, now time.Time, tolerance time.Duration) error
  ```

  It accepts `whsec_`-prefixed or bare base64 secrets of any non-zero length (receiver side, like the
  reference library), requires the three headers, rejects a timestamp outside `now ± tolerance`, splits
  the signature header on spaces, ignores entries that are not `v1,` or not valid base64, and compares
  with `hmac.Equal`. Errors are sentinel values (`ErrMissingHeaders`, `ErrTimestamp`,
  `ErrNoMatchingSignature`). `now` is a parameter so tests are deterministic. Its users are the sink's own
  tests and an optional `angelia webhook verify` CLI; it is `internal/`, so external Go consumers use the
  official library.
- **Test vectors** (acceptance criterion 1) are vendored from the official repository
  `github.com/standard-webhooks/standard-webhooks` (Apache-2.0, per its `LICENSE`), pinned to a commit
  recorded in `internal/sink/webhook/testdata/standard-webhooks/README.md` with the licence notice:
  - signing: secret `whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw` (24 bytes), id
    `msg_p5jXN8AQM9LWM0D4loKWxJek`, timestamp `1614265330`, body `{"test": 2432232314}` gives
    `v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE=` (`libraries/go/webhook_test.go`,
    `TestWebhookSign`; recomputed independently for this note);
  - verification cases from the same file: wrong signature, empty `v1,`, multiple entries with one
    valid, garbage entries before a valid one, `v2,` prefix, trailing `,extra`, timestamp at and beyond
    the 5 min tolerance in both directions.
  A test runs every vector through both `Sign` and `Verify`, and a round-trip test signs Angelia-shaped
  deliveries (ULID id, CloudEvents body, three secrets) and verifies each secret alone.
- **Proposal only**: publish Angelia-shaped vectors (structured CloudEvents body, `webhook-id` with the
  `:` separator, rotation with two and three keys, negative cases) in the spec repository under
  `webhooks/v1/vectors.json`, CC-BY-4.0 like the schemas, so receivers in any language can test against
  them. That is a spec-repo story; the reference vectors stay Apache-2.0 under core `testdata/`.

## Consequences

### Positive

- Receivers use any Standard Webhooks library unchanged; `webhook-id` is a real idempotency key.
- #108 ships without pre-empting #75: no secret value ever reaches the database or the API.
- Rotation works today with no contract change, only a documented format for the variable's value.
- Webhooks get the dial-time SSRF check before they can be configured through the API.

### Negative

- Rotation and secret changes need a process restart while secrets come from the environment.
- No `Authorization` or other credential header until stored secrets exist.
- A refused SSRF target, a redirect and a 410 are dead at once; the operator must replay.
- The sender requires `whsec_` while the reference library tolerates bare base64; a user who copies a
  bare key from another tool must add the prefix.

### Open questions

Numbered for the maintainer; each has a recommendation.

1. **`webhook-id` value.** (a) the idempotency key `<event>:<subscription>` (recommended: stable across
   retries and replay, distinct per subscription); (b) the event id (receivers with two subscriptions to
   one URL would drop one); (c) a per-attempt id (defeats deduplication).
2. **Rotation format.** (a) up to three space-separated secrets in the one `secret_env` value
   (recommended: no contract change, mirrors the header); (b) a list key `secret_envs` (contract change,
   clearer in `config check` output). Is three the right maximum?
3. **Custom headers.** (a) add `headers` (non-secret values only, forbidden list above) in #108 as an
   additive change to `WebhookSinkConfig` and the watch file (recommended, it is in the issue's scope);
   (b) defer all custom headers to #75 together with credential headers.
4. **`timeout` above 15 s.** The contract allows `2m` but the dispatcher cuts an attempt at 15 s.
   (a) keep the contract, apply `min`, warn in `config check` (recommended); (b) lower the maximum to
   `15s` (contract change); (c) raise `DeliverTimeout` and the lease TTL (touches the 25 s drain bound).
5. **DNS rebinding.** (a) #108 adds the netguard `Dialer.Control` check (recommended, small, and
   netguard's doc already reserves the hook); (b) block #108 on #76; (c) ship with the documented gap.
6. **Allowlist keys.** Name and shape of the instance keys: `http.allow_networks` (CIDRs) and
   `http.allow_http_hosts` (hostnames, D12), shared by sources and sinks (recommended), or sink-only
   keys. This is an instance-config contract change.
7. **Unsigned deliveries.** Keep `secret_env` optional with a warning (recommended, already the
   contract), or require it.
8. **Spec-repo vectors.** Approve the proposal in section 7 as a follow-up spec story?

## Alternatives considered

- **Store the secret in the sink config now, plaintext or with an ad hoc key.** It would pre-empt #75 and
  ADR 0024 and break architecture.md §8. Rejected.
- **Wait for #75 before #108.** Blocks delivery on a Phase 1 story while D14 already gives a safe path.
- **Asymmetric `v1a` (ed25519) signatures.** Better when the receiver is untrusted, but needs key
  distribution; HMAC is what ADR 0005 and the issue name. Can be added later as another entry type.
- **Binary-mode CloudEvents (`ce-*` headers, data-only body).** Splits the event between signed body and
  unsigned headers. Rejected as the default.
- **Storing a truncated response body for debugging.** Receiver-controlled content shown to `read`
  callers; the status code is enough to act on.

## References

- [ADR 0005](0005-standard-webhooks-signing.md), [ADR 0017](0017-dispatcher-delivery-leases-and-retries.md),
  [ADR 0022](0022-rest-api-in-house-spec-driven-validation.md); ADR 0024 (secrets at rest, #75, drafted
  in parallel)
- [architecture.md §3.5](../architecture.md#35-dispatcher-and-sink-plugins-deliver),
  [§8](../architecture.md#8-security); [watch-file.md](../watch-file.md) §2.4, D12, D14;
  [configuration.md](../configuration.md) (`api.allowed_env`)
- Spec repository: docs/api.md decisions 25–27, `openapi/angelia.v1.yaml` `WebhookSinkConfig`
- Issues: #108, #75, #76, #49, #107
- Standard Webhooks specification, `spec/standard-webhooks.md`, and reference library
  `libraries/go/webhook_test.go`, github.com/standard-webhooks/standard-webhooks (Apache-2.0)
- CloudEvents HTTP protocol binding 1.0 (structured and binary content modes)
