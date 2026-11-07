# ADR 0026: The shoutrrr sink takes its whole service URL as a secret; the core checks the HTTP destinations derived from it, and every request goes through netguard

- **Status:** Accepted (2026-11-07, maintainer decision on #120: all seven recommendations)
- **Date:** 2026-11-06
- **Story:** angelia-dev/angelia#120

> Written as the design note for #120 before any code. #120 is labelled `Agent`, but its first
> criterion contradicted ADR 0024 §1, and the maintainer chose option A on the issue: a design note
> that makes shoutrrr the "whole-URL secret" kind that ADR 0024 §1 anticipates. The maintainer
> accepted it on 2026-11-07 with every open question settled as recommended (see Decided
> questions). The config shape lands in a separate spec PR.

## Context

Issue #120 asks for a `shoutrrr` sink (Slack, Discord, Telegram, ntfy, Gotify, …) through the
maintained fork named in [architecture.md §6](../architecture.md#6-language-and-framework-phase-0-assumptions).
Its first acceptance criterion is "Shoutrrr URL stored as a secret".

[ADR 0024](0024-secrets-encrypted-at-rest.md) §1 says the opposite for chat sinks: "a plain base URL
… plus a referenced token …; no whole-URL secret". Its alternatives list rejects `url_ref`
because it hides the host. Hidden hosts escape write-time validation, the diff and `url_redacted`,
and they make host binding impossible. The same §1 also leaves a door open. A future kind that
needs a whole URL as a secret marks it in its schema, and the core validates the resolved URL at
delivery: scheme `https`, no userinfo, and the host allowed by netguard and by the secret's
`hosts`.

A shoutrrr URL does not split into a base URL and a token:

- the scheme picks the service (`slack://`, `ntfy://`);
- the userinfo, host and path carry service-specific parts. Credentials can sit in the userinfo
  (`telegram://TOKEN@telegram`, `ntfy://user:pass@host/topic`, `matrix://user:pass@host`). They
  can sit in the path (`gotify://host/TOKEN`). They can sit in pseudo-hosts too
  (`slack://hook:A-B-C@webhook`, `discord://TOKEN@WEBHOOK_ID`);
- for several services the URL host is **not** the HTTP host. `telegram://…@telegram` posts to
  `api.telegram.org`, and `slack://…@webhook` posts to `hooks.slack.com`.

So the "no userinfo, host = URL host" check of ADR 0024 §1 cannot be applied to the shoutrrr URL
itself.

What the code has today (main at `d980345`):

- `internal/dispatch.ResolveSecrets` resolves every `*_env` / `*_ref` key into
  `sink.Request.Secrets` (`map[string]secrets.Value`, keyed by JSON Pointer). A **bound** stored
  secret is resolved only for the URL that the kind's `config_schema` marks
  `x-angelia-destination: true`, and `secrets.Resolver.Resolve` checks `hosts` **before**
  decrypting. A kind that marks no destination gets unbound secrets only.
- `secrets.Bound.Allows(u)` compares the canonical `host:port` of an http(s) URL with `hosts`.
- netguard gives the shared `*http.Client`: URL-level SSRF check on every hop, dial-time check
  (ADR 0023 §5), per-host limiter, body cap, and `http.allow_http_hosts` / `http.allow_networks`.
  Spans and the metrics observer never carry a sink's host or URL.
- The webhook sink (ADR 0023) is the reference sink. Its client follows no redirect, a refused
  destination is permanent, transport errors become fixed classes, and no receiver body is stored.

### The library, as checked for this note

`github.com/nicholas-fedor/shoutrrr` **v0.23.0** (tag `52a642b`, released 2026-11-03), read from
the module cache:

- **Licence: MIT** (`LICENSE.md`, "Copyright (c) 2026 Nicholas Fedor"). It is allowed by
  AGENTS.md.
- **`go 1.27`**, the same as Angelia's `go.mod` (`go 1.27.0`, toolchain `go1.27.3`).
- **Custom HTTP client: supported.** `types.HTTPClientSetter.SetHTTPClient(types.HTTPClient)`,
  where `types.HTTPClient` is `Do(*http.Request) (*http.Response, error)`. It is implemented by
  all six candidate services (`chat/slack`, `chat/discord`, `chat/telegram`, `push/ntfy`,
  `push/gotify`, `chat/matrix`), and each has `SendContext(ctx, message, params)`. The services
  hold the client in `internal/httpclient.Holder`. An injected client is used for every request,
  **including** when the URL sets `skiptlsverify=yes`: `Holder.InsecureClient` returns the
  injected client unchanged ("its owner controls its TLS settings"). The hard-coded
  `&http.Client{…}` sites are deprecated fallbacks (`DefaultHTTPClient` in discord and matrix)
  that run only when nothing is injected. In **v0.22.1** the client handling is different (no
  `Holder`, a per-service `newDefaultHTTPClient`), so this note applies only to v0.23.0 and later.
- **Non-HTTP services** (`smtp`, `xmpp`, `mqtt`) take a `DialContextSetter` instead. They are out
  of scope here.
- **Link footprint.** Importing only the six service packages, not `router` or the root package,
  links shoutrrr's own packages plus `mattn/go-colorable` (MIT, already indirect),
  `mattn/go-isatty` (MIT, already indirect), `golang.org/x/sys` (BSD-3, already indirect) and
  **`golang.org/x/term`** (BSD-3, new). The module graph and `go.sum` also gain the rest of its
  requirements (paho MQTT, mellium XMPP, `x/oauth2`, cobra, testify, …), but none of them is
  linked. `CGO_ENABLED=0 go build` of a scratch module importing the six packages succeeds.
  `router` imports all 33 services.
- **There is no `custom` scheme in this fork.** Arbitrary HTTP targets go through `generic`
  (`generic+https://…`).

What each candidate service sends (v0.23.0 source):

| Scheme | URL shape | HTTP destination | Host chosen by | Requests per send |
|---|---|---|---|---|
| `slack` | `slack://hook:A-B-C@webhook` | `https://hooks.slack.com/services/…` | fixed | 1 |
| `slack` | `slack://xoxb:…@CHANNEL` | `https://slack.com/api/chat.postMessage`, token in `Authorization` | fixed | 1 |
| `discord` | `discord://TOKEN@WEBHOOK_ID` | `https://discord.com/api/v10/webhooks/ID/TOKEN` | fixed | 1 per chunk; its own retries on transport errors and 429 |
| `telegram` | `telegram://TOKEN@telegram?chats=…[&apiurl=…]` | `https://api.telegram.org/bot<TOKEN>/sendMessage`, or the `apiurl` host | fixed, or user (`apiurl`) | 1 per chat |
| `ntfy` | `ntfy://[user:pass@][host[:port]]/topic` | `https://host/topic` (default `ntfy.sh`); `http` with `disabletls=yes` | user | 1 |
| `gotify` | `gotify://host[:port][/path]/TOKEN` | `https://host/path/message`; `http` with `disabletls=yes` | user | 1 |
| `matrix` | `matrix://[user]:PASSWORD_OR_TOKEN@host[:port]/?rooms=…` | `https://host/_matrix/client/…`; `http` with `disabletls=yes` | user | login (with a user), then join and send per room |

All six use HTTP only, and all six accept the injected client.

## Decision

### 1. Configuration: the whole URL is the secret

The `shoutrrr` kind's `config` (proposed spec shape in §7):

```yaml
sinks:
  - name: releases-chat
    kind: shoutrrr
    config:
      url_ref: slack-releases     # a stored secret (ADR 0024) holding the whole shoutrrr URL
      # url_env: CHAT_URL         # or an environment variable; exactly one of the two
      service: slack              # optional, see decided question 3
      timeout: 10s
```

- **Exactly one** of `url_ref` / `url_env` is required. Each holds one shoutrrr URL. Lists of
  URLs (the router's fan-out) are not supported: one sink is one destination, so retries and
  `last_error` stay per destination.
- Both follow the reserved-suffix rules of ADR 0024: the dispatcher resolves them into
  `Request.Secrets["/url_ref"]` (or `"/url_env"`) as a `secrets.Value`. The value is never
  logged, traced, stored in `last_error`, returned by the API or put in an event. The sink calls
  `Reveal()` once, to parse it.
- The secret's format is "one shoutrrr URL, surrounding white space trimmed". A stored secret
  written with `angelia secret set --from-file` already loses one trailing newline.
- `config_schema` marks both keys with a **new annotation, `x-angelia-secret-destination: true`**:
  "the resolved value of this reference is the destination". `x-angelia-destination` keeps its
  meaning (a plain URL property). The kind marks no `x-angelia-destination`.

### 2. Resolution and host binding for a secret destination

ADR 0024 checks `hosts` before it decrypts, against a URL that is visible in the config. Here the
destination is only known after decrypting, so a reference marked `x-angelia-secret-destination`
follows a different order. The secret is still checked before any byte of it leaves the process.

1. **Resolve without the pre-check.** The dispatcher resolves the reference with a new
   `secrets.Resolver.ResolveSelfAddressed(ctx, name) (Bound, error)`, which decrypts and returns
   the value **with its `hosts`**. `*_env` resolves as today, unbound. All other references in the
   same config keep the ADR 0024 path.
2. **Derive the destinations (no I/O).** The core calls a new optional sink interface (additive
   to ADR 0017, in the manner of ADR 0021's `Canonicalizer`):

   ```go
   // SecretDestinations is implemented by kinds whose config_schema marks a
   // reference x-angelia-secret-destination. It parses the resolved value
   // and returns the origins (scheme://host[:port]) the sink will send to.
   // It does no I/O, and its errors never contain the value.
   type SecretDestinations interface {
       SecretDestinations(ptr string, v secrets.Value) ([]*url.URL, error)
   }
   ```

   A kind that marks the annotation but does not implement the interface fails every delivery.
   This fails closed.
3. **Check each origin** before `Deliver`:
   - `https`, or `http` only when the host is in `http.allow_http_hosts` (D12);
   - netguard's URL-level guard (`Guard.CheckURL`, through a new exported
     `netguard.Client.CheckURL`);
   - `Bound.Allows` against the secret's `hosts`, when it has any. Every origin must match one
     entry. **`hosts` stays optional** for sink secrets (ADR 0024 §2); `config check` warns
     about a `url_ref` whose secret has none (decided question 4).
4. **Pass them on.** `sink.Request` gains `Destinations []string`, the canonical `host:port`
   list that was checked. The sink pins its client to it (§4).

**Failures** from steps 1–3 happen before `Deliver` and are **retried** (ADR 0024 decided
question 9 and the §1 clause). The operator fixes them by rewriting the secret, not the sink, so
an SSRF refusal here is retried, unlike ADR 0023 §5 (decided question 5). This covers an
unparsable value, a scheme outside the allowlist, a refused option, a host netguard refuses and a
host binding mismatch. `last_error` names the pointer, the secret or variable name, the **service
name** and the **origin** (`scheme://host[:port]`). It never names the shoutrrr URL, its path,
query or userinfo, or the text of a `url.Parse` or shoutrrr error, since both quote their input.

What "no userinfo" means here: ADR 0024 §1's rule applies to the **derived HTTP origins**, which
never carry userinfo (ntfy basic auth goes in a header). It does not apply to the shoutrrr URL,
where userinfo is the token's normal place.

### 3. Service allowlist and refused options

`SecretDestinations` (in `internal/sink/shoutrrr`) accepts only these schemes. Each derivation is
a small in-house function, tested against the URLs that shoutrrr itself builds:

| Scheme | Origin(s) | Notes |
|---|---|---|
| `slack` | `https://hooks.slack.com` (webhook token) or `https://slack.com` (`xoxb`/`xoxp`) | fixed |
| `discord` | `https://discord.com` | fixed |
| `telegram` | `https://api.telegram.org`, or the `apiurl` origin | `apiurl` is user-chosen, so it is checked like any host |
| `ntfy` | URL host (default `ntfy.sh`) | `disabletls=yes` or the deprecated `scheme=http` gives `http://` (decided question 7) |
| `gotify` | URL host | `disabletls=yes` gives `http://` |
| `matrix` | URL host | access-token form only in v0.1 (decided question 2) |

- **Refused**: every other scheme, including `generic` (arbitrary HTTP; the `webhook` kind covers
  it), `smtp`, `xmpp` and `mqtt` (non-HTTP, netguard cannot guard them) and the rest of the
  router's list. Adding a scheme later is an additive change. It needs its derivation function,
  its injection test (§4) and a docs entry.
- **`skiptlsverify=yes`** (and its deprecated aliases) is refused. With an injected client the
  library ignores it and verifies TLS anyway, so accepting it would mislead the operator.
  Self-signed servers go through the system trust store.
- Comparison is case-insensitive on the scheme. The parse uses `url.Parse` on the revealed bytes,
  and any error is replaced by a fixed message.

### 4. HTTP goes through netguard, pinned to the checked origins

- The sink's constructor takes the netguard `*http.Client`, as the webhook does. For **each
  attempt** the sink:
  - creates a **new** service value (no cache across deliveries: the secret can change between
    attempts, and matrix keeps a session token in the service);
  - calls `Initialize(url, discardLogger)`. Shoutrrr's own logging (deprecation and insecure
    warnings) can quote config values, so it is discarded;
  - calls `SetHTTPClient(pinned)` and `SendContext(ctx, rendered body, nil)`.
- **`pinned`** is an `*http.Client` whose transport wraps the netguard client:
  - it refuses, without sending, any request whose canonical `host:port` is not in
    `Request.Destinations`. This is the per-request re-check that ADR 0024 §2 requires before a
    sink sends a bound secret in more than one request (matrix, telegram per chat, discord per
    chunk);
  - it records the status and `Retry-After` of the last response;
  - its `CheckRedirect` returns `http.ErrUseLastResponse` (no redirects, ADR 0017 §7).

  The netguard dial-time check, limiter and body cap apply underneath, unchanged.
- **Injection tests** for each allowed service (they also guard library upgrades):
  - `http.DefaultTransport` is replaced with a failing transport for the test, so a request that
    escapes the injected client fails the test;
  - an `httptest` server receives the request through the pinned client, and the test checks the
    path and body;
  - a URL whose derived host differs from where the library actually sends is refused by the pin.
- **Deadline**: `min(config.timeout, the remaining dispatcher DeliverTimeout)`, as ADR 0023 §5
  does. The library's own retries (discord transport errors and 429, matrix rate limits) run
  inside that deadline.

### 5. Results and errors

- The outcome is classified from **what the pinned transport observed**, not from shoutrrr's
  error types, which differ per service. The steps, in order:
  - a non-2xx last status goes through `sink.HTTPStatus(code, retryAfter)`, so 3xx and 4xx are
    permanent and 408, 429 and 5xx are retried;
  - a transport error is retried, with the webhook's fixed transport classes (`connection
    refused`, `timeout`, `TLS handshake failed`, …);
  - a refusal by the pin is permanent. It means the derivation is out of step with the library;
  - any other library error after 2xx responses (for example telegram `ok:false`) is retried, as
    `service error`;
  - a template or body problem is permanent.
- `last_error` and logs hold `shoutrrr: <service> <scheme://host[:port]>: <class or HTTP status>`.
  The library's error is **not wrapped**: its text and chain can carry response bodies, URLs or
  tokens. `Result.StatusCode` is the last observed status.
- **Partial sends** (several telegram chats, several matrix rooms, split discord messages) are not
  atomic. A retry after a partial failure sends to every target again. This is at-least-once
  delivery, as for every sink, and the docs recommend one target per sink when duplicates matter.

### 6. Message

- The message is `Rendered.Body` as text. When the subscription has no template, the shoutrrr
  kind gets a **built-in plain-text default per event type** (acceptance criterion 2), rendered by
  the ADR 0016 engine instead of the CloudEvents JSON. For example `{{.package}} {{.version}}
  released ({{.bump}})` with the release URL, and a yank wording for `release.yanked`. The
  wording ships in `docs/templates.md`, since it is not a schema contract.
- No shoutrrr `params` in v0.1. Title, priority, tags and similar settings come from the URL's
  query, which is part of the secret.

### 7. Proposed spec shape (separate spec PR, `Human-led`)

```yaml
ShoutrrrSinkConfig:
  description: The `config` of a `shoutrrr` sink. The whole shoutrrr service URL is a secret, named by exactly one of `url_ref` and `url_env`. Allowed services: slack, discord, telegram, ntfy, gotify, matrix (access token).
  type: object
  additionalProperties: false
  oneOf:
    - required: [url_ref]
    - required: [url_env]
  properties:
    url_ref:
      description: Name of a stored secret (`/v1/secrets`) whose value is one shoutrrr URL. Its `hosts`, when set, must contain every HTTP host the service sends to.
      type: string
      pattern: '^[a-z0-9][a-z0-9._-]{0,62}$'
      x-angelia-secret-destination: true
    url_env:
      description: Name of an environment variable of the instance whose value is one shoutrrr URL.
      type: string
      pattern: '^[A-Za-z_][A-Za-z0-9_]*$'
      x-angelia-secret-destination: true
    service:
      description: Optional. When set, the URL's scheme must equal it; it makes the service visible without the secret.
      type: string
      enum: [slack, discord, telegram, ntfy, gotify, matrix]
    timeout:
      $ref: '#/components/schemas/Duration'
      description: Per-attempt timeout, `1s` to `2m`.
      default: 10s
```

The spec PR also documents `x-angelia-secret-destination`. Under spec decision 26 it adds no
masking: the keys hold names, which are returned like `secret_ref`. The spec PR also adds the
kind to the sink kind docs and states that admin scope is needed to change `url_ref` (ADR 0024
§2, unchanged).

### 8. Dependency

Add `github.com/nicholas-fedor/shoutrrr` pinned at **v0.23.0** (MIT, already in architecture.md
§6). Only the six service packages and `pkg/types` are imported, never `router` or the root
package. A test that runs `go list -deps` on the sink package and fails on any other shoutrrr
package enforces this. A `depguard` rule would do the same, but the lint config has none yet. The new linked module is `golang.org/x/term`
(BSD-3). `govulncheck` runs over the linked code as usual. Any version bump re-runs the §4
injection tests, which are the guard against a regression in the client handling.

## Consequences

### Positive

- Criterion 1 is met as written. Operators paste the shoutrrr URL that the library documents, and
  Angelia does not re-specify six URL formats in its public contract.
- Every HTTP request of the sink goes through netguard (SSRF, dial-time check, limiter, body cap)
  and is pinned to origins that were checked against the allowlist and the secret's `hosts`.
- No fork and no in-house senders: the library's injection point is enough.
- The pattern (`x-angelia-secret-destination`, `SecretDestinations`, `Request.Destinations`) is
  generic, so a future Apprise-style kind can reuse it.

### Negative

- The destination host is **not visible** in the sink config, the diff, `url_redacted` or
  write-time validation (the trade-off ADR 0024 rejected for chat sinks in general). Mitigations:
  - the secret's `hosts` (shown by `GET /v1/secrets`);
  - the optional `service` key;
  - `last_error`, which shows the origin.

  A wrong URL is found only at the first delivery.
- New public contract:
  - the `shoutrrr` config schema;
  - the `x-angelia-secret-destination` annotation;
  - new Go contracts in core: `sink.SecretDestinations`, `sink.Request.Destinations`,
    `secrets.Resolver.ResolveSelfAddressed` and `netguard.Client.CheckURL`.

  The netguard and secrets changes are on sensitive paths.
- A bound secret is decrypted before its host check for this kind (it never leaves the process
  first). That is a weaker ordering than ADR 0024 §2 for other kinds.
- The derivation table duplicates knowledge of the library's endpoints. The pin turns drift into
  a permanent, visible error rather than a leak, but upgrades need the injection tests.
- The `go.sum` and module graph grow by the library's unlinked requirements, and Dependabot will
  raise PRs for them.
- Partial multi-target sends can duplicate on retry.

### Decided questions

The maintainer accepted every recommendation on #120 (2026-11-07).

1. **Agreement with ADR 0024:** a dated Amendment to 0024 points here, and 0024 stays Accepted.
   Only one table row of 0024 §1 and its rejected alternative change. 0024 is not marked
   superseded.
2. **Matrix:** only the access-token form (`matrix://:TOKEN@host`) is allowed in v0.1. A URL with
   a user name is refused by `SecretDestinations` (retried, §2). It would log in on every delivery,
   because the service is not cached, and send the password on every attempt.
3. **`service` key:** optional. When it is set, the URL's scheme must equal it; a mismatch fails
   before `Deliver` (retried, §2). It makes sink lists readable without the secret.
4. **`hosts` for `url_ref` secrets:** optional, as for other sink secrets. `config check` warns
   about a `url_ref` whose secret has no `hosts`.
5. **SSRF refusal of a derived origin:** retried, like every other failure that comes from the
   secret's value. The fix is a secret rewrite, which then takes effect without a replay. The
   delivery still goes dead after the normal attempts.
6. **Library version:** pinned at v0.23.0, guarded by the §4 injection tests. v0.22.1 is not
   used.
7. **`http` destinations** (`disabletls=yes`, ntfy's deprecated `scheme=http`): allowed only when
   the host is in `http.allow_http_hosts`, like D12 for webhooks, and `config check` warns about
   them because credentials then travel in clear text.

## Implementation notes (2026-11-07, #120)

The implementation keeps the decision above. It records three points where the code is more specific
than the text, accepted in the #120 review:

- **Origins are derived through the library's own parser** (§3). `SecretDestinations` builds a new
  value of the service and calls its `Initialize` with a discarded logger, which does no I/O in v0.23.0.
  It then reads the parsed configuration: the slack token type, telegram `apiurl`, ntfy `GetAPIURL`,
  gotify `DefaultURLBuilder`, matrix host, `disabletls` and the `skiptlsverify` fields. The URL is
  parsed once, by the library, so Angelia and the library cannot read it differently. A test runs every
  derivation with `http.DefaultTransport` set to fail, so an upgrade whose `Initialize` sends a request
  is caught.
- **The `service` check runs at the start of `Deliver`** (decided question 3), not in the core. The §2
  interface passes no configuration to `SecretDestinations`. The check still runs before any request
  and is still retried.
- **The core's check errors name the sink kind, not the service** (§2). The dispatcher does not know
  the service. Its errors read `shoutrrr: secret /url_ref (secret NAME): https://host: <reason>`. A
  refusal by the sink's own derivation adds the service (`shoutrrr: secret /url_ref (secret NAME):
  slack: ...`). A refused scheme is named only when it is one of the router's known schemes.
- **Private CAs** (§3, `skiptlsverify`). Since #208, `http.ca_file` adds CAs to the netguard client
  that the sink sends through, so a self-signed server is trusted that way rather than through the
  system trust store. `skiptlsverify=yes` stays refused.

## Alternatives considered

- **B: base URL plus `token_ref`, as ADR 0024 §1 says.** The sink would build the URL from plain
  fields and referenced parts. Every service has its own token layout (slack's three parts, a
  telegram token plus chats, a discord id plus token, gotify's path token, matrix's user and
  password), so the spec would have to define per-service config shapes. That is a large public
  contract that mirrors the library's URL format and must follow its changes. Criterion 1 would
  have to be amended. The hosts would stay visible, which is its real advantage. Not chosen
  (maintainer decision for option A on #120).
- **C: defer #120** until a whole-URL secret mechanism exists as its own story. That blocks a
  Phase 1 MVP sink (prd.md §9) on a mechanism this note specifies in about the same effort.
- **Use the router** (`shoutrrr.CreateSender` with `SenderOptions.HTTPClient`). It links all 33
  services and their dependencies (MQTT, XMPP, OAuth2 and GCP metadata), and it accepts any scheme.
  Calling the six service packages directly keeps the allowlist structural.
- **Fork the library, or write own minimal senders.** These would be needed only if the services
  did not accept a client. v0.23.0 accepts one in all six, so neither is worth the maintenance.
  Own senders stay the fallback if a later version drops injection.
- **Trust the library's errors for classification.** Their types and texts differ per service, and
  some carry response bodies or URLs. Observing the exchange in the pinned transport gives one
  classification and safe messages.
- **Check the shoutrrr URL's own host.** For slack, discord and telegram that host is a placeholder
  (`webhook`, a webhook id, `telegram`), not where the request goes. The check must apply to the
  derived origins.

## References

- Issue angelia-dev/angelia#120 and the maintainer's option A in its comments; epic #11
- [ADR 0016](0016-message-templates-text-template-bounded.md), [ADR 0017](0017-dispatcher-delivery-leases-and-retries.md),
  [ADR 0021](0021-source-canonical-target.md), [ADR 0023](0023-webhook-sink-standard-webhooks.md),
  [ADR 0024](0024-secrets-encrypted-at-rest.md) §1, §2, §6 and its amendment
- [architecture.md §3.5](../architecture.md#35-dispatcher-and-sink-plugins-deliver),
  [§6](../architecture.md#6-language-and-framework-phase-0-assumptions),
  [§8](../architecture.md#8-security), [§10](../architecture.md#10-open-questions-for-phase-0) question 4
- Code: `internal/dispatch/secrets.go`, `internal/secrets/{resolver,hosts}.go`,
  `internal/netguard/client.go`, `internal/sink/sink.go`, `internal/sink/webhook`
- Spec repository: `openapi/angelia.v1.yaml` `WebhookSinkConfig`, docs/api.md decisions 25 and 26
- `github.com/nicholas-fedor/shoutrrr` v0.23.0 (`52a642b`): `LICENSE.md`, `pkg/types/http_client.go`,
  `pkg/types/sender_options.go`, `internal/httpclient/httpclient.go`, `pkg/router/servicemap.go`,
  `pkg/services/{chat/slack,chat/discord,chat/telegram,push/ntfy,push/gotify,chat/matrix}`
