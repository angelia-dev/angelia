# Webhook sink

The `webhook` sink POSTs each delivery to a URL, signed per
[Standard Webhooks](https://www.standardwebhooks.com/) v1. Design: [ADR 0023](adr/0023-webhook-sink-standard-webhooks.md).
Configuration keys: [watch-file.md §2.4](watch-file.md#24-sinks) (`url`, `secret_env` or `secret_ref`, `timeout`).

## For operators

### Signing secret

1. Generate a secret. Do not type one: a hand-made key is guessable.

   ```
   angelia webhook secret new
   whsec_3q2+7wAAb0...=
   ```

   It prints `whsec_` plus the base64 of 32 random bytes. Nothing is stored or sent.

2. Put it in an environment variable of the `angelia` process, name that variable in `secret_env`, and give
   the same secret to the receiver.

   ```yaml
   sinks:
     - name: ci-webhook
       kind: webhook
       config:
         url: https://ci.example.org/hooks/angelia
         secret_env: CI_WEBHOOK_SECRET
   ```

   The watch file, the database and the API hold only the name (`CI_WEBHOOK_SECRET`), never the value.
   A sink created through the API may name only a variable listed in
   [`api.allowed_env`](configuration.md).

   Or store the secret in the instance, encrypted at rest, and name it with `secret_ref` instead (at most
   one of the two keys):

   ```
   angelia secret set ci-webhook --from-file whsec.txt --host ci.example.org
   ```

   ```yaml
         url: https://ci.example.org/hooks/angelia
         secret_ref: ci-webhook
   ```

   A stored value signs exactly as a variable does, and is read from the store at every attempt, so a
   new `angelia secret set` takes effect without a restart. With `--host`, the secret is resolved only for
   a `url` on that host and port; since redirects are not followed, it is never sent anywhere else. See
   [Stored secrets](configuration.md#stored-secrets).

Rules for the value: `whsec_` followed by **padded standard base64** (`+`, `/`, `=`; not the URL-safe or
unpadded forms) of **24 to 64 bytes**. A key made of one repeated byte is refused as a placeholder.
Leading and trailing white space, such as the newline of a mounted file, is ignored.

A missing, empty or invalid value does not stop the instance: each delivery fails and is **retried**, with
`last_error` naming the variable (or the stored secret) and the rule, never the value, for example
`webhook: secret_env CI_WEBHOOK_SECRET: invalid secret (want 1-3 whsec_ secrets of 24-64 bytes, padded standard base64)`.
Fix the variable and restart, or store a new value with `angelia secret set`; the next retry succeeds.

Without `secret_env` or `secret_ref` the sink sends **unsigned** deliveries. `angelia config check --watches` and
`angelia serve --watches` warn about it, and the sink logs one warning per unsigned sink the first time
the dispatcher role delivers for it, so sinks created through the API are reported too.

### Rotating the secret

One value holds up to **three** secrets separated by spaces; each delivery carries one signature per
secret, in that order, so a receiver that knows any one of them accepts it.

1. Add the new secret first: `CI_WEBHOOK_SECRET="whsec_NEW whsec_OLD"`, then restart `angelia` (the
   environment is read when the process starts).
2. Give `whsec_NEW` to every receiver.
3. Retire the old one: `CI_WEBHOOK_SECRET="whsec_NEW"`, restart.

### Timeouts

A request is bounded by the sink's `timeout` (default `10s`) and by the dispatcher's deliver timeout
(15 s), which also covers rendering and any wait for the host's rate limit. A `timeout` above 15 s has
no effect; `config check` warns about it.

### Responses and retries

| Outcome | Result |
|---|---|
| 2xx | delivered |
| 408, 429, 5xx, other unexpected codes | retried; `Retry-After` is honoured |
| 3xx, other 4xx (401, 404, 410, …) | dead at once: fix the sink, then replay |
| connection refused or reset, DNS failure, TLS error, timeout | retried |
| destination refused by the SSRF guard, invalid URL | dead at once |
| secret unset or invalid | retried |

Redirects are **not followed**: update the URL instead. The response body is read up to 64 KiB, discarded
and never stored; `last_error` reads `webhook: https://host: HTTP 404 Not Found`, with the URL cut to
scheme and host. A request that got no response reads `webhook: https://host:` followed by one of
`connection refused`, `connection reset`, `timeout`, `DNS lookup failed`, `TLS handshake failed` or
`request failed`, never the address or port it dialled. A URL whose host is an IP address in a
non-canonical form (`2130706433`, `127.1`) is refused when the sink is stored.

A `429` or `503` with `Retry-After` pauses that host for every request of the instance, sources included.

### Private networks

Deliveries go through the same SSRF guard as sources. Loopback, private, link-local, CGNAT, multicast,
reserved and IPv4-embedding IPv6 addresses are refused unless their prefix is listed in the instance
configuration's [`http.allow_networks`](configuration.md#outgoing-http-and-private-networks); plain `http://`
URLs are accepted only for hosts in `http.allow_http_hosts`, checked when the sink is stored and again at
every delivery, so removing a host stops cleartext deliveries to it (dead at once). The watch file and the API cannot widen
either list. A refused destination is dead at once, with `last_error`
`webhook: destination not allowed (<host>)`; the resolved address is never shown. Allowlist it, then replay.

The guard checks the URL's addresses and, for a direct connection, the address actually dialled, so a
name that resolves differently the second time (DNS rebinding) is refused. **Through an HTTP proxy**
(`HTTPS_PROXY`, `HTTP_PROXY`) only the URL check applies: the proxy resolves and connects, so protecting
against rebinding is the proxy's job.

### Replay and deduplication

`webhook-id` is the delivery's idempotency key, `<event id>:<subscription id>`. It is the same on every
retry **and on every replay** of that delivery. A receiver that deduplicates on it, as Standard Webhooks
recommends, drops a replay of a delivery it already processed. Replay is meant for deliveries the
receiver never accepted (dead, or failed); to make a receiver process an event again, clear its record
of that `webhook-id` first.

## For receivers

Each delivery is an HTTP `POST`:

| Header | Value |
|---|---|
| `Content-Type` | `application/cloudevents+json; charset=utf-8` (or the template's type) |
| `webhook-id` | `<event ULID>:<subscription ULID>`, the same on every retry and replay |
| `webhook-timestamp` | Unix seconds of this attempt |
| `webhook-signature` | `v1,<base64 HMAC-SHA256>`, one entry per secret, separated by spaces |

The body is the CloudEvents event in structured mode (all attributes are in the JSON body; there are no
`ce-*` headers), or the subscription's rendered template. The signed content is
`<webhook-id>.<webhook-timestamp>.<body>` with the body's exact bytes.

- **Require a valid signature.** Reject any request without one. If an operator removes `secret_env`,
  deliveries silently become unsigned, and only a receiver that insists on a signature notices.
- **Verify with a Standard Webhooks library** (available for Go, Python, JavaScript, Java, Rust, Ruby,
  PHP, C#, Kotlin, Elixir): pass the raw body, before any JSON parsing, and the secret as given
  (`whsec_…`). Accept a timestamp at most 5 minutes from your clock.
- **Deduplicate on `webhook-id`**, and keep in mind the replay note above.
- Answer `2xx` quickly. Do not redirect: a `3xx` is a permanent failure.
